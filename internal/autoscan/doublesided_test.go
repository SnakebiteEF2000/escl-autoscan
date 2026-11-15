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

// widthsOf returns the page widths of one file in dir.
func widthsOf(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	w, err := pdftest.Widths(data)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(w)
}

// stays reports that cond still holds after the Run loop had time for
// several polls (the harness polls every 5 ms).
func (h *harness) stays(what string, cond func() bool) {
	h.t.Helper()
	time.Sleep(60 * time.Millisecond)
	if !cond() {
		h.t.Fatalf("%s: no longer true; snapshot: %+v", what, h.svc.Snapshot())
	}
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
	h, offset := dsHarness(t)
	h.start()
	state := func(want int) func() bool {
		return func() bool { return h.svc.Snapshot().DoubleSided == want }
	}

	// Every step and every failed pass gets the full timeout again: the
	// clock moves 20 of the 30 minutes before each of them.
	offset.Add(int64(20 * time.Minute))
	if rec := h.pass(escl.Feeder, jam()...); rec.Result != metrics.ResultAborted {
		t.Fatalf("jammed fronts: %+v", rec)
	}
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedFronts || s.DoubleSidedNote.Kind != NoteWarn || !strings.Contains(s.DoubleSidedNote.Text, "fronts again") {
		t.Fatalf("after jammed fronts: %+v", s)
	}
	offset.Add(int64(20 * time.Minute))
	h.stays("waiting for the fronts 20 minutes after the jam", state(DoubleSidedFronts))
	h.pass(escl.Feeder, stack(2, 100))
	offset.Add(int64(20 * time.Minute))
	h.stays("waiting for the backs 20 minutes after the fronts", state(DoubleSidedBacks))
	if h.metric("escl_autoscan_double_sided_state") != 2 {
		t.Errorf("double_sided_state = %v, want 2", h.metric("escl_autoscan_double_sided_state"))
	}

	if rec := h.pass(escl.Feeder, jam()...); rec.Result != metrics.ResultAborted || rec.DoubleSided {
		t.Fatalf("jammed backs: %+v", rec)
	}
	s := h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedBacks || s.DoubleSidedFronts != 2 || !strings.Contains(s.DoubleSidedNote.Text, "still held") || len(pdfs(t, h.out)) != 0 {
		t.Fatalf("after jammed backs: %+v, output %v", s, pdfs(t, h.out))
	}
	offset.Add(int64(20 * time.Minute))
	h.stays("waiting for the backs 20 minutes after the jam", state(DoubleSidedBacks))

	h.pass(escl.Feeder, stack(2, 200))
	if got, want := pageWidths(t, h.out), "[100 201 101 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	if h.dsTotal(metrics.DoubleSidedDelivered) != 1 || h.dsTotal(metrics.DoubleSidedFailed) != 0 || h.dsTotal(metrics.DoubleSidedTimeout) != 0 {
		t.Error("double_sided_total: want one delivered run, no failed one and no timeout")
	}
}

// A scan that cannot be the backs (another page count) is still a valid
// scan: somebody may have scanned a letter while the fronts were held. It is
// delivered as a normal scan, never thrown away, and the fronts stay.
func TestDoubleSidedPageCountMismatch(t *testing.T) {
	h, _ := dsHarness(t)
	h.start()
	h.pass(escl.Feeder, stack(3, 100))

	rec := h.pass(escl.Feeder, stack(2, 200))
	if rec.Result != metrics.ResultOK || rec.DoubleSided || len(rec.Files) != 1 || !strings.Contains(rec.Message, "3 front pages but 2 back pages") {
		t.Fatalf("scan of 2 pages: %+v", rec)
	}
	if got, want := pageWidths(t, h.out), "[200 201]"; got != want {
		t.Errorf("delivered as a normal scan: %s, want %s", got, want)
	}
	s := h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedBacks || s.DoubleSidedFronts != 3 || s.DoubleSidedNote.Kind != NoteWarn ||
		!strings.Contains(s.DoubleSidedNote.Text, "3 pages but this scan had 2 pages") || !strings.Contains(s.DoubleSidedNote.Text, rec.Files[0]) {
		t.Fatalf("after the scan of 2 pages: %+v", s)
	}
	if failed := pdfs(t, h.failed); len(failed) != 0 {
		t.Errorf("failed dir = %v, want nothing: the scan was valid", failed)
	}

	backs := h.pass(escl.Feeder, stack(3, 300))
	if got, want := widthsOf(t, h.out, backs.Files[0]), "[100 302 101 301 102 300]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	if n := pdfs(t, h.out); len(n) != 2 || h.dsTotal(metrics.DoubleSidedDelivered) != 1 {
		t.Errorf("output %v, delivered %v", n, h.dsTotal(metrics.DoubleSidedDelivered))
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

// Double-sided with autoscan doing both passes. The deadline passes while
// the turned-over stack is already in the feeder and the settle time runs:
// that stack still is the backs.
func TestDoubleSidedByAutoscanAndTimeoutDuringSettle(t *testing.T) {
	offset := &atomic.Int64{}
	h := newHarness(t, func(c *Config) {
		c.Settle = 500 * time.Millisecond // long enough to be seen on a slow machine
		c.DoubleSidedTimeout = 30 * time.Minute
		c.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	}, nil)
	h.eventually("armed", h.armed)
	h.start()
	insert := func(step escltest.Step) {
		// The job empties the feeder, which arms autoscan for the next stack.
		step.Before = func(s *escltest.Server) { s.SetStatus("Idle", "ScannerAdfEmpty") }
		h.fake.QueueJob(step)
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	}

	insert(stack(2, 100))
	if rec := h.waitRecords(1)[0]; rec.Trigger != "auto" || !rec.DoubleSided {
		t.Fatalf("fronts: %+v", rec)
	}
	h.eventually("armed again, waiting for the backs", func() bool {
		s := h.svc.Snapshot()
		return s.Armed && s.DoubleSided == DoubleSidedBacks && s.Scanning == nil
	})

	insert(stack(2, 200))
	h.eventually("paper detected", func() bool { return h.svc.Snapshot().Settling })
	offset.Add(int64(31 * time.Minute))
	h.waitRecords(2)
	h.eventually("mode off", func() bool { return h.svc.Snapshot().DoubleSided == DoubleSidedOff })

	if got, want := pageWidths(t, h.out), "[100 201 101 200]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	if h.dsTotal(metrics.DoubleSidedDelivered) != 1 || h.dsTotal(metrics.DoubleSidedTimeout) != 0 {
		t.Errorf("delivered %v, timeout %v, want 1 and 0", h.dsTotal(metrics.DoubleSidedDelivered), h.dsTotal(metrics.DoubleSidedTimeout))
	}
}

