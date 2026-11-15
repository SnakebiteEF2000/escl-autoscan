package autoscan

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escltest"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/pdftest"
)

// dsHarness has autoscan off (passes are started manually, as with autoscan
// paused) and a clock that tests can advance.
func dsHarness(t *testing.T) (*harness, *atomic.Int64) {
	t.Helper()
	offset := &atomic.Int64{}
	h := newHarness(t, func(c *Config) {
		c.Enabled = false
		c.DoubleSidedTimeout = 30 * time.Minute
		c.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	}, nil)
	h.eventually("reachable", func() bool { return h.svc.Snapshot().Reachable })
	return h, offset
}

// stack is a scan of the given number of pages. The pages are width,
// width+1, ... points wide, which identifies them after collating.
func stack(pages, width int) escltest.Step {
	return escltest.Step{Body: pdftest.New(pdftest.Options{Pages: pages, Width: width})}
}

// jam is a feeder job that jams: a truncated PDF, then 410.
func jam() []escltest.Step {
	pdf := pdftest.New(pdftest.Options{Pages: 2})
	return []escltest.Step{
		{Body: pdf[:bytes.LastIndex(pdf, []byte("xref"))], Before: func(s *escltest.Server) { s.SetStatus("Stopped", "ScannerAdfJam") }},
		{Code: 410},
	}
}

// pass scans the given job from source and waits for its record.
func (h *harness) pass(source escl.InputSource, steps ...escltest.Step) Record {
	h.t.Helper()
	n := len(h.history())
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	h.fake.QueueJob(steps...)
	if err := h.svc.Scan(source, ""); err != nil {
		h.t.Fatal(err)
	}
	return h.waitRecords(n + 1)[0]
}

func (h *harness) start() {
	h.t.Helper()
	if err := h.svc.StartDoubleSided(true); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) dsTotal(result string) float64 {
	return h.metric("escl_autoscan_double_sided_total", "result", result)
}

func pdfs(t *testing.T, dir string) []string {
	var out []string
	for _, n := range files(t, dir) {
		if strings.HasSuffix(n, ".pdf") {
			out = append(out, n)
		}
	}
	return out
}

// pageWidths returns the page widths of the only PDF in dir.
func pageWidths(t *testing.T, dir string) string {
	t.Helper()
	names := pdfs(t, dir)
	if len(names) != 1 {
		t.Fatalf("files in %s = %v, want one PDF", dir, names)
	}
	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	w, err := pdftest.Widths(data)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(w)
}

func TestDoubleSidedPair(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedFronts || h.metric("escl_autoscan_double_sided_state") != 1 {
		t.Fatalf("after start: %+v", s)
	}

	fronts := h.pass(escl.Feeder, stack(3, 100))
	s := h.svc.Snapshot()
	if fronts.Result != metrics.ResultOK || !fronts.DoubleSided || len(fronts.Files) != 0 || fronts.Pages != 3 ||
		s.DoubleSided != DoubleSidedBacks || s.DoubleSidedFronts != 3 || len(pdfs(t, h.out)) != 0 {
		t.Fatalf("fronts: %+v, snapshot %+v, output %v", fronts, s, pdfs(t, h.out))
	}

	// The stack is turned over, so the backs arrive last sheet first.
	backs := h.pass(escl.Feeder, stack(3, 200))
	if backs.Result != metrics.ResultOK || !backs.DoubleSided || len(backs.Files) != 1 || !strings.Contains(backs.Message, "6 pages") {
		t.Fatalf("backs: %+v", backs)
	}
	if got, want := pageWidths(t, h.out), "[100 202 101 201 102 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	s = h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedOff || s.DoubleSidedFronts != 0 || s.DoubleSidedNote.Kind != NoteOK ||
		!strings.Contains(s.DoubleSidedNote.Text, backs.Files[0]) ||
		h.dsTotal(metrics.DoubleSidedDelivered) != 1 || h.metric("escl_autoscan_double_sided_state") != 0 {
		t.Fatalf("after pair: %+v", s)
	}
	if h.metric("escl_autoscan_pages_total") != 6 {
		t.Errorf("pages_total = %v, want 6", h.metric("escl_autoscan_pages_total"))
	}

	// The next feeder scan is a normal one again, and clears the note.
	if rec := h.pass(escl.Feeder, stack(1, 300)); rec.DoubleSided || len(pdfs(t, h.out)) != 2 || h.svc.Snapshot().DoubleSidedNote.Text != "" {
		t.Fatalf("third scan: %+v, note %+v", rec, h.svc.Snapshot().DoubleSidedNote)
	}
}

