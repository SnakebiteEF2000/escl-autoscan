// Package pdftest generates tiny, structurally valid PDFs for tests.
package pdftest

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Options select the PDF shape.
type Options struct {
	Pages      int
	XRefStream bool // cross-reference stream (PDF 1.5) instead of xref table + trailer
	Compact    bool // write "/Type/Page" without a space, as some producers do
}

// New returns a PDF with the given number of blank A4 pages.
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

	// 1: catalog, 2: page tree, then one page and one content stream per page.
	obj(fmt.Sprintf("<< %s /Pages 2 0 R >>", typ("Catalog")))
	kids := ""
	for i := range o.Pages {
		kids += fmt.Sprintf("%d 0 R ", 3+2*i)
	}
	obj(fmt.Sprintf("<< %s /Kids [%s] /Count %d >>", typ("Pages"), kids, o.Pages))
	for i := range o.Pages {
		obj(fmt.Sprintf("<< %s /Parent 2 0 R /MediaBox [0 0 595 842] /Contents %d 0 R >>", typ("Page"), 4+2*i))
		content := fmt.Sprintf("%% page %d\n", i+1)
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
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
