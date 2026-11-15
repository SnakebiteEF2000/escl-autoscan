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
		case autoscan.DoubleSidedFronts:
			what = "Scanning the fronts"
		case autoscan.DoubleSidedBacks:
			what = "Scanning the backs"
		}
		return hero{"busy", "Paper detected", what + " starts within " + settle + ". Take the paper out to stop it."}
	case s.State == escl.StateStopped || s.State == escl.StateDown:
		return hero{"bad", "Scanner stopped", "It reports “" + s.State + "”. Check the display of the scanner."}
	case s.State != escl.StateIdle:
		return hero{"warn", "Scanner is busy", "It reports “" + s.State + "”: someone is copying, or using its panel."}
	case !s.AutoscanEnabled:
		return hero{"idle", "Ready", "Autoscan is switched off. Use the buttons below to scan."}
	case s.Paused:
		return hero{"warn", "Autoscan is paused", "Paper in the feeder is not scanned by itself. Use the buttons below, or resume autoscan."}
	case s.AdfState == escl.AdfLoaded && !s.Armed:
		return hero{"warn", "Paper is waiting in the feeder",
			"It is not scanned by itself: it was already there, or its scan was skipped or failed. Press Scan feeder, or take the stack out and put it back in."}
	case !s.Armed:
		return hero{"idle", "Waiting for an empty feeder", "Autoscan starts again once the feeder has been empty."}
	case s.DoubleSided == autoscan.DoubleSidedFronts:
		return hero{"ok", "Ready for the fronts", "Put the stack into the feeder. Scanning starts after " + settle + "."}
	case s.DoubleSided == autoscan.DoubleSidedBacks:
		return hero{"ok", "Ready for the backs", "Turn the stack over and put it back into the feeder. Scanning starts after " + settle + "."}
	}
	return hero{"ok", "Ready for paper", "Put a stack into the feeder. Scanning starts after " + settle + "."}
}

func scanSubject(c *autoscan.Current) string {
	switch {
	case c.Pass == autoscan.DoubleSidedFronts:
		return "fronts"
	case c.Pass == autoscan.DoubleSidedBacks:
		return "backs"
	}
	return sourceName(c.Source)
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

func pageCount(n int) string {
	if n == 1 {
		return "1 page"
	}
	return fmt.Sprintf("%d pages", n)
}