func TestDoubleSidedFlatbedDoesNotTakePart(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(2, 100))
	if rec := h.pass(escl.Platen, stack(1, 300)); rec.DoubleSided || len(rec.Files) != 1 || len(pdfs(t, h.out)) != 1 ||
		h.svc.Snapshot().DoubleSided != DoubleSidedBacks {
		t.Fatalf("platen: %+v", rec)
	}
}

// A pass may arrive as several documents (one per page on some scanners).
func TestDoubleSidedSeveralDocumentsPerPass(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(1, 100), stack(1, 101))
	h.pass(escl.Feeder, stack(1, 200), stack(1, 201))
	if got, want := pageWidths(t, h.out), "[100 201 101 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
}

func TestDoubleSidedFailedPassCanBeRepeated(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()

	if rec := h.pass(escl.Feeder, jam()...); rec.Result != metrics.ResultAborted {
		t.Fatalf("jammed fronts: %+v", rec)
	}
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedFronts || s.DoubleSidedNote.Kind != NoteWarn || !strings.Contains(s.DoubleSidedNote.Text, "fronts again") {
		t.Fatalf("after jammed fronts: %+v", s)
	}
	h.pass(escl.Feeder, stack(2, 100))

	if rec := h.pass(escl.Feeder, jam()...); rec.Result != metrics.ResultAborted || rec.DoubleSided {
		t.Fatalf("jammed backs: %+v", rec)
	}
	s := h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedBacks || s.DoubleSidedFronts != 2 || !strings.Contains(s.DoubleSidedNote.Text, "still held") || len(pdfs(t, h.out)) != 0 {
		t.Fatalf("after jammed backs: %+v, output %v", s, pdfs(t, h.out))
	}

	h.pass(escl.Feeder, stack(2, 200))
	if got, want := pageWidths(t, h.out), "[100 201 101 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	if h.dsTotal(metrics.DoubleSidedDelivered) != 1 || h.dsTotal(metrics.DoubleSidedFailed) != 0 {
		t.Error("double_sided_total: want one delivered run and no failed one")
	}
}

func TestDoubleSidedPageCountMismatch(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(3, 100))

	rec := h.pass(escl.Feeder, stack(2, 200))
	if rec.Result != metrics.ResultError || !strings.Contains(rec.Message, "3 front pages but 2 back pages") || rec.DoubleSided {
		t.Fatalf("short backs: %+v", rec)
	}
	s := h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedBacks || s.DoubleSidedFronts != 3 || !strings.Contains(s.DoubleSidedNote.Text, "3 pages but the backs have 2 pages") {
		t.Fatalf("after short backs: %+v", s)
	}
	// The mismatched backs are kept for inspection, never delivered.
	if out, failed := pdfs(t, h.out), pdfs(t, h.failed); len(out) != 0 || len(failed) != 1 || !strings.HasSuffix(failed[0], "-error.pdf") {
		t.Fatalf("output %v, failed %v", out, failed)
	}

	h.pass(escl.Feeder, stack(3, 200))
	if got, want := pageWidths(t, h.out), "[100 202 101 201 102 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
}

