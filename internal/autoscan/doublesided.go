package autoscan

import (
	"errors"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
)

// Double-sided mode routes the next two feeder scans (fronts, then backs)
// into OUTPUT_DIR/<DoubleSidedSubdir>. Paperless-ngx collates two files from
// a folder of that name: it parks the first, reverses the pages of the
// second and interleaves them. The state lives in memory only.
//
//	off --start--> fronts --feeder ok--> backs --feeder ok--> off (delivered)
//	fronts/backs --timeout--> off;  --feeder failed--> off;  --cancel--> off
//
// Flatbed scans never take part.

// Double-sided states, also the value of escl_autoscan_double_sided_state.
const (
	DoubleSidedOff    = 0
	DoubleSidedFronts = 1
	DoubleSidedBacks  = 2
)

// ErrDoubleSidedDisabled is returned when DoubleSidedSubdir is empty.
var ErrDoubleSidedDisabled = errors.New("double-sided mode is disabled")

// frontsParkedNote explains what happens to fronts Paperless already parked.
const frontsParkedNote = "Paperless keeps the delivered fronts for up to 30 minutes and pairs them with the next " +
	"double-sided file. Wait until then before scanning the fronts again, or start double-sided again and scan only the backs."

// StartDoubleSided arms (start=true) or cancels double-sided mode.
func (s *Service) StartDoubleSided(start bool) error {
	if s.dsOut == nil {
		return ErrDoubleSidedDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireDoubleSided()
	if start {
		if s.ds != DoubleSidedOff {
			return nil
		}
		s.setDoubleSided(DoubleSidedFronts, "")
		s.dsDeadline = s.now().Add(s.cfg.DoubleSidedTimeout)
		s.log.Info("double-sided mode started", "timeout", s.cfg.DoubleSidedTimeout)
		return nil
	}
	if s.ds == DoubleSidedOff {
		return nil
	}
	note := ""
	if s.ds == DoubleSidedBacks {
		note = "Double-sided cancelled after the fronts. " + frontsParkedNote
	}
	s.log.Info("double-sided mode cancelled", "state", s.ds)
	s.endDoubleSided(metrics.DoubleSidedCancelled, note)
	return nil
}

// setDoubleSided changes the state; s.mu must be held.
func (s *Service) setDoubleSided(state int, note string) {
	s.ds, s.dsNote = state, note
	s.m.SetDoubleSidedState(state)
}

func (s *Service) endDoubleSided(result, note string) {
	s.setDoubleSided(DoubleSidedOff, note)
	s.dsDeadline = time.Time{}
	s.m.DoubleSided(result)
}

// expireDoubleSided ends the mode once its deadline has passed; s.mu must
// be held.
func (s *Service) expireDoubleSided() {
	if s.ds == DoubleSidedOff || s.now().Before(s.dsDeadline) {
		return
	}
	note := "Double-sided timed out; the next scan goes to the normal output."
	if s.ds == DoubleSidedBacks {
		note = "Double-sided timed out waiting for the backs."
	}
	s.log.Warn("double-sided mode timed out", "state", s.ds)
	s.endDoubleSided(metrics.DoubleSidedTimeout, note)
}

// deliveryTarget returns the writer for a successful scan with n documents.
// A feeder job with several documents cannot be one pass of a pair, so it
// goes to the normal output and ends the mode.
func (s *Service) deliveryTarget(source escl.InputSource, n int) (*output.Writer, bool) {
	if source != escl.Feeder || s.dsOut == nil {
		return s.out, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireDoubleSided()
	if s.ds == DoubleSidedOff {
		return s.out, false
	}
	if n > 1 {
		s.log.Warn("double-sided: the scanner returned several documents for one pass; writing them to the normal output and cancelling",
			"documents", n)
		s.failDoubleSided("the scanner returned several documents for one pass")
		return s.out, false
	}
	return s.dsOut, true
}

// doubleSidedDone records the outcome of a feeder scan that was routed (or
// would have been routed) to the double-sided folder.
func (s *Service) doubleSidedDone(source escl.InputSource, result string, routed bool) {
	if source != escl.Feeder || s.dsOut == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.ds == DoubleSidedOff:
	case result == metrics.ResultOK && routed && s.ds == DoubleSidedFronts:
		s.setDoubleSided(DoubleSidedBacks, "")
		s.dsDeadline = s.now().Add(s.cfg.DoubleSidedTimeout)
		s.log.Info("double-sided fronts delivered, waiting for the backs", "timeout", s.cfg.DoubleSidedTimeout)
	case result == metrics.ResultOK && routed:
		s.log.Info("double-sided pair delivered")
		s.endDoubleSided(metrics.DoubleSidedDelivered, "")
	case result == metrics.ResultBusy || result == metrics.ResultSkipped:
		// Nothing was scanned; the paper is still waiting.
	default:
		s.failDoubleSided("the scan failed (" + result + ")")
	}
}

// failDoubleSided cancels the mode after a failed pass; s.mu must be held.
func (s *Service) failDoubleSided(why string) {
	note := "Double-sided cancelled: " + why + ". Start double-sided again and scan the fronts again."
	if s.ds == DoubleSidedBacks {
		note = "Double-sided cancelled: " + why + " while scanning the backs. " + frontsParkedNote
	}
	s.log.Warn("double-sided mode cancelled", "reason", why, "state", s.ds)
	s.endDoubleSided(metrics.DoubleSidedFailed, note)
}