// While OUTPUT_DIR cannot be written, held fronts are not given up: neither
// by the timeout nor when collating fails.
func TestDoubleSidedKeepsFrontsWhileOutputDirIsUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	h, offset := dsHarness(t)
	readOnly := func(on bool) {
		mode := os.FileMode(0o700)
		if on {
			mode = 0o500
		}
		if err := os.Chmod(h.out, mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { readOnly(false) })
	held := func() bool {
		s := h.svc.Snapshot()
		return s.DoubleSided == DoubleSidedBacks && s.DoubleSidedFronts == 2
	}
	h.start()
	h.pass(escl.Feeder, stack(2, 100))

	// The timeout cannot save the fronts: they are held again.
	readOnly(true)
	offset.Add(int64(31 * time.Minute))
	h.eventually("note about the failed save", func() bool {
		return strings.Contains(h.svc.Snapshot().DoubleSidedNote.Text, "could not be saved")
	})
	if !held() || h.dsTotal(metrics.DoubleSidedTimeout) != 0 {
		t.Fatalf("after the failed save: %+v, timeout %v", h.svc.Snapshot(), h.dsTotal(metrics.DoubleSidedTimeout))
	}

	// Backs that cannot be collated, and nothing can be written: this scan
	// is rejected like any scan that cannot be written, the fronts stay.
	rec := h.pass(escl.Feeder, unreadable())
	if rec.Result != metrics.ResultError || !held() {
		t.Fatalf("unreadable backs, read-only output: %+v, snapshot %+v", rec, h.svc.Snapshot())
	}
	// The collated PDF cannot be written either.
	if rec := h.pass(escl.Feeder, stack(2, 200)); rec.Result != metrics.ResultError || !held() {
		t.Fatalf("backs, read-only output: %+v, snapshot %+v", rec, h.svc.Snapshot())
	}

	readOnly(false)
	backs := h.pass(escl.Feeder, stack(2, 300))
	if got, want := widthsOf(t, h.out, backs.Files[0]), "[100 301 101 300]"; got != want {
		t.Errorf("page order = %s, want %s", got, want)
	}
	if n := pdfs(t, h.out); len(n) != 1 || h.dsTotal(metrics.DoubleSidedDelivered) != 1 {
		t.Errorf("output %v, delivered %v", n, h.dsTotal(metrics.DoubleSidedDelivered))
	}
}

// pdfcpu fails on this input, which passes ValidatePDF and collate.Pages
// (found by fuzzing), most of the time with a panic. The service must
// survive and deliver both passes.
func TestDoubleSidedSurvivesAPdfcpuPanic(t *testing.T) {
	crasher, err := os.ReadFile("../collate/testdata/pdfcpu-panic.pdf")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := dsHarness(t)
	h.start()
	if rec := h.pass(escl.Feeder, escltest.Step{Body: crasher}); !rec.DoubleSided {
		t.Fatalf("fronts: %+v (the fixture should be accepted as fronts)", rec)
	}
	rec := h.pass(escl.Feeder, stack(2, 200))
	s := h.svc.Snapshot()
	if rec.Result != metrics.ResultOK || len(rec.Files) != 2 || len(pdfs(t, h.out)) != 2 || !strings.Contains(rec.Message, "could not collate") ||
		s.DoubleSided != DoubleSidedOff || h.dsTotal(metrics.DoubleSidedFailed) != 1 {
		t.Fatalf("record %+v, snapshot %+v, output %v", rec, s, pdfs(t, h.out))
	}
}
