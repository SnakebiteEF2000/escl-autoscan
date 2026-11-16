package web

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/autoscan"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
)

// hero is the top of the page: what the scanner is doing right now, and
// what to do next.
type hero struct {
	Tone   string // ok, busy, warn, bad or idle; a CSS class
	Title  string
	Detail string
}

// newHero picks the one statement that matters most. The order is the
// priority: a running scan first, then what keeps the scanner from scanning,
// then what it is waiting for.
func newHero(s autoscan.Snapshot, now time.Time) hero {
	settle := span(s.Settle)
	switch {
	case s.Scanning != nil:
		return hero{"busy", "Scanning the " + scanSubject(s.Scanning),
			"Running for " + span(now.Sub(s.Scanning.Started)) + ". The result appears under Recent scans."}
	case s.LastPoll.IsZero():
		return hero{"idle", "Connecting to the scanner", ""}
	case !s.Reachable:
		detail := "It is asleep or switched off."
		if s.AutoscanEnabled && !s.Paused && s.Armed {
			detail += " Paper put into the feeder is scanned once it answers again."
		}
		return hero{"idle", "Scanner not reachable", detail}
	case escl.AdfIsFault(s.AdfState):
		return hero{"bad", faultTitle(s.AdfState), "Fix it at the scanner, then put the stack back into the feeder."}
	case s.Settling:
		what := "Scanning"
		switch s.DoubleSided {
		case autoscan.DoubleSidedWaiting:
			what = "Scanning the fronts"
		case autoscan.DoubleSidedBacks:
			what = "Scanning the backs"
		}
		return hero{"busy", "Paper detected", what + " starts within " + settle + ". Take the paper out to stop it."}
	case s.State == escl.StateStopped || s.State == escl.StateDown:
		return hero{"bad", "Scanner stopped", "It reports “" + s.State + "”. Check the display of the scanner."}
	case s.State != escl.StateIdle:
		return hero{"warn", "Scanner is busy", "It reports “" + s.State + "”: someone is copying, or using its panel."}
	}

	// The scanner is idle. A double-sided document in the making comes
	// first: the user is in the middle of it.
	auto := s.AutoscanEnabled && !s.Paused && s.Armed // a stack put in now is scanned by itself
	loaded := s.AdfState == escl.AdfLoaded
	switch s.DoubleSided {
	case autoscan.DoubleSidedWaiting:
		feeder := "Put the stack into the feeder and press Scan feeder"
		switch {
		case auto:
			feeder = "Put the stack into the feeder"
		case loaded:
			feeder = "Press Scan feeder for the stack in the feeder"
		}
		return hero{"ok", "Ready for a double-sided document", feeder + ", or put the first page on the glass and press Scan flatbed."}
	case autoscan.DoubleSidedBacks:
		switch {
		case auto:
			return hero{"ok", "Ready for the backs", "Turn the stack over and put it back into the feeder. Scanning starts after " + settle + "."}
		case loaded:
			return hero{"ok", "Ready for the backs", "Press Scan feeder to scan the stack in the feeder as the backs."}
		}
		return hero{"ok", "Ready for the backs", "Turn the stack over, put it back into the feeder and press Scan feeder."}
	case autoscan.DoubleSidedFlatbed:
		return hero{"ok", "Ready for the next page", "Put it on the glass and press Scan flatbed, or save the document: " +
			pageCount(s.DoubleSidedHeld) + " so far."}
	}

	switch {
	case !s.AutoscanEnabled:
		return hero{"idle", "Ready", "Autoscan is switched off. Use the buttons below to scan."}
	case s.Paused:
		return hero{"warn", "Autoscan is paused", "Paper in the feeder is not scanned by itself. Use the buttons below, or resume autoscan."}
	case loaded && !s.Armed:
		return hero{"warn", "Paper is waiting in the feeder",
			"It is not scanned by itself: it was already there, or its scan was skipped or failed. Press Scan feeder, or take the stack out and put it back in."}
	case !s.Armed:
		return hero{"idle", "Waiting for an empty feeder", "Autoscan starts again once the feeder has been empty."}
	}
	return hero{"ok", "Ready for paper", "Put a stack into the feeder. Scanning starts after " + settle + "."}
}

