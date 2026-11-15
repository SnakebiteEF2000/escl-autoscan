// Package pdftest generates tiny, structurally valid PDFs for tests.
package pdftest

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"

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
	// Inherit, with Image, puts the MediaBox and the image resources on the
	// page tree node instead of on every page, as scanners often do. All
	// pages are then Width wide.
	Inherit bool
	// Labels prints one line of text on each page, for PDFs that people
	// look at (the scanner simulator). It sets the number of pages.
	Labels []string
}

// ImageData returns the stream data of the image on the given page (from 1).
func (o Options) ImageData(page int) []byte {
	return fmt.Appendf(bytes.Clone(o.Image), "page %d", page)
}

// pdfString escapes text for a PDF literal string.
var pdfString = strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)

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

	// 1: catalog, 2: page tree, with Options.Labels 3: the font, then per
	// page: the page, its content stream and, with Options.Image, its image.
	first, per := 3, 2
	if o.Labels != nil {
		o.Pages, first = len(o.Labels), 4
	} else if o.Image != nil {
		per = 3
	}
	obj(fmt.Sprintf("<< %s /Pages 2 0 R >>", typ("Catalog")))
	kids := ""
	for i := range o.Pages {
		kids += fmt.Sprintf("%d 0 R ", first+per*i)
	}
	inherited := ""
	if o.Inherit && o.Image != nil {
		images := ""
		for i := range o.Pages {
			images += fmt.Sprintf("/Im%d %d 0 R ", i, first+per*i+2)
		}
		inherited = fmt.Sprintf(" /MediaBox [0 0 %d 842] /Resources << /XObject << %s>> >>", max(o.Width, 1), images)
	}
	obj(fmt.Sprintf("<< %s /Kids [%s] /Count %d%s >>", typ("Pages"), kids, o.Pages, inherited))
	if o.Labels != nil {
		obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>")
	}
	for i := range o.Pages {
		width, n := 595, first+per*i
		if o.Width > 0 {
			width = o.Width + i
		}
		switch {
		case o.Labels != nil:
			obj(fmt.Sprintf("<< %s /Parent 2 0 R /MediaBox [0 0 %d 842] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
				typ("Page"), width, n+1))
			content := fmt.Sprintf("BT /F1 44 Tf 60 720 Td (%s) Tj ET\n", pdfString.Replace(o.Labels[i]))
			obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		case o.Image != nil:
			name := "Im0"
			if inherited != "" {
				name = fmt.Sprintf("Im%d", i)
				obj(fmt.Sprintf("<< %s /Parent 2 0 R /Contents %d 0 R >>", typ("Page"), n+1))
			} else {
				obj(fmt.Sprintf("<< %s /Parent 2 0 R /MediaBox [0 0 %d 842] /Contents %d 0 R /Resources << /XObject << /Im0 %d 0 R >> >> >>",
					typ("Page"), width, n+1, n+2))
			}
			content := fmt.Sprintf("q %d 0 0 842 0 0 cm /%s Do Q\n", width, name)
			obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
			data := o.ImageData(i + 1)
			obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width 8 /Height 8 /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream",
				len(data), data))
		default:
			obj(fmt.Sprintf("<< %s /Parent 2 0 R /MediaBox [0 0 %d 842] /Contents %d 0 R >>", typ("Page"), width, n+1))
			content := fmt.Sprintf("%% page %d\n", i+1)
			obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
		}
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
