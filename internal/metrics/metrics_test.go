package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAdfStateGaugeSet(t *testing.T) {
	m := New(prometheus.NewRegistry(), []string{"ScannerAdfEmpty", "ScannerAdfLoaded"})
	get := func(s string) float64 { return testutil.ToFloat64(m.adfState.WithLabelValues(s)) }

	m.SetAdfState("ScannerAdfLoaded")
	if get("ScannerAdfLoaded") != 1 || get("ScannerAdfEmpty") != 0 {
		t.Error("loaded not exclusive")
	}
	m.SetAdfState("ScannerAdfSomethingNew")
	if get("ScannerAdfSomethingNew") != 1 || get("ScannerAdfLoaded") != 0 {
		t.Error("unknown state not exported exclusively")
	}
	m.SetAdfState("")
	for _, s := range []string{"ScannerAdfEmpty", "ScannerAdfLoaded", "ScannerAdfSomethingNew"} {
		if get(s) != 0 {
			t.Errorf("%s = 1 while unreachable", s)
		}
	}
}

func TestScanCounters(t *testing.T) {
	m := New(prometheus.NewRegistry(), nil)
	at := time.Unix(1_700_000_000, 0)
	m.Scan("feeder", ResultOK, 3, 45*time.Second, at)
	m.Scan("feeder", ResultAborted, 2, 30*time.Second, at.Add(time.Hour))
	m.Scan("feeder", ResultSkipped, 0, 0, at)

	if v := testutil.ToFloat64(m.pages); v != 3 {
		t.Errorf("pages_total = %v, want 3 (aborted pages not counted)", v)
	}
	if v := testutil.ToFloat64(m.lastSuccess); v != float64(at.Unix()) {
		t.Errorf("last success = %v", v)
	}
	if v := testutil.ToFloat64(m.scans.WithLabelValues("feeder", ResultAborted)); v != 1 {
		t.Errorf("aborted = %v", v)
	}
	if n := testutil.CollectAndCount(m.scans); n != len(Sources)*len(Results) {
		t.Errorf("scans_total series = %d, want all combinations pre-created", n)
	}
	if n := testutil.CollectAndCount(m.duration); n != 1 {
		t.Errorf("histogram count = %d", n)
	}
}

func TestScanBacklog(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(rel string, age time.Duration) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("a.pdf", time.Minute)
	write("scanner/scan-1.PDF", 10*time.Minute)
	write("scanner/b.jpeg", time.Second)
	write("mail/c.eml", 2*time.Minute)
	write("scanner/scan-2.pdf.part", time.Hour) // in flight
	write("scanner/scan-3-123.part", time.Hour) // in flight
	write(".hidden.pdf", time.Hour)             // dotfile
	write(".stversions/old.pdf", time.Hour)     // dot directory
	write("notes.docx", time.Hour)              // not consumable

	n, oldest, err := ScanBacklog(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("files = %d, want 4", n)
	}
	if oldest < 10*time.Minute || oldest > 11*time.Minute {
		t.Errorf("oldest = %v, want ~10m", oldest)
	}

	empty := t.TempDir()
	if n, oldest, err := ScanBacklog(empty, now); n != 0 || oldest != 0 || err != nil {
		t.Errorf("empty: %d %v %v", n, oldest, err)
	}
	if _, _, err := ScanBacklog(filepath.Join(dir, "missing"), now); err == nil {
		t.Error("missing dir: want error")
	}
}
