package autoscan

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escltest"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/pdftest"
)

// A flatbed document: pages scanned one by one on the glass, saved as one
// PDF in the order they were scanned.

func (h *harness) startFlatbed() {
	h.t.Helper()
	if err := h.svc.StartDoubleSided(escl.Platen); err != nil {
		h.t.Fatal(err)
	}
}

// held reports that the flatbed document is open with the given pages.
func (h *harness) held(pages int) bool {
	s := h.svc.Snapshot()
	return s.DoubleSided == DoubleSidedFlatbed && s.DoubleSidedHeld == pages
}

func TestFlatbedDocument(t *testing.T) {
	h, _ := dsHarness(t)
	h.startFlatbed()
	if !h.held(0) || h.metric("escl_autoscan_double_sided_state") != 3 {
		t.Fatalf("after start: %+v", h.svc.Snapshot())
	}

	for i, width := range []int{100, 200, 300} {
		rec := h.pass(escl.Platen, stack(1, width))
		if rec.Result != metrics.ResultOK || !rec.DoubleSided || len(rec.Files) != 0 || !h.held(i+1) || len(pdfs(t, h.out)) != 0 {
			t.Fatalf("page %d: %+v, snapshot %+v, output %v", i+1, rec, h.svc.Snapshot(), pdfs(t, h.out))
		}
	}

	if err := h.svc.FinishDoubleSided(); err != nil {
		t.Fatal(err)
	}
	if got, want := pageWidths(t, h.out), "[100 200 300]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	s := h.svc.Snapshot()
	name := pdfs(t, h.out)[0]
	if s.DoubleSided != DoubleSidedOff || s.DoubleSidedHeld != 0 || s.DoubleSidedNote.Kind != NoteOK ||
		!strings.Contains(s.DoubleSidedNote.Text, name) || !strings.Contains(s.DoubleSidedNote.Text, "3 pages") ||
		h.dsTotal(metrics.DoubleSidedDelivered) != 1 {
		t.Fatalf("after saving: %+v", s)
	}
	// The saved file is in the history, but it is not a scan: three scans
	// and three pages are counted, not four and six.
	if top := s.History[0]; len(s.History) != 4 || len(top.Files) != 1 || top.Files[0] != name || top.Pages != 3 || top.Source != "platen" {
		t.Errorf("history: %+v", s.History)
	}
	if h.scans("platen", metrics.ResultOK) != 3 || h.metric("escl_autoscan_pages_total") != 3 {
		t.Errorf("scans = %v, pages = %v, want 3 and 3", h.scans("platen", metrics.ResultOK), h.metric("escl_autoscan_pages_total"))
	}

	// The next flatbed scan is a normal one again.
	if rec := h.pass(escl.Platen, stack(1, 400)); rec.DoubleSided || len(rec.Files) != 1 || len(pdfs(t, h.out)) != 2 {
		t.Fatalf("scan after the document: %+v", rec)
	}
}

// One page needs no joining: it is delivered exactly as scanned.
func TestFlatbedDocumentOfOnePage(t *testing.T) {
	h, _ := dsHarness(t)
	h.startFlatbed()
	page := stack(1, 100)
	h.pass(escl.Platen, page)
	if err := h.svc.FinishDoubleSided(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(h.out, pdfs(t, h.out)[0]))
	if err != nil || !bytes.Equal(got, page.Body) {
		t.Errorf("the page was rewritten (%v)", err)
	}

	// Saving without a page just ends the run.
	h.startFlatbed()
	if err := h.svc.FinishDoubleSided(); err != nil || h.svc.Snapshot().DoubleSided != DoubleSidedOff || len(pdfs(t, h.out)) != 1 {
		t.Errorf("saving an empty document: %v, %+v", err, h.svc.Snapshot())
	}
}

// Autoscan stays live: a feeder scan while a flatbed document is open is
// delivered as usual and leaves the held pages alone.
func TestFlatbedDocumentIgnoresFeederScans(t *testing.T) {
	h, _ := dsHarness(t)
	h.startFlatbed()
	h.pass(escl.Platen, stack(1, 100))

	letter := h.pass(escl.Feeder, stack(2, 500))
	if letter.DoubleSided || len(letter.Files) != 1 || !h.held(1) {
		t.Fatalf("feeder scan: %+v, snapshot %+v", letter, h.svc.Snapshot())
	}
	if got, want := widthsOf(t, h.out, letter.Files[0]), "[500 501]"; got != want {
		t.Errorf("feeder scan = %s, want %s", got, want)
	}

	h.pass(escl.Platen, stack(1, 200))
	if err := h.svc.FinishDoubleSided(); err != nil {
		t.Fatal(err)
	}
	for _, name := range pdfs(t, h.out) {
		if name != letter.Files[0] {
			if got, want := widthsOf(t, h.out, name), "[100 200]"; got != want {
				t.Errorf("flatbed document = %s, want %s", got, want)
			}
		}
	}
}

