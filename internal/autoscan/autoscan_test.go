package autoscan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escltest"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/pdftest"
)

const settle = 60 * time.Millisecond

type harness struct {
	t      *testing.T
	fake   *escltest.Server
	svc    *Service
	reg    *prometheus.Registry
	out    string
	failed string
}

func newHarness(t *testing.T, mod func(*Config), setup func(*escltest.Server)) *harness {
	t.Helper()
	fake := escltest.New(t)
	if setup != nil {
		setup(fake)
	}
	sum := sha256.Sum256(fake.Certificate().Raw)
	dev, err := escl.New(escl.Options{
		URL: fake.URL, Fingerprint: hex.EncodeToString(sum[:]),
		Timeout: 2 * time.Second, RetryInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		PollInterval: 5 * time.Millisecond, MaxBackoff: 40 * time.Millisecond, Settle: settle,
		Enabled: true, Intent: "Document", ColorMode: "Grayscale8", Resolution: 300,
		Region:      escl.Region{Width: 2550, Height: 3508},
		BusyBackoff: time.Millisecond, DeleteTimeout: time.Second,
	}
	if mod != nil {
		mod(&cfg)
	}
	h := &harness{t: t, fake: fake, reg: prometheus.NewRegistry(), out: t.TempDir(), failed: t.TempDir()}
	log := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.svc = New(cfg, dev, &output.Writer{Dir: h.out}, &output.Writer{Dir: h.failed},
		metrics.New(h.reg, escl.KnownAdfStates), log)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.svc.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		h.svc.Shutdown(time.Second)
	})
	return h
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; snapshot: %+v", what, h.svc.Snapshot())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) history() []Record { return h.svc.Snapshot().History }

func (h *harness) waitRecords(n int) []Record {
	h.t.Helper()
	h.eventually("scan records", func() bool { return len(h.history()) >= n })
	return h.history()
}

func (h *harness) armed() bool { return h.svc.Snapshot().Armed }

// metric returns the value of a metric with the given labels.
func (h *harness) metric(name string, labels ...string) float64 {
	h.t.Helper()
	mfs, err := h.reg.Gather()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.GetMetric() {
			for i := 0; i < len(labels); i += 2 {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == labels[i] && lp.GetValue() == labels[i+1] {
						found = true
					}
				}
				if !found {
					continue next
				}
			}
			if m.Counter != nil {
				return m.Counter.GetValue()
			}
			return m.Gauge.GetValue()
		}
	}
	return 0
}