func TestDoubleSidedTimeoutBeforeFronts(t *testing.T) {
	h, offset := dsHarness(t)
	h.start()
	offset.Add(int64(31 * time.Minute))
	h.eventually("timeout", func() bool { return h.svc.Snapshot().DoubleSided == DoubleSidedOff })
	if s := h.svc.Snapshot(); s.DoubleSidedNote.Kind != NoteWarn || !strings.Contains(s.DoubleSidedNote.Text, "timed out") || h.dsTotal(metrics.DoubleSidedTimeout) != 1 {
		t.Fatalf("after timeout: %+v", s)
	}
	if rec := h.pass(escl.Feeder, stack(1, 100)); rec.DoubleSided || len(pdfs(t, h.out)) != 1 {
		t.Fatalf("scan after timeout: %+v", rec)
	}
}

// Held fronts are never thrown away silently: a timeout saves them as a
// normal single-sided scan.
func TestDoubleSidedTimeoutSavesFronts(t *testing.T) {
	h, offset := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(3, 100))
	offset.Add(int64(31 * time.Minute))
	h.eventually("fronts saved", func() bool { return len(pdfs(t, h.out)) == 1 })
	if got, want := pageWidths(t, h.out), "[100 101 102]"; got != want {
		t.Errorf("saved fronts = %s, want %s", got, want)
	}
	h.eventually("note with the file name", func() bool {
		return strings.Contains(h.svc.Snapshot().DoubleSidedNote.Text, pdfs(t, h.out)[0])
	})
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedOff || s.DoubleSidedFronts != 0 || h.dsTotal(metrics.DoubleSidedTimeout) != 1 {
		t.Fatalf("after timeout: %+v", s)
	}
}

func TestDoubleSidedCancelDiscardsFronts(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(3, 100))
	if err := h.svc.StartDoubleSided(false); err != nil {
		t.Fatal(err)
	}
	s := h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedOff || s.DoubleSidedFronts != 0 || s.DoubleSidedNote.Kind != NoteInfo ||
		!strings.Contains(s.DoubleSidedNote.Text, "3 fronts held were discarded") || h.dsTotal(metrics.DoubleSidedCancelled) != 1 {
		t.Fatalf("after cancel: %+v", s)
	}
	if rec := h.pass(escl.Feeder, stack(1, 300)); rec.DoubleSided || len(pdfs(t, h.out)) != 1 {
		t.Fatalf("scan after cancel: %+v, output %v", rec, pdfs(t, h.out))
	}

	// Starting twice and cancelling when off change nothing.
	h.start()
	h.start()
	_ = h.svc.StartDoubleSided(false)
	_ = h.svc.StartDoubleSided(false)
	if h.dsTotal(metrics.DoubleSidedCancelled) != 2 {
		t.Errorf("cancelled = %v, want 2", h.dsTotal(metrics.DoubleSidedCancelled))
	}
}

