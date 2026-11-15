package collate

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/pdftest"
)

// widths returns the MediaBox width of every page; pdftest makes each page
// one point wider than the one before, so the widths identify the pages.
func widths(t *testing.T, pdf []byte) []int {
	t.Helper()
	w, err := pdftest.Widths(pdf)
	if err != nil {
		t.Fatalf("page widths: %v", err)
	}
	return w
}

func TestInterleave(t *testing.T) {
	// Fronts 100..102 (sheets 1-3), backs 200..202 scanned last sheet first.
	want := "[100 202 101 201 102 200]"
	for _, tc := range []struct {
		name          string
		fronts, backs pdftest.Options
	}{
		{"xref table", pdftest.Options{Pages: 3, Width: 100}, pdftest.Options{Pages: 3, Width: 200}},
		{"xref stream", pdftest.Options{Pages: 3, Width: 100, XRefStream: true}, pdftest.Options{Pages: 3, Width: 200, XRefStream: true}},
		{"mixed, compact", pdftest.Options{Pages: 3, Width: 100, Compact: true}, pdftest.Options{Pages: 3, Width: 200, XRefStream: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, pages, err := Interleave([][]byte{pdftest.New(tc.fronts)}, [][]byte{pdftest.New(tc.backs)})
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(widths(t, out)); got != want || pages != 6 {
				t.Errorf("pages = %d, order %s, want 6, %s", pages, got, want)
			}
			// The service validates what it delivers with ValidatePDF, which
			// needs a classic cross-reference table and plain page objects.
			if n, err := output.ValidatePDF(out); err != nil || n != 6 {
				t.Errorf("ValidatePDF = %d, %v", n, err)
			}
		})
	}
}

// A pass may arrive as several documents (one per page on some scanners).
func TestInterleaveSeveralDocumentsPerPass(t *testing.T) {
	doc := func(width int) []byte { return pdftest.New(pdftest.Options{Pages: 1, Width: width}) }
	out, pages, err := Interleave(
		[][]byte{doc(100), doc(101), pdftest.New(pdftest.Options{Pages: 2, Width: 102})},
		[][]byte{doc(200), pdftest.New(pdftest.Options{Pages: 2, Width: 201}), doc(203)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(widths(t, out)), "[100 203 101 202 102 201 103 200]"; got != want || pages != 8 {
		t.Errorf("pages = %d, order %s, want 8, %s", pages, got, want)
	}
}

// The scanned images must come out byte for byte as they went in.
func TestInterleaveKeepsImageData(t *testing.T) {
	fronts := pdftest.Options{Pages: 2, Width: 100, Image: []byte("\xff\xd8 fronts \xff\xd9")}
	backs := pdftest.Options{Pages: 2, Width: 200, Image: []byte("\xff\xd8 backs \xff\xd9"), XRefStream: true}
	out, _, err := Interleave([][]byte{pdftest.New(fronts)}, [][]byte{pdftest.New(backs)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(widths(t, out)), "[100 201 101 200]"; got != want {
		t.Errorf("order %s, want %s", got, want)
	}
	for _, o := range []pdftest.Options{fronts, backs} {
		for page := 1; page <= o.Pages; page++ {
			if n := bytes.Count(out, o.ImageData(page)); n != 1 {
				t.Errorf("image %q found %d times, want once", o.ImageData(page), n)
			}
		}
	}
	if n, err := output.ValidatePDF(out); err != nil || n != 4 {
		t.Errorf("ValidatePDF = %d, %v", n, err)
	}
}

func TestInterleavePageCountMismatch(t *testing.T) {
	_, _, err := Interleave(
		[][]byte{pdftest.New(pdftest.Options{Pages: 3})},
		[][]byte{pdftest.New(pdftest.Options{Pages: 2})})
	var mismatch *PageCountError
	if !errors.As(err, &mismatch) || mismatch.Fronts != 3 || mismatch.Backs != 2 {
		t.Fatalf("err = %v, want a PageCountError 3/2", err)
	}
}

func TestUnreadableDocument(t *testing.T) {
	good := pdftest.New(pdftest.Options{Pages: 1})
	bad := []byte("%PDF-1.4\nthis is not a PDF\n%%EOF\n")
	if _, err := Pages([][]byte{good, bad}); err == nil {
		t.Error("Pages accepted an unreadable document")
	}
	var mismatch *PageCountError
	if _, _, err := Interleave([][]byte{good}, [][]byte{bad}); err == nil || errors.As(err, &mismatch) {
		t.Errorf("Interleave: err = %v", err)
	}
	if _, _, err := Interleave(nil, [][]byte{good}); err == nil {
		t.Error("Interleave accepted a missing pass")
	}
}

func TestPages(t *testing.T) {
	n, err := Pages([][]byte{
		pdftest.New(pdftest.Options{Pages: 3}),
		pdftest.New(pdftest.Options{Pages: 2, XRefStream: true}),
	})
	if err != nil || n != 5 {
		t.Fatalf("Pages = %d, %v, want 5", n, err)
	}
}