func (h *harness) scans(source, result string) float64 {
	return h.metric("escl_autoscan_scans_total", "source", source, "result", result)
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// lastJobUUID is used by Before hooks to mark the running job in the status.
func lastJobUUID(s *escltest.Server) string {
	jobs := s.Jobs()
	return jobs[len(jobs)-1].UUID
}

var scanName = regexp.MustCompile(`^scan-\d{8}-\d{6}\.pdf$`)

func TestAutoscanDeliversOnceUntilFeederEmpty(t *testing.T) {
	h := newHarness(t, nil, func(s *escltest.Server) {
		// An old aborted job in the device's list must not affect new jobs.
		s.AddJobInfo("11111111-2222-4333-8444-555555555502", escl.JobAborted, 2)
	})
	h.eventually("armed", h.armed)
	if h.metric("escl_autoscan_scanner_up") != 1 || h.metric("escl_autoscan_armed") != 1 {
		t.Error("up/armed metrics not set")
	}

	pdf := pdftest.New(pdftest.Options{Pages: 3})
	h.fake.OnCreate(func(s *escltest.Server, _ *escltest.Job) { s.SetStatus("Processing", "ScannerAdfProcessing") })
	h.fake.QueueJob(escltest.Step{Body: pdf, Delay: 20 * time.Millisecond, Before: func(s *escltest.Server) {
		// The sensor still reports paper: must not trigger a second scan.
		s.SetStatus("Idle", "ScannerAdfLoaded")
		s.AddJobInfo(lastJobUUID(s), escl.JobCompleted, 3)
	}})
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")

	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultOK || rec.Pages != 3 || rec.Trigger != "auto" || rec.Source != "feeder" || len(rec.Files) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	names := files(t, h.out)
	if len(names) != 1 || !scanName.MatchString(names[0]) || names[0] != rec.Files[0] {
		t.Fatalf("output dir = %v", names)
	}
	got, _ := os.ReadFile(filepath.Join(h.out, names[0]))
	if !bytes.Equal(got, pdf) {
		t.Error("delivered file differs from the scanned PDF")
	}
	if len(files(t, h.failed)) != 0 {
		t.Error("failed dir not empty")
	}
	if h.scans("feeder", metrics.ResultOK) != 1 || h.metric("escl_autoscan_pages_total") != 3 || h.metric("escl_autoscan_last_success_timestamp_seconds") == 0 {
		t.Error("ok metrics not recorded")
	}

	jobs := h.fake.Jobs()
	if len(jobs) != 1 || !strings.Contains(jobs[0].Settings, "<pwg:InputSource>Feeder</pwg:InputSource>") ||
		!strings.Contains(jobs[0].Settings, "<scan:ColorMode>Grayscale8</scan:ColorMode>") {
		t.Fatalf("jobs = %+v", jobs)
	}

	// Still loaded: no second scan, however long we wait.
	time.Sleep(5 * settle)
	if n := len(h.fake.Jobs()); n != 1 || h.armed() {
		t.Fatalf("re-scanned before the feeder was empty: %d jobs, armed=%v", n, h.armed())
	}

	// Empty re-arms; the next stack is scanned.
	h.fake.QueueJob(escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 1})})
	h.fake.SetStatus("Idle", "ScannerAdfEmpty")
	h.eventually("re-armed", h.armed)
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	recs := h.waitRecords(2)
	if recs[0].Result != metrics.ResultOK || len(files(t, h.out)) != 2 {
		t.Fatalf("second scan: %+v, files %v", recs[0], files(t, h.out))
	}
	for _, r := range h.fake.Requests() {
		if !strings.HasSuffix(r.Host, ":"+strings.Split(h.fake.Listener.Addr().String(), ":")[1]) {
			t.Fatalf("Host header without port: %q", r.Host)
		}
	}
}

func TestSettleSkipsWhenPaperRemoved(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.eventually("armed", h.armed)
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	h.eventually("settling", func() bool { return h.svc.Snapshot().Settling })
	h.fake.SetStatus("Idle", "ScannerAdfEmpty")

	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultSkipped || rec.Message != "paper removed" {
		t.Fatalf("record = %+v", rec)
	}
	if len(h.fake.Jobs()) != 0 || h.scans("feeder", metrics.ResultSkipped) != 1 {
		t.Fatal("scanned although the paper was removed")
	}
	h.eventually("re-armed by empty feeder", h.armed)
}

func TestSettleSkipsWhenCopyStarted(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.eventually("armed", h.armed)
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	h.eventually("settling", func() bool { return h.svc.Snapshot().Settling })
	h.fake.SetStatus("Processing", "ScannerAdfLoaded") // somebody pressed Copy

	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultSkipped || !strings.Contains(rec.Message, "Processing") {
		t.Fatalf("record = %+v", rec)
	}
	// Back to idle with paper still in the feeder: stays disarmed.
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	time.Sleep(5 * settle)
	if len(h.fake.Jobs()) != 0 || h.armed() {
		t.Fatalf("scanned after a skip: jobs=%d armed=%v", len(h.fake.Jobs()), h.armed())
	}
}