// While a feeder pass runs its role is fixed: the mode cannot be started or
// cancelled, and the timeout waits for the scan.
func TestDoubleSidedStateIsFrozenWhileAPassRuns(t *testing.T) {
	h, offset := dsHarness(t)

	scanning := func() bool { return h.svc.Snapshot().Scanning != nil }
	slow := func(s escltest.Step) escltest.Step { s.Delay = 150 * time.Millisecond; return s }

	// A normal feeder scan is running: too late to make it the fronts.
	h.fake.QueueJob(slow(stack(1, 300)))
	if err := h.svc.Scan(escl.Feeder, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.StartDoubleSided(true); !errors.Is(err, ErrBusy) {
		t.Fatalf("start during a feeder scan: err = %v, want ErrBusy", err)
	}
	h.waitRecords(1)
	h.eventually("idle", func() bool { return !scanning() })

	h.start()
	h.pass(escl.Feeder, stack(2, 100))
	h.fake.QueueJob(slow(stack(2, 200)))
	if err := h.svc.Scan(escl.Feeder, ""); err != nil {
		t.Fatal(err)
	}
	if cur := h.svc.Snapshot().Scanning; cur == nil || cur.Pass != DoubleSidedBacks {
		t.Fatalf("running pass = %+v, want the backs", cur)
	}
	if err := h.svc.StartDoubleSided(false); !errors.Is(err, ErrBusy) {
		t.Fatalf("cancel during the backs: err = %v, want ErrBusy", err)
	}
	offset.Add(int64(31 * time.Minute)) // the deadline passes while the backs are scanned
	h.waitRecords(3)
	h.eventually("idle", func() bool { return !scanning() })

	if h.dsTotal(metrics.DoubleSidedDelivered) != 1 || h.dsTotal(metrics.DoubleSidedTimeout) != 0 || len(pdfs(t, h.out)) != 2 {
		t.Fatalf("delivered %v, timeout %v, output %v", h.dsTotal(metrics.DoubleSidedDelivered), h.dsTotal(metrics.DoubleSidedTimeout), pdfs(t, h.out))
	}
}

// The timeout saves the fronts while a flatbed scan delivers: two writers.
func TestDoubleSidedTimeoutDuringFlatbedScan(t *testing.T) {
	h, offset := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(2, 100))

	step := stack(1, 300)
	step.Delay = 50 * time.Millisecond
	h.fake.QueueJob(step)
	if err := h.svc.Scan(escl.Platen, ""); err != nil {
		t.Fatal(err)
	}
	offset.Add(int64(31 * time.Minute))
	h.waitRecords(2)
	h.eventually("fronts and flatbed scan delivered", func() bool { return len(pdfs(t, h.out)) == 2 })
	if h.dsTotal(metrics.DoubleSidedTimeout) != 1 {
		t.Error("timeout not counted")
	}
}

// unreadable passes output.ValidatePDF but is not a PDF a parser accepts.
func unreadable() escltest.Step {
	body := "%PDF-1.4\n1 0 obj\n<< /Type /Page >>\nendobj\n"
	return escltest.Step{Body: fmt.Appendf(nil, "%sxref\n0 1\n0000000000 65535 f \ntrailer\n<< /Size 1 >>\nstartxref\n%d\n%%%%EOF\n", body, len(body))}
}

// A scan must never be lost because the PDF library cannot read it.
func TestDoubleSidedUnreadablePDF(t *testing.T) {
	t.Run("fronts", func(t *testing.T) {
		h, _ := dsHarness(t)
		h.start()
		rec := h.pass(escl.Feeder, unreadable())
		s := h.svc.Snapshot()
		if rec.Result != metrics.ResultOK || rec.DoubleSided || len(rec.Files) != 1 || len(pdfs(t, h.out)) != 1 ||
			s.DoubleSided != DoubleSidedOff || s.DoubleSidedNote.Kind != NoteWarn || h.dsTotal(metrics.DoubleSidedFailed) != 1 {
			t.Fatalf("record %+v, snapshot %+v", rec, s)
		}
	})
	t.Run("backs", func(t *testing.T) {
		h, _ := dsHarness(t)
		h.start()
		h.pass(escl.Feeder, stack(1, 100))
		rec := h.pass(escl.Feeder, unreadable())
		s := h.svc.Snapshot()
		if rec.Result != metrics.ResultOK || rec.DoubleSided || len(rec.Files) != 2 || len(pdfs(t, h.out)) != 2 ||
			s.DoubleSided != DoubleSidedOff || !strings.Contains(s.DoubleSidedNote.Text, "separate") || h.dsTotal(metrics.DoubleSidedFailed) != 1 {
			t.Fatalf("record %+v, snapshot %+v, output %v", rec, s, pdfs(t, h.out))
		}
	})
}

func TestShutdownSavesHeldFronts(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(2, 100))
	h.svc.Shutdown(time.Second)
	if got, want := pageWidths(t, h.out), "[100 101]"; got != want {
		t.Errorf("saved fronts = %s, want %s", got, want)
	}
}
