// Package pdftest generates tiny, structurally valid PDFs for tests.
package pdftest

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// Options select the PDF shape.
type Options struct {
	Pages      int
	XRefStream bool // cross-reference stream (PDF 1.5) instead of xref table + trailer
	Compact    bool // write "/Type/Page" without a space, as some producers do
	// Width is the MediaBox width of the first page in points; every further
	// page is one point wider, so tests can tell pages apart after a merge.
	// All pages are 595 (A4) wide when zero.
	Width int
	// Image puts an image XObject (DCTDecode) on every page, as a scanner
	// does. Its data is Image followed by "page N", so it differs per page.
	Image []byte
}

// ImageData returns the stream data of the image on the given page (from 1).
func (o Options) ImageData(page int) []byte {
	return fmt.Appendf(bytes.Clone(o.Image), "page %d", page)
}

// Widths returns the MediaBox width of every page of pdf, in page order, as
// a full PDF parser (pdfcpu) sees them. With Options.Width they identify the
// pages.
func Widths(pdf []byte) ([]int, error) {
	dims, err := api.PageDims(context.Background(), bytes.NewReader(pdf), model.NewStatelessConfiguration())
	if err != nil {
		return nil, err
	}
	out := make([]int, len(dims))
	for i, d := range dims {
		out[i] = int(d.Width)
	}
	return out, nil
}

// New returns a PDF with the given number of A4 pages, blank unless
// Options.Image is set.
func New(o Options) []byte {
	var buf bytes.Buffer
	var offsets []int // offsets[i] is the offset of object i+1
	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	typ := func(t string) string {
		if o.Compact {
			return "/Type/" + t
		}
		return "/Type /" + t
	}

	version := "1.4"
	if o.XRefStream {
		version = "1.5"
	}
	fmt.Fprintf(&buf, "%%PDF-%s\n%%\xe2\xe3\xcf\xd3\n", version)

	// 1: catalog, 2: page tree, then per page: the page, its content stream
	// and, with Options.Image, its image.
	per := 2
	if o.Image != nil {
		per = 3
	}
	obj(fmt.Sprintf("<< %s /Pages 2 0 R >>", typ("Catalog")))
	kids := ""
	for i := range o.Pages {
		kids += fmt.Sprintf("%d 0 R ", 3+per*i)
	}
	obj(fmt.Sprintf("<< %s /Kids [%s] /Count %d >>", typ("Pages"), kids, o.Pages))
	for i := range o.Pages {
		width, n := 595, 3+per*i
		if o.Width > 0 {
			width = o.Width + i
		}
		if o.Image == nil {
			obj(fmt.Sprintf("<< %s /Parent 2 0 R /MediaBox [0 0 %d 842] /Contents %d 0 R >>", typ("Page"), width, n+1))
			content := fmt.Sprintf("%% page %d\n", i+1)
			obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
			continue
		}
		obj(fmt.Sprintf("<< %s /Parent 2 0 R /MediaBox [0 0 %d 842] /Contents %d 0 R /Resources << /XObject << /Im0 %d 0 R >> >> >>",
			typ("Page"), width, n+1, n+2))
		content := fmt.Sprintf("q %d 0 0 842 0 0 cm /Im0 Do Q\n", width)
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		data := o.ImageData(i + 1)
		obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream",
			len(data), data))
	}

	if !o.XRefStream {
		xref := buf.Len()
		fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
		for _, off := range offsets {
			fmt.Fprintf(&buf, "%010d 00000 n \n", off)
		}
		fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
		return buf.Bytes()
	}

	// Cross-reference stream: entries of 1+4+2 bytes, including itself.
	xref := buf.Len()
	n := len(offsets) + 2
	var entries bytes.Buffer
	entries.Write([]byte{0, 0, 0, 0, 0, 0xff, 0xff})
	for _, off := range append(offsets, xref) {
		entries.WriteByte(1)
		_ = binary.Write(&entries, binary.BigEndian, uint32(off))
		entries.Write([]byte{0, 0})
	}
	fmt.Fprintf(&buf, "%d 0 obj\n<< %s /Size %d /W [1 4 2] /Root 1 0 R /Length %d >>\nstream\n",
		n-1, typ("XRef"), n, entries.Len())
	buf.Write(entries.Bytes())
	fmt.Fprintf(&buf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xref)
	return buf.Bytes()
}