func TestNotReadyThenDocumentAndBusyCreate(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.eventually("armed", h.armed)
	pdf := pdftest.New(pdftest.Options{Pages: 2})
	h.fake.QueueCreate(503, 409) // busy twice, then accepted
	h.fake.QueueJob(escltest.Step{Code: 503}, escltest.Step{Code: 503}, escltest.Step{Body: pdf})
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")

	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultOK || rec.Pages != 2 {
		t.Fatalf("record = %+v", rec)
	}
	posts := 0
	for _, r := range h.fake.Requests() {
		if r.Method == "POST" {
			posts++
		}
	}
	if posts != 3 {
		t.Errorf("POST requests = %d, want 3", posts)
	}
}

func TestBusyGivesUp(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BusyRetries = 2 }, nil)
	h.eventually("armed", h.armed)
	h.fake.QueueCreate(503, 503, 503, 503)
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")

	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultBusy || h.scans("feeder", metrics.ResultBusy) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	if len(files(t, h.out)) != 0 || len(h.fake.Jobs()) != 0 {
		t.Fatal("busy scan produced output")
	}
}

func TestJamNeverDelivers(t *testing.T) {
	pdf := pdftest.New(pdftest.Options{Pages: 3})
	truncated := pdf[:bytes.LastIndex(pdf, []byte("xref"))]
	jam := func(s *escltest.Server) {
		s.SetStatus("Stopped", "ScannerAdfJam")
		s.AddJobInfo(lastJobUUID(s), escl.JobAborted, 2)
	}

	t.Run("truncated PDF then 410", func(t *testing.T) {
		h := newHarness(t, nil, nil)
		h.eventually("armed", h.armed)
		h.fake.QueueJob(escltest.Step{Body: truncated, Before: jam}, escltest.Step{Code: 410})
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")

		rec := h.waitRecords(1)[0]
		if rec.Result != metrics.ResultAborted {
			t.Fatalf("result = %q, want aborted (%+v)", rec.Result, rec)
		}
		if n := files(t, h.out); len(n) != 0 {
			t.Fatalf("output dir = %v", n)
		}
		failed := files(t, h.failed)
		if len(failed) != 1 || !strings.HasSuffix(failed[0], "-aborted.pdf") {
			t.Fatalf("failed dir = %v", failed)
		}
		if got, _ := os.ReadFile(filepath.Join(h.failed, failed[0])); !bytes.Equal(got, truncated) {
			t.Error("failed dir holds something else than the partial scan")
		}
		if h.scans("feeder", metrics.ResultAborted) != 1 || h.scans("feeder", metrics.ResultIncompletePDF) != 0 ||
			h.metric("escl_autoscan_pages_total") != 0 {
			t.Error("metrics: want exactly one aborted scan and no pages")
		}

		// Jam cleared but paper left in the feeder: no automatic scan.
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
		time.Sleep(5 * settle)
		if len(h.fake.Jobs()) != 1 {
			t.Fatal("scanned again before the feeder was emptied")
		}
		h.fake.SetStatus("Idle", "ScannerAdfEmpty")
		h.eventually("re-armed", h.armed)
	})

	t.Run("feeder reports empty mid-job, then jams", func(t *testing.T) {
		h := newHarness(t, nil, nil)
		h.eventually("armed", h.armed)
		// Between sheets the sensor may report an empty feeder.
		h.fake.OnCreate(func(s *escltest.Server, _ *escltest.Job) { s.SetStatus("Processing", "ScannerAdfEmpty") })
		h.fake.QueueJob(escltest.Step{Body: truncated, Delay: 10 * settle, Before: jam}, escltest.Step{Code: 410})
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
		if rec := h.waitRecords(1)[0]; rec.Result != metrics.ResultAborted {
			t.Fatalf("record = %+v", rec)
		}
		// The jam is cleared and the rest of the stack is still in the feeder.
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
		time.Sleep(5 * settle)
		if n := len(h.fake.Jobs()); n != 1 || h.armed() {
			t.Fatalf("remaining stack scanned automatically: %d jobs, armed=%v", n, h.armed())
		}
	})

	t.Run("complete-looking PDF from an aborted job", func(t *testing.T) {
		h := newHarness(t, nil, nil)
		h.eventually("armed", h.armed)
		h.fake.QueueJob(escltest.Step{Body: pdf, Before: jam}) // then 404
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
		rec := h.waitRecords(1)[0]
		if rec.Result != metrics.ResultAborted || len(files(t, h.out)) != 0 {
			t.Fatalf("record = %+v, output %v", rec, files(t, h.out))
		}
	})

	t.Run("truncated PDF without abort", func(t *testing.T) {
		h := newHarness(t, nil, nil)
		h.eventually("armed", h.armed)
		h.fake.QueueJob(escltest.Step{Body: truncated, Before: func(s *escltest.Server) { s.SetStatus("Idle", "ScannerAdfEmpty") }})
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
		rec := h.waitRecords(1)[0]
		if rec.Result != metrics.ResultIncompletePDF || len(files(t, h.out)) != 0 {
			t.Fatalf("record = %+v, output %v", rec, files(t, h.out))
		}
		if f := files(t, h.failed); len(f) != 1 || !strings.HasSuffix(f[0], "-incomplete_pdf.pdf") {
			t.Fatalf("failed dir = %v", f)
		}
	})
}

