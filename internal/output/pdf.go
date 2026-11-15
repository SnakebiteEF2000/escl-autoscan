// Package output validates scanned PDFs and delivers them atomically.
package output

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrIncomplete is wrapped by every validation error.
var ErrIncomplete = errors.New("incomplete PDF")

var (
	// "/Type /Page" or "/Type/Page", but not "/Type /Pages".
	pageRe = regexp.MustCompile(`/Type\s*/Page[^A-Za-z0-9_]`)
	// Classic "xref" table or an indirect object "N G obj" (xref stream).
	objRe     = regexp.MustCompile(`^\d+\s+\d+\s+obj\b`)
	xrefTypRe = regexp.MustCompile(`/Type\s*/XRef\b`)
	startRe   = regexp.MustCompile(`startxref\s+(\d+)\s+%%EOF`)
)

// tailWindow is how far from the end %%EOF may be (the PDF spec says
// "within the last 1024 bytes").
const tailWindow = 1024

// ValidatePDF checks that a PDF is structurally complete and returns its page
// count. It does not parse the PDF; it looks for the markers a writer emits
// last, which a truncated transfer lacks. The WF-2960 returned a 200 with a
// truncated PDF (no xref, no trailer) after a paper jam.
//
// Page objects inside compressed object streams are not counted; a PDF whose
// pages all live in object streams is reported as having no pages.
func ValidatePDF(data []byte) (pages int, err error) {
	fail := func(format string, args ...any) (int, error) {
		return 0, fmt.Errorf("%w: %s", ErrIncomplete, fmt.Sprintf(format, args...))
	}
	if len(data) == 0 {
		return fail("empty document")
	}
	if i := bytes.Index(data[:min(len(data), 1024)], []byte("%PDF-")); i < 0 {
		return fail("no %%PDF- header")
	}
	tail := data[max(0, len(data)-tailWindow):]
	if !bytes.Contains(tail, []byte("%%EOF")) {
		return fail("no %%%%EOF near the end")
	}
	// The last "startxref <offset> %%EOF" in the file wins (incremental updates).
	matches := startRe.FindAllSubmatchIndex(tail, -1)
	if matches == nil {
		return fail("no startxref before %%%%EOF")
	}
	m := matches[len(matches)-1]
	off, err := strconv.ParseInt(string(tail[m[2]:m[3]]), 10, 64)
	if err != nil || off <= 0 || off >= int64(len(data)) {
		return fail("startxref offset %s outside the file", tail[m[2]:m[3]])
	}
	at := data[off:]
	switch {
	case bytes.HasPrefix(at, []byte("xref")):
		if !bytes.Contains(at, []byte("trailer")) {
			return fail("xref table without trailer")
		}
	case objRe.Match(at):
		end := bytes.Index(at, []byte("stream"))
		if end < 0 || !xrefTypRe.Match(at[:end]) {
			return fail("startxref does not point to a cross-reference stream")
		}
	default:
		return fail("startxref does not point to a cross-reference section")
	}
	pages = len(pageRe.FindAllIndex(data, -1))
	if pages == 0 {
		return fail("no pages")
	}
	return pages, nil
}