// One run at a time, and none of them changes while its scan runs.
func TestFlatbedDocumentAndFeederRunExcludeEachOther(t *testing.T) {
	h, _ := dsHarness(t)
	h.startFlatbed()
	if err := h.svc.StartDoubleSided(escl.Feeder); !errors.Is(err, ErrDoubleSidedActive) {
		t.Errorf("feeder run during a flatbed document: err = %v", err)
	}
	if err := h.svc.StartDoubleSided(escl.Platen); err != nil {
		t.Errorf("starting twice: %v", err)
	}

	slow := stack(1, 100)
	slow.Delay = 150 * time.Millisecond
	h.fake.QueueJob(slow)
	if err := h.svc.Scan(escl.Platen, ""); err != nil {
		t.Fatal(err)
	}
	if cur := h.svc.Snapshot().Scanning; cur == nil || cur.Pass != DoubleSidedFlatbed {
		t.Fatalf("running pass = %+v", cur)
	}
	if err := h.svc.FinishDoubleSided(); !errors.Is(err, ErrBusy) {
		t.Errorf("saving while a page is scanned: err = %v", err)
	}
	if err := h.svc.CancelDoubleSided(); !errors.Is(err, ErrBusy) {
		t.Errorf("cancelling while a page is scanned: err = %v", err)
	}
	h.waitRecords(1)
	h.eventually("page held", func() bool { return h.held(1) && h.svc.Snapshot().Scanning == nil })

	if err := h.svc.CancelDoubleSided(); err != nil {
		t.Fatal(err)
	}
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedOff || !strings.Contains(s.DoubleSidedNote.Text, "1 page held were discarded") || len(pdfs(t, h.out)) != 0 {
		t.Fatalf("after cancel: %+v, output %v", s, pdfs(t, h.out))
	}

	h.start()
	if err := h.svc.StartDoubleSided(escl.Platen); !errors.Is(err, ErrDoubleSidedActive) {
		t.Errorf("flatbed document during a feeder run: err = %v", err)
	}
	// Saving is for flatbed documents; in a feeder run it does nothing.
	h.pass(escl.Feeder, stack(1, 100))
	if err := h.svc.FinishDoubleSided(); err != nil || h.svc.Snapshot().DoubleSided != DoubleSidedBacks {
		t.Errorf("saving in a feeder run: %v, %+v", err, h.svc.Snapshot())
	}
}