func TestRejectWithoutFailedDirDiscards(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.svc.failed = nil
	h.eventually("armed", h.armed)
	h.fake.QueueJob(escltest.Step{Body: []byte("%PDF-1.4 garbage")}, escltest.Step{Code: 410})
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultAborted || len(rec.Files) != 0 || len(files(t, h.out)) != 0 || len(files(t, h.failed)) != 0 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestUnreachableBacksOffAndKeepsArm(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.eventually("armed", h.armed)
	h.fake.SetDown(true)
	h.eventually("up=0", func() bool { return h.metric("escl_autoscan_scanner_up") == 0 })
	if snap := h.svc.Snapshot(); snap.Reachable || snap.LastError == "" {
		t.Errorf("snapshot while down = %+v", snap)
	}

	before := len(h.fake.Requests())
	time.Sleep(400 * time.Millisecond)
	// Without backoff a 5ms interval would mean ~80 requests.
	if n := len(h.fake.Requests()) - before; n > 20 {
		t.Errorf("%d status requests in 400ms while unreachable; no backoff", n)
	}

	// The printer wakes up with paper already in the feeder.
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	h.fake.QueueJob(escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 1})})
	h.fake.SetDown(false)
	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultOK || h.metric("escl_autoscan_scanner_up") != 1 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestWakeGlitch(t *testing.T) {
	t.Run("detected", func(t *testing.T) {
		h := newHarness(t, nil, nil)
		h.eventually("armed", h.armed)
		h.fake.QueueJob() // 404 without a document, paper still loaded
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")
		rec := h.waitRecords(1)[0]
		if rec.Result != metrics.ResultWakeGlitch || h.scans("feeder", metrics.ResultWakeGlitch) != 1 {
			t.Fatalf("record = %+v", rec)
		}
		time.Sleep(5 * settle)
		if len(h.fake.Jobs()) != 1 || len(files(t, h.out)) != 0 {
			t.Fatalf("jobs = %d, files = %v", len(h.fake.Jobs()), files(t, h.out))
		}
	})

	t.Run("workaround", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.WakeWorkaround = true }, nil)
		h.eventually("armed", h.armed)
		h.fake.QueueJob()                                                            // feeder: glitch
		h.fake.QueueJob(escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 1})}) // platen wake scan
		h.fake.QueueJob(escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 2})}) // feeder retry
		h.fake.SetStatus("Idle", "ScannerAdfLoaded")

		rec := h.waitRecords(1)[0]
		if rec.Result != metrics.ResultOK || rec.Pages != 2 || !strings.Contains(rec.Message, "wake") {
			t.Fatalf("record = %+v", rec)
		}
		jobs := h.fake.Jobs()
		if len(jobs) != 3 || !strings.Contains(jobs[1].Settings, "Platen") || !strings.Contains(jobs[2].Settings, "Feeder") {
			t.Fatalf("jobs = %+v", jobs)
		}
		if n := files(t, h.out); len(n) != 1 {
			t.Fatalf("output = %v (the wake scan must not be delivered)", n)
		}
		if h.scans("feeder", metrics.ResultWakeGlitch) != 1 || h.scans("feeder", metrics.ResultOK) != 1 ||
			h.scans("platen", metrics.ResultOK) != 0 || h.metric("escl_autoscan_pages_total") != 2 {
			t.Error("metrics: want glitch=1, ok=1, platen untouched, 2 pages")
		}
	})
}

