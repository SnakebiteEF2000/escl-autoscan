package collate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"

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

// Scanners often put the page size and the images on the page tree node,
// from where the pages inherit them. They must not get lost or mixed up when
// the pages of two documents are put into a new tree.
func TestInterleaveInheritedPageAttributes(t *testing.T) {
	for _, xrefStream := range []bool{false, true} {
		fronts := pdftest.Options{Pages: 3, Width: 100, Image: []byte("\xff\xd8 fronts \xff\xd9"), Inherit: true, XRefStream: xrefStream}
		backs := pdftest.Options{Pages: 3, Width: 200, Image: []byte("\xff\xd8 backs \xff\xd9"), Inherit: true, XRefStream: xrefStream}
		out, pages, err := Interleave([][]byte{pdftest.New(fronts)}, [][]byte{pdftest.New(backs)})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := fmt.Sprint(widths(t, out)), "[100 200 100 200 100 200]"; got != want || pages != 6 {
			t.Errorf("pages = %d, widths %s, want 6, %s", pages, got, want)
		}
		// Page order by the image each page draws: fronts 1-3 with backs 3-1.
		if got, want := fmt.Sprint(drawnImages(t, out)), fmt.Sprint([]string{
			string(fronts.ImageData(1)), string(backs.ImageData(3)), string(fronts.ImageData(2)),
			string(backs.ImageData(2)), string(fronts.ImageData(3)), string(backs.ImageData(1))}); got != want {
			t.Errorf("images in page order:\n got %q\nwant %q", got, want)
		}
		if n, err := output.ValidatePDF(out); err != nil || n != 6 {
			t.Errorf("ValidatePDF = %d, %v", n, err)
		}
	}
}

// drawnImages returns, for every page in order, the data of the image that
// the page's content stream draws, resolved through the page's resources as
// a PDF viewer would.
func drawnImages(t *testing.T, pdf []byte) []string {
	t.Helper()
	c := context.Background()
	ctx, err := api.ReadAndValidate(c, bytes.NewReader(pdf), config())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for page := 1; page <= ctx.PageCount; page++ {
		d, _, inh, err := ctx.PageDict(c, page, true)
		if err != nil {
			t.Fatal(err)
		}
		content, err := ctx.PageContent(d, page)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`/(Im\d+) Do`).FindSubmatch(content)
		if m == nil {
			t.Fatalf("page %d draws no image: %q", page, content)
		}
		res := inh.Resources
		if obj, ok := d.Find("Resources"); ok {
			if res, err = ctx.DereferenceDict(obj); err != nil {
				t.Fatal(err)
			}
		}
		xobjects, err := ctx.DereferenceDict(res["XObject"])
		if err != nil || xobjects == nil {
			t.Fatalf("page %d has no XObject resources (%v)", page, err)
		}
		sd, _, err := ctx.DereferenceStreamDict(xobjects[string(m[1])])
		if err != nil || sd == nil {
			t.Fatalf("page %d: image %s not found (%v)", page, m[1], err)
		}
		out = append(out, string(sd.Raw))
	}
	return out
}

// testdata/pdfcpu-panic.pdf was found by fuzzing: it passes ValidatePDF and
// Pages, and then pdfcpu v0.16.4 fails while its pages are put into a new
// page tree, in about five of six runs by dereferencing nil. A panic must
// come back as an error; in the scan goroutine it would end the process.
// Without the recover in Interleave this test crashes.
func TestInterleaveSurvivesAPdfcpuPanic(t *testing.T) {
	bad, err := os.ReadFile("testdata/pdfcpu-panic.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.ValidatePDF(bad); err != nil {
		t.Fatalf("the fixture no longer passes ValidatePDF: %v", err)
	}
	n, err := Pages([][]byte{bad})
	if err != nil {
		t.Skipf("pdfcpu now refuses the fixture when counting pages: %v", err)
	}
	good := pdftest.New(pdftest.Options{Pages: n})
	panics := 0
	for range 20 {
		for _, pass := range [][2][]byte{{bad, good}, {good, bad}} {
			if _, _, err := Interleave([][]byte{pass[0]}, [][]byte{pass[1]}); err != nil && strings.Contains(err.Error(), "panicked") {
				panics++
			}
		}
	}
	if panics == 0 {
		t.Log("pdfcpu no longer panics on the fixture; the recover is not exercised by this test any more")
	}
}

// Join keeps the order of the scans: pages of a flatbed document.
func TestJoin(t *testing.T) {
	image := pdftest.Options{Pages: 2, Width: 300, Image: []byte("\xff\xd8 glass \xff\xd9"), Inherit: true}
	out, pages, err := Join([][]byte{
		pdftest.New(pdftest.Options{Pages: 1, Width: 100}),
		pdftest.New(pdftest.Options{Pages: 1, Width: 200, XRefStream: true}),
		pdftest.New(image),
		pdftest.New(pdftest.Options{Pages: 1, Width: 400, Compact: true}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(widths(t, out)), "[100 200 300 300 400]"; got != want || pages != 5 {
		t.Errorf("pages = %d, order %s, want 5, %s", pages, got, want)
	}
	for page := 1; page <= 2; page++ {
		if n := bytes.Count(out, image.ImageData(page)); n != 1 {
			t.Errorf("image %q found %d times, want once", image.ImageData(page), n)
		}
	}
	if n, err := output.ValidatePDF(out); err != nil || n != 5 {
		t.Errorf("ValidatePDF = %d, %v", n, err)
	}
	if _, _, err := Join(nil); err == nil {
		t.Error("Join accepted no documents")
	}
	if _, _, err := Join([][]byte{pdftest.New(pdftest.Options{Pages: 1}), []byte("%PDF-1.4\nnot a PDF\n%%EOF\n")}); err == nil {
		t.Error("Join accepted an unreadable document")
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