func scanSubject(c *autoscan.Current) string {
	switch {
	case c.Pass == autoscan.DoubleSidedWaiting && c.Source == "feeder":
		return "fronts"
	case c.Pass == autoscan.DoubleSidedBacks:
		return "backs"
	}
	return sourceName(c.Source)
}

// document pictures a double-sided document in the making as a row of
// sheets in the order of the finished PDF.
type document struct {
	Sheets []sheet
	// Before and After count sheets left out of a long document.
	Before, After int
}

// sheet is one page of the document in the making.
type sheet struct {
	N    int  // page number in the finished document
	Held bool // scanned and held; otherwise still to come
}

// maxSheets is how many sheets the page draws; a feeder takes 50.
const maxSheets = 16

func newDocument(s autoscan.Snapshot) document {
	var d document
	switch s.DoubleSided {
	case autoscan.DoubleSidedWaiting:
		d.Sheets = []sheet{{N: 1}}
	case autoscan.DoubleSidedBacks:
		// The fronts are the odd pages; the backs will fill the gaps. A
		// long stack shows its beginning: the pattern is what matters.
		total := 2 * s.DoubleSidedHeld
		for n := 1; n <= min(total, maxSheets); n++ {
			d.Sheets = append(d.Sheets, sheet{N: n, Held: n%2 == 1})
		}
		d.After = total - len(d.Sheets)
	case autoscan.DoubleSidedFlatbed:
		// The pages so far and the next one. A long document shows its end:
		// where the user is.
		total := s.DoubleSidedHeld + 1
		d.Before = max(total-maxSheets, 0)
		for n := d.Before + 1; n <= total; n++ {
			d.Sheets = append(d.Sheets, sheet{N: n, Held: n < total})
		}
	}
	return d
}

// sourceName is the word the UI uses for a scan source.
func sourceName(source string) string {
	if source == "platen" {
		return "flatbed"
	}
	return source
}

func faultTitle(adf string) string {
	switch adf {
	case escl.AdfJam:
		return "Paper jam in the feeder"
	case escl.AdfMispick:
		return "The feeder could not pull in the paper"
	case escl.AdfHatchOpen, escl.AdfDoorOpen:
		return "The feeder cover is open"
	case escl.AdfMultipickDetected:
		return "The feeder pulled in several sheets at once"
	case escl.AdfInputTrayOverloaded:
		return "Too much paper in the feeder"
	case escl.AdfDuplexPageTooShort, escl.AdfDuplexPageTooLong:
		return "The paper has the wrong length for the feeder"
	}
	return "The feeder has a problem"
}

// feederLabel puts scan:AdfState into words.
func feederLabel(adf string) string {
	switch adf {
	case "":
		return "unknown"
	case escl.AdfEmpty, escl.AdfHopperEmpty:
		return "empty"
	case escl.AdfLoaded:
		return "paper loaded"
	case escl.AdfProcessing:
		return "feeding"
	case escl.AdfJam:
		return "paper jam"
	}
	return strings.TrimPrefix(adf, "ScannerAdf")
}

// resultLabel puts the result of a scan into words.
func resultLabel(r autoscan.Record) string {
	switch r.Result {
	case metrics.ResultOK:
		if len(r.Files) == 0 {
			return "held"
		}
		return "saved"
	case metrics.ResultIncompletePDF:
		return "incomplete PDF"
	case metrics.ResultBusy:
		return "scanner busy"
	case metrics.ResultError:
		return "failed"
	case metrics.ResultWakeGlitch:
		return "wake glitch"
	}
	return r.Result
}

// span formats a duration the way people say it: "45 s", "3 min".
func span(d time.Duration) string {
	d = max(d, 0)
	if d < 90*time.Second {
		return fmt.Sprintf("%d s", int(d.Round(time.Second).Seconds()))
	}
	return fmt.Sprintf("%d min", int(math.Ceil(d.Minutes())))
}

// minutes formats the time left in a double-sided step. It changes once a
// minute only: the page replaces a part whenever its text changes, and this
// part has a button in it.
func minutes(d time.Duration) string {
	return fmt.Sprintf("%d min", int(math.Ceil(max(d, 0).Minutes())))
}

func pageCount(n int) string { return plural(n, "page") }

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