func TestManualScanAndBusy(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Enabled = false }, nil)
	h.eventually("reachable", func() bool { return h.svc.Snapshot().Reachable })
	h.fake.QueueJob(escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 1}), Delay: 100 * time.Millisecond})

	if err := h.svc.Scan(escl.Platen, "RGB24"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Scan(escl.Feeder, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("second scan: err = %v, want ErrBusy", err)
	}
	rec := h.waitRecords(1)[0]
	if rec.Result != metrics.ResultOK || rec.Source != "platen" || rec.Trigger != "manual" || rec.Color != "RGB24" {
		t.Fatalf("record = %+v", rec)
	}
	s := h.fake.Jobs()[0].Settings
	if !strings.Contains(s, "<pwg:InputSource>Platen</pwg:InputSource>") || strings.Contains(s, "Duplex") || !strings.Contains(s, "RGB24") {
		t.Errorf("settings = %s", s)
	}

	// Autoscan disabled: paper in the feeder is left alone.
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	time.Sleep(5 * settle)
	if len(h.fake.Jobs()) != 1 {
		t.Error("autoscan ran although disabled")
	}
}

func TestPauseAndResume(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Paused = true }, nil)
	h.eventually("armed", h.armed)
	if h.metric("escl_autoscan_paused") != 1 {
		t.Error("paused metric not set")
	}
	h.fake.QueueJob(escltest.Step{Body: pdftest.New(pdftest.Options{Pages: 1})})
	h.fake.SetStatus("Idle", "ScannerAdfLoaded")
	time.Sleep(5 * settle)
	if len(h.fake.Jobs()) != 0 {
		t.Fatal("scanned while paused")
	}
	h.svc.SetPaused(false)
	if rec := h.waitRecords(1)[0]; rec.Result != metrics.ResultOK {
		t.Fatalf("record = %+v", rec)
	}
}

func TestLoadedAtStartupIsNotScanned(t *testing.T) {
	h := newHarness(t, nil, func(s *escltest.Server) { s.SetStatus("Idle", "ScannerAdfLoaded") })
	h.eventually("reachable", func() bool { return h.svc.Snapshot().Reachable })
	time.Sleep(5 * settle)
	if len(h.fake.Jobs()) != 0 || h.armed() {
		t.Fatal("scanned a stack that was loaded before startup")
	}
}

func TestShutdownCancelsAndDeletesRunningJob(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Enabled = false }, nil)
	h.eventually("reachable", func() bool { return h.svc.Snapshot().Reachable })
	h.fake.QueueJob(escltest.Step{Body: []byte("never sent"), Delay: 10 * time.Second})
	if err := h.svc.Scan(escl.Feeder, ""); err != nil {
		t.Fatal(err)
	}
	h.eventually("job created", func() bool { return len(h.fake.Jobs()) == 1 })

	start := time.Now()
	h.svc.Shutdown(100 * time.Millisecond)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	if !h.fake.Jobs()[0].Deleted {
		t.Error("running job not deleted on the device")
	}
	if rec := h.history(); len(rec) != 1 || rec[0].Result != metrics.ResultError {
		t.Errorf("history = %+v", rec)
	}
	if err := h.svc.Scan(escl.Platen, ""); !errors.Is(err, ErrStopping) {
		t.Errorf("scan after shutdown: err = %v", err)
	}
}
