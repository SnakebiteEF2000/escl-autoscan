package autoscan

import (
	"bytes"
	"errors"
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

// dsHarness has double-sided mode enabled, autoscan off (passes are started
// manually, as with autoscan paused) and a clock that tests can advance.
func dsHarness(t *testing.T) (*harness, *atomic.Int64, string) {
	t.Helper()
	offset := &atomic.Int64{}
	h := newHarness(t, func(c *Config) {
		c.Enabled = false
		c.DoubleSidedSubdir = "double-sided"
		c.DoubleSidedTimeout = 30 * time.Minute
		c.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	}, nil)
	sub := filepath.Join(h.out, "double-sided")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	h.eventually("reachable", func() bool { return h.svc.Snapshot().Reachable })
	return h, offset, sub
}

// pass queues one document, scans it from source and waits for the record.
func (h *harness) pass(source escl.InputSource, steps ...escltest.Step) Record {
	h.t.Helper()
	if len(steps) == 0 {
		steps = []escltest.Step{{Body: pdftest.New(pdftest.Options{Pages: 2})}}
	}
	n := len(h.history())
	h.fake.QueueJob(steps...)
	if err := h.svc.Scan(source, ""); err != nil {
		h.t.Fatal(err)
	}
	return h.waitRecords(n + 1)[0]
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

func TestDoubleSidedPair(t *testing.T) {
	h, _, sub := dsHarness(t)
	if err := h.svc.StartDoubleSided(true); err != nil {
		t.Fatal(err)
	}
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedFronts || !s.DoubleSidedEnabled || h.metric("escl_autoscan_double_sided_state") != 1 {
		t.Fatalf("after start: %+v", s)
	}

	fronts := h.pass(escl.Feeder)
	if !fronts.DoubleSided || len(pdfs(t, sub)) != 1 || h.svc.Snapshot().DoubleSided != DoubleSidedBacks {
		t.Fatalf("fronts: %+v, subdir %v", fronts, pdfs(t, sub))
	}

	// A flatbed scan in between goes to the normal output and changes nothing.
	if rec := h.pass(escl.Platen); rec.DoubleSided || len(pdfs(t, h.out)) != 1 || h.svc.Snapshot().DoubleSided != DoubleSidedBacks {
		t.Fatalf("platen: %+v", rec)
	}

	backs := h.pass(escl.Feeder)
	if !backs.DoubleSided || !strings.HasPrefix(backs.Files[0], "double-sided/scan-") || len(pdfs(t, sub)) != 2 {
		t.Fatalf("backs: %+v, subdir %v", backs, pdfs(t, sub))
	}
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedOff || s.DoubleSidedNote != "" ||
		h.metric("escl_autoscan_double_sided_total", "result", metrics.DoubleSidedDelivered) != 1 ||
		h.metric("escl_autoscan_double_sided_state") != 0 {
		t.Fatalf("after pair: %+v", s)
	}

	// The third feeder scan is a normal one again.
	if rec := h.pass(escl.Feeder); rec.DoubleSided || len(pdfs(t, sub)) != 2 || len(pdfs(t, h.out)) != 2 {
		t.Fatalf("third scan: %+v", rec)
	}
}

func TestDoubleSidedTimeoutAfterFronts(t *testing.T) {
	h, offset, sub := dsHarness(t)
	_ = h.svc.StartDoubleSided(true)
	h.pass(escl.Feeder)
	offset.Add(int64(31 * time.Minute))
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedOff || !strings.Contains(s.DoubleSidedNote, "timed out") {
		t.Fatalf("after timeout: %+v", s)
	}
	if rec := h.pass(escl.Feeder); rec.DoubleSided || len(pdfs(t, sub)) != 1 || len(pdfs(t, h.out)) != 1 {
		t.Fatalf("scan after timeout: %+v", rec)
	}
	if h.metric("escl_autoscan_double_sided_total", "result", metrics.DoubleSidedTimeout) != 1 {
		t.Error("timeout not counted")
	}
}

func TestDoubleSidedFailedBacksCancel(t *testing.T) {
	h, _, sub := dsHarness(t)
	_ = h.svc.StartDoubleSided(true)
	h.pass(escl.Feeder)

	pdf := pdftest.New(pdftest.Options{Pages: 2})
	truncated := pdf[:bytes.LastIndex(pdf, []byte("xref"))]
	rec := h.pass(escl.Feeder, escltest.Step{Body: truncated, Before: func(s *escltest.Server) {
		s.SetStatus("Stopped", "ScannerAdfJam")
	}}, escltest.Step{Code: 410})
	if rec.Result != metrics.ResultAborted || rec.DoubleSided {
		t.Fatalf("backs: %+v", rec)
	}
	if n := pdfs(t, sub); len(n) != 1 {
		t.Fatalf("subdir = %v, want only the fronts", n)
	}
	s := h.svc.Snapshot()
	if s.DoubleSided != DoubleSidedOff || !strings.Contains(s.DoubleSidedNote, "Paperless keeps the delivered fronts") ||
		h.metric("escl_autoscan_double_sided_total", "result", metrics.DoubleSidedFailed) != 1 {
		t.Fatalf("after failed backs: %+v", s)
	}
}

func TestDoubleSidedSeveralDocumentsGoToNormalOutput(t *testing.T) {
	h, _, sub := dsHarness(t)
	_ = h.svc.StartDoubleSided(true)
	one := pdftest.New(pdftest.Options{Pages: 1})
	rec := h.pass(escl.Feeder, escltest.Step{Body: one}, escltest.Step{Body: one})
	if rec.Result != metrics.ResultOK || rec.DoubleSided || len(pdfs(t, h.out)) != 2 || len(pdfs(t, sub)) != 0 {
		t.Fatalf("record %+v, out %v, subdir %v", rec, pdfs(t, h.out), pdfs(t, sub))
	}
	if h.svc.Snapshot().DoubleSided != DoubleSidedOff || h.metric("escl_autoscan_double_sided_total", "result", metrics.DoubleSidedFailed) != 1 {
		t.Fatal("mode not cancelled")
	}
}

func TestDoubleSidedCancelAndDisabled(t *testing.T) {
	h, _, _ := dsHarness(t)
	_ = h.svc.StartDoubleSided(true)
	_ = h.svc.StartDoubleSided(false)
	if s := h.svc.Snapshot(); s.DoubleSided != DoubleSidedOff ||
		h.metric("escl_autoscan_double_sided_total", "result", metrics.DoubleSidedCancelled) != 1 {
		t.Fatalf("after cancel: %+v", s)
	}

	off := newHarness(t, nil, nil)
	if err := off.svc.StartDoubleSided(true); !errors.Is(err, ErrDoubleSidedDisabled) || off.svc.Snapshot().DoubleSidedEnabled {
		t.Fatalf("disabled: err = %v", err)
	}
}
