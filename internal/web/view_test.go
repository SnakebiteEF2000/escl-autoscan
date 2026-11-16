package web

import (
	"fmt"
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
		// A double-sided document in the making comes before what autoscan is doing.
		{"double-sided on", ready(func(s *autoscan.Snapshot) { s.DoubleSided = autoscan.DoubleSidedWaiting }), "ok",
			"Ready for a double-sided document", "Put the stack into the feeder, or put the first page on the glass and press Scan flatbed."},
		{"double-sided on, autoscan paused", ready(func(s *autoscan.Snapshot) { s.DoubleSided, s.Paused = autoscan.DoubleSidedWaiting, true }), "ok",
			"Ready for a double-sided document", "Put the stack into the feeder and press Scan feeder, or"},
		{"double-sided on, stack already there", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.AdfState, s.Armed = autoscan.DoubleSidedWaiting, "ScannerAdfLoaded", false
		}), "ok", "Ready for a double-sided document", "Press Scan feeder for the stack in the feeder, or"},
		{"backs", ready(func(s *autoscan.Snapshot) { s.DoubleSided = autoscan.DoubleSidedBacks }), "ok", "Ready for the backs", "Turn the stack over and put it back into the feeder. Scanning starts"},
		{"backs, autoscan paused", ready(func(s *autoscan.Snapshot) { s.DoubleSided, s.Paused = autoscan.DoubleSidedBacks, true }), "ok",
			"Ready for the backs", "put it back into the feeder and press Scan feeder"},
		{"backs, stack waiting after a failed pass", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.AdfState, s.Armed = autoscan.DoubleSidedBacks, "ScannerAdfLoaded", false
		}), "ok", "Ready for the backs", "Press Scan feeder to scan the stack in the feeder as the backs."},
		{"flatbed document", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.DoubleSidedHeld, s.Paused = autoscan.DoubleSidedFlatbed, 2, true
		}), "ok", "Ready for the next page", "press Scan flatbed, or save the document: 2 pages so far"},
		{"flatbed document, paper detected in the feeder", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.DoubleSidedHeld, s.AdfState, s.Settling = autoscan.DoubleSidedFlatbed, 1, "ScannerAdfLoaded", true
		}), "busy", "Paper detected", "Scanning starts within 10 s"},
		{"fronts detected", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided, s.AdfState, s.Settling = autoscan.DoubleSidedWaiting, "ScannerAdfLoaded", true
		}), "busy", "Paper detected", "Scanning the fronts starts"},
		// A running scan comes first, whatever the scanner reports meanwhile.
		{"scanning", ready(func(s *autoscan.Snapshot) {
			s.State, s.AdfState, s.Armed = "Processing", "ScannerAdfProcessing", false
			s.Scanning = &autoscan.Current{Source: "feeder", Started: now.Add(-42 * time.Second)}
		}), "busy", "Scanning the feeder", "Running for 42 s"},
		{"scanning the flatbed", ready(func(s *autoscan.Snapshot) {
			s.Scanning = &autoscan.Current{Source: "platen", Started: now.Add(-3 * time.Minute)}
		}), "busy", "Scanning the flatbed", "Running for 3 min"},
		{"scanning the fronts", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided = autoscan.DoubleSidedWaiting
			s.Scanning = &autoscan.Current{Source: "feeder", Pass: autoscan.DoubleSidedWaiting, Started: now}
		}), "busy", "Scanning the fronts", ""},
		{"scanning the first flatbed page", ready(func(s *autoscan.Snapshot) {
			s.DoubleSided = autoscan.DoubleSidedWaiting
			s.Scanning = &autoscan.Current{Source: "platen", Pass: autoscan.DoubleSidedWaiting, Started: now}
		}), "busy", "Scanning the flatbed", ""},
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

// The document in the making is drawn as sheets. A full feeder (50 sheets)
// or a long flatbed document must not flood the page.
func TestDocumentSheets(t *testing.T) {
	show := func(d document) string {
		var b strings.Builder
		if d.Before > 0 {
			fmt.Fprintf(&b, "(%d before) ", d.Before)
		}
		for _, s := range d.Sheets {
			if s.Held {
				fmt.Fprintf(&b, "[%d] ", s.N)
			} else {
				fmt.Fprintf(&b, "%d ", s.N)
			}
		}
		if d.After > 0 {
			fmt.Fprintf(&b, "(%d after)", d.After)
		}
		return strings.TrimSpace(b.String())
	}
	for _, tc := range []struct {
		name        string
		state, held int
		want        string
	}{
		{"off", autoscan.DoubleSidedOff, 0, ""},
		{"on", autoscan.DoubleSidedWaiting, 0, "1"},
		{"fronts of 3 sheets", autoscan.DoubleSidedBacks, 3, "[1] 2 [3] 4 [5] 6"},
		{"fronts of 50 sheets", autoscan.DoubleSidedBacks, 50, "[1] 2 [3] 4 [5] 6 [7] 8 [9] 10 [11] 12 [13] 14 [15] 16 (84 after)"},
		{"flatbed, 2 pages", autoscan.DoubleSidedFlatbed, 2, "[1] [2] 3"},
		{"flatbed, 40 pages", autoscan.DoubleSidedFlatbed, 40, "(25 before) [26] [27] [28] [29] [30] [31] [32] [33] [34] [35] [36] [37] [38] [39] [40] 41"},
	} {
		if got := show(newDocument(autoscan.Snapshot{DoubleSided: tc.state, DoubleSidedHeld: tc.held})); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}