// A failed scan keeps the pages, and so does an output folder that cannot be
// written.
func TestFlatbedDocumentKeepsPages(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	h, _ := dsHarness(t)
	t.Cleanup(func() { _ = os.Chmod(h.out, 0o700) })
	h.startFlatbed()
	h.pass(escl.Platen, stack(1, 100))

	if rec := h.pass(escl.Platen, escltest.Step{Code: 410}); rec.Result != metrics.ResultAborted {
		t.Fatalf("failed scan: %+v", rec)
	}
	if s := h.svc.Snapshot(); !h.held(1) || !strings.Contains(s.DoubleSidedNote.Text, "1 page scanned before are still held") {
		t.Fatalf("after the failed scan: %+v", s)
	}
	h.pass(escl.Platen, stack(1, 200))

	if err := os.Chmod(h.out, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.FinishDoubleSided(); err == nil {
		t.Fatal("saving into a read-only folder: no error")
	}
	if s := h.svc.Snapshot(); !h.held(2) || !strings.Contains(s.DoubleSidedNote.Text, "could not be saved") || h.dsTotal(metrics.DoubleSidedDelivered) != 0 {
		t.Fatalf("after the failed save: %+v", s)
	}

	if err := os.Chmod(h.out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.FinishDoubleSided(); err != nil {
		t.Fatal(err)
	}
	if got, want := pageWidths(t, h.out), "[100 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
}

// Pages that pdfcpu cannot join are saved as they were scanned.
func TestFlatbedDocumentThatCannotBeJoined(t *testing.T) {
	h, _ := dsHarness(t)
	h.startFlatbed()
	h.pass(escl.Platen, stack(1, 100))
	h.pass(escl.Platen, unreadable())
	if err := h.svc.FinishDoubleSided(); err != nil {
		t.Fatal(err)
	}
	s := h.svc.Snapshot()
	if len(pdfs(t, h.out)) != 2 || s.DoubleSided != DoubleSidedOff || s.DoubleSidedNote.Kind != NoteWarn ||
		!strings.Contains(s.DoubleSidedNote.Text, "separate") || h.dsTotal(metrics.DoubleSidedFailed) != 1 {
		t.Fatalf("snapshot %+v, output %v", s, pdfs(t, h.out))
	}
}

// A timeout and a shutdown save the pages as one document.
func TestFlatbedDocumentTimeoutAndShutdown(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		h, offset := dsHarness(t)
		h.startFlatbed()
		h.pass(escl.Platen, stack(1, 100))
		offset.Add(int64(20 * time.Minute))
		h.pass(escl.Platen, stack(1, 200)) // every page gets the full time again
		offset.Add(int64(20 * time.Minute))
		h.stays("document open 20 minutes after the last page", func() bool { return h.held(2) })
		offset.Add(int64(11 * time.Minute))
		h.eventually("document saved", func() bool { return len(pdfs(t, h.out)) == 1 })
		if got, want := pageWidths(t, h.out), "[100 200]"; got != want {
			t.Errorf("page order = %s, want %s", got, want)
		}
		h.eventually("note", func() bool {
			s := h.svc.Snapshot()
			return s.DoubleSided == DoubleSidedOff && strings.Contains(s.DoubleSidedNote.Text, "timed out") && strings.Contains(s.DoubleSidedNote.Text, pdfs(t, h.out)[0])
		})
		if h.dsTotal(metrics.DoubleSidedTimeout) != 1 {
			t.Error("timeout not counted")
		}
	})
	t.Run("timeout without a page", func(t *testing.T) {
		h, offset := dsHarness(t)
		h.startFlatbed()
		offset.Add(int64(31 * time.Minute))
		h.eventually("off", func() bool { return h.svc.Snapshot().DoubleSided == DoubleSidedOff })
		if len(pdfs(t, h.out)) != 0 {
			t.Errorf("output %v", pdfs(t, h.out))
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		h, _ := dsHarness(t)
		h.startFlatbed()
		h.pass(escl.Platen, stack(1, 100))
		h.pass(escl.Platen, escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 1, Width: 200, XRefStream: true})})
		h.svc.Shutdown(time.Second)
		if got, want := pageWidths(t, h.out), "[100 200]"; got != want {
			t.Errorf("page order = %s, want %s", got, want)
		}
	})
}

// One document, one look: within a run the next scan uses the color mode of
// the scans before, unless another one is chosen. The UI preselects it.
func TestDoubleSidedRunKeepsTheColorMode(t *testing.T) {
	lastJob := func(h *harness) string {
		jobs := h.fake.Jobs()
		return jobs[len(jobs)-1].Settings
	}
	scan := func(h *harness, source escl.InputSource, color string, step escltest.Step) {
		t.Helper()
		n := len(h.history())
		h.fake.QueueJob(step)
		if err := h.svc.Scan(source, color); err != nil {
			t.Fatal(err)
		}
		h.waitRecords(n + 1)
		h.eventually("idle", func() bool { return h.svc.Snapshot().Scanning == nil })
	}

	t.Run("flatbed", func(t *testing.T) {
		h, _ := dsHarness(t)
		h.startFlatbed()
		scan(h, escl.Platen, "RGB24", stack(1, 100))
		if got := h.svc.Snapshot().ColorMode; got != "RGB24" {
			t.Errorf("preselected mode = %s, want RGB24", got)
		}
		scan(h, escl.Platen, "", stack(1, 200))
		if !strings.Contains(lastJob(h), "<scan:ColorMode>RGB24<") {
			t.Errorf("second page: %s", lastJob(h))
		}
		// A feeder scan in between is not part of the document.
		scan(h, escl.Feeder, "", stack(1, 300))
		if !strings.Contains(lastJob(h), "<scan:ColorMode>Grayscale8<") {
			t.Errorf("feeder scan: %s", lastJob(h))
		}
		if err := h.svc.FinishDoubleSided(); err != nil {
			t.Fatal(err)
		}
		if got := h.svc.Snapshot().ColorMode; got != "Grayscale8" {
			t.Errorf("preselected mode after the document = %s, want the default", got)
		}
	})
	t.Run("feeder", func(t *testing.T) {
		h, _ := dsHarness(t)
		h.start()
		scan(h, escl.Feeder, "RGB24", stack(2, 100))
		scan(h, escl.Feeder, "", stack(2, 200)) // as autoscan starts it
		if !strings.Contains(lastJob(h), "<scan:ColorMode>RGB24<") || h.dsTotal(metrics.DoubleSidedDelivered) != 1 {
			t.Errorf("backs: %s", lastJob(h))
		}
	})
}
