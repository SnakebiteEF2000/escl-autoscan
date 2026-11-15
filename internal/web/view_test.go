package web

import (
	"strings"
	"testing"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/autoscan"
)

func TestHero(t *testing.T) {
	now := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	// ready is an idle scanner with an empty feeder and autoscan armed.
	ready := func(mod func(*autoscan.Snapshot)) autoscan.Snapshot {
		s := autoscan.Snapshot{Reachable: true, State: "Idle", AdfState: "ScannerAdfEmpty", LastPoll: now,
			AutoscanEnabled: true, Armed: true, Settle: 10 * time.Second}
		if mod != nil {
			mod(&s)
		}
		return s
	}
	for _, tc := range []struct {
		name          string
		snap          autoscan.Snapshot
		tone, title   string
		detailContain string
	}{
		{"ready", ready(nil), "ok", "Ready for paper", "after 10 s"},
		{"before the first poll", ready(func(s *autoscan.Snapshot) { s.LastPoll, s.Reachable = time.Time{}, false }), "idle", "Connecting to the scanner", ""},
		{"asleep", ready(func(s *autoscan.Snapshot) { s.Reachable, s.State, s.AdfState = false, "", "" }), "idle", "Scanner not reachable", "scanned once it answers again"},
		{"asleep, paused", ready(func(s *autoscan.Snapshot) { s.Reachable, s.Paused = false, true }), "idle", "Scanner not reachable", "asleep or switched off."},
		{"jam", ready(func(s *autoscan.Snapshot) { s.State, s.AdfState, s.Armed = "Stopped", "ScannerAdfJam", false }), "bad", "Paper jam in the feeder", "put the stack back"},
		{"unknown fault", ready(func(s *autoscan.Snapshot) { s.AdfState = "ScannerAdfInputTrayFailed" }), "bad", "The feeder has a problem", ""},
		{"settling", ready(func(s *autoscan.Snapshot) { s.AdfState, s.Settling = "ScannerAdfLoaded", true }), "busy", "Paper detected", "Scanning starts within 10 s"},
		{"settling, backs", ready(func(s *autoscan.Snapshot) {
			s.AdfState, s.Settling, s.DoubleSided = "ScannerAdfLoaded", true, autoscan.DoubleSidedBacks
		}), "busy", "Paper detected", "Scanning the backs starts"},
		{"copying", ready(func(s *autoscan.Snapshot) { s.State = "Processing" }), "warn", "Scanner is busy", "Processing"},
		{"stopped", ready(func(s *autoscan.Snapshot) { s.State = "Stopped" }), "bad", "Scanner stopped", "display"},
		{"autoscan off", ready(func(s *autoscan.Snapshot) { s.AutoscanEnabled, s.Armed = false, false }), "idle", "Ready", "buttons"},
		{"paused", ready(func(s *autoscan.Snapshot) { s.Paused = true }), "warn", "Autoscan is paused", "resume"},
		{"loaded at startup", ready(func(s *autoscan.Snapshot) { s.AdfState, s.Armed = "ScannerAdfLoaded", false }), "warn", "Paper is waiting in the feeder", "Press Scan feeder"},
		{"between sheets", ready(func(s *autoscan.Snapshot) { s.AdfState, s.Armed = "ScannerAdfProcessing", false }), "idle", "Waiting for an empty feeder", ""},
		{"fronts", ready(func(s *autoscan.Snapshot) { s.DoubleSided = autoscan.DoubleSidedFronts }), "ok", "Ready for the fronts", "Put the stack"},
		{"backs", ready(func(s *autoscan.Snapshot) { s.DoubleSided = autoscan.DoubleSidedBacks }), "ok", "Ready for the backs", "Turn the stack over"},
		{"flatbed, first page", ready(func(s *autoscan.Snapshot) { s.DoubleSided = autoscan.DoubleSidedFlatbed }), "ok", "Ready for the first page", "Scan first page"},
		{"flatbed, next page", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.DoubleSidedHeld = autoscan.DoubleSidedFlatbed, 2
		}), "ok", "Ready for the next page", "2 pages so far"},
		// A flatbed document does not need autoscan, but paper in the feeder still comes first.
		{"flatbed, autoscan paused", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.DoubleSidedHeld, s.Paused = autoscan.DoubleSidedFlatbed, 1, true
		}), "ok", "Ready for the next page", "1 page so far"},
		{"flatbed, paper detected", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.AdfState, s.Settling = autoscan.DoubleSidedFlatbed, "ScannerAdfLoaded", true
		}), "busy", "Paper detected", "Scanning starts within 10 s"},
		// A running scan comes first, whatever the scanner reports meanwhile.
		{"scanning", ready(func(s *autoscan.Snapshot) {
			s.State, s.AdfState, s.Armed = "Processing", "ScannerAdfProcessing", false
			s.Scanning = &autoscan.Current{Source: "feeder", Started: now.Add(-42 * time.Second)}
		}), "busy", "Scanning the feeder", "Running for 42 s"},
		{"scanning the flatbed", ready(func(s *autoscan.Snapshot) {
			s.Scanning = &autoscan.Current{Source: "platen", Started: now.Add(-3 * time.Minute)}
		}), "busy", "Scanning the flatbed", "Running for 3 min"},
		{"scanning the backs", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided = autoscan.DoubleSidedBacks
			s.Scanning = &autoscan.Current{Source: "feeder", Pass: autoscan.DoubleSidedBacks, Started: now}
		}), "busy", "Scanning the backs", "Running for 0 s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHero(tc.snap, now)
			if h.Tone != tc.tone || h.Title != tc.title || !strings.Contains(h.Detail, tc.detailContain) {
				t.Errorf("hero = %+v, want %s / %q / detail containing %q", h, tc.tone, tc.title, tc.detailContain)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	for adf, want := range map[string]string{
		"": "unknown", "ScannerAdfEmpty": "empty", "ScannerAdfLoaded": "paper loaded",
		"ScannerAdfProcessing": "feeding", "ScannerAdfJam": "paper jam", "ScannerAdfMispick": "Mispick",
	} {
		if got := feederLabel(adf); got != want {
			t.Errorf("feederLabel(%q) = %q, want %q", adf, got, want)
		}
	}
	for want, r := range map[string]autoscan.Record{
		"saved":          {Result: "ok", Files: []string{"scan.pdf"}},
		"held":           {Result: "ok", DoubleSided: true},
		"failed":         {Result: "error"},
		"aborted":        {Result: "aborted"},
		"incomplete PDF": {Result: "incomplete_pdf"},
		"skipped":        {Result: "skipped"},
	} {
		if got := resultLabel(r); got != want {
			t.Errorf("resultLabel(%+v) = %q, want %q", r, got, want)
		}
	}
	// The time left in a double-sided step changes once a minute only.
	for d, want := range map[time.Duration]string{
		-time.Second: "0 min", time.Second: "1 min", 61 * time.Second: "2 min", 30 * time.Minute: "30 min",
	} {
		if got := minutes(d); got != want {
			t.Errorf("minutes(%v) = %q, want %q", d, got, want)
		}
	}
	for d, want := range map[time.Duration]string{
		-time.Second: "0 s", 10 * time.Second: "10 s", 89 * time.Second: "89 s",
		90 * time.Second: "2 min", 29*time.Minute + 30*time.Second: "30 min",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
}
