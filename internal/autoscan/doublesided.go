package autoscan

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/collate"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
)

// Double-sided mode turns two feeder scans of the same stack, the fronts and
// then the backs with the stack turned over, into one PDF in OUTPUT_DIR. The
// fronts are held in memory until the backs arrive.
//
//	off --start--> fronts --feeder ok--> backs --feeder ok--> off (one PDF delivered)
//
// A failed pass keeps the state, so the pass can be repeated. Cancelling
// discards held fronts; a timeout or a shutdown saves them as a normal
// single-sided scan. Flatbed scans never take part.
//
// The role of a feeder scan (Current.Pass) is fixed when the scan starts, and
// while such a pass runs neither cancel nor the timeout changes the state.
// So the scan goroutine finds the state it started with.

// Double-sided states, also the value of escl_autoscan_double_sided_state.
const (
	DoubleSidedOff    = 0
	DoubleSidedFronts = 1 // waiting for the fronts
	DoubleSidedBacks  = 2 // fronts held, waiting for the backs
)

// Note kinds.
const (
	NoteOK   = "ok"   // a double-sided document was delivered
	NoteInfo = "info" // the mode ended as asked
	NoteWarn = "warn" // something needs the user's attention
)

// Note is the last double-sided event, shown in the UI until the next feeder
// scan starts.
type Note struct {
	Kind string // NoteOK, NoteInfo or NoteWarn; empty: no note
	Text string
}

// frontsPass is the first pass of a double-sided document.
type frontsPass struct {
	docs  [][]byte
	pages int
	at    time.Time // scan time; names the file if the fronts are saved alone
}

// StartDoubleSided starts (start=true) or cancels double-sided mode.
// Cancelling discards held fronts. Both return ErrBusy while a feeder scan
// runs: its role is already fixed.
func (s *Service) StartDoubleSided(start bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scanning != nil && s.scanning.Source == "feeder" {
		return ErrBusy
	}
	switch {
	case start && s.ds == DoubleSidedOff:
		s.setDoubleSided(DoubleSidedFronts, Note{})
		s.log.Info("double-sided mode started", "timeout", s.cfg.DoubleSidedTimeout)
	case !start && s.ds != DoubleSidedOff:
		note := Note{NoteInfo, "Double-sided cancelled."}
		if s.dsFronts != nil {
			note.Text = fmt.Sprintf("Double-sided cancelled. The %s held were discarded.", plural(s.dsFronts.pages, "front"))
		}
		s.log.Info("double-sided mode cancelled", "state", s.ds)
		s.endDoubleSided(metrics.DoubleSidedCancelled, note)
	}
	return nil
}

// setDoubleSided changes the state and restarts the timeout; s.mu must be
// held.
func (s *Service) setDoubleSided(state int, note Note) {
	s.ds, s.dsNote = state, note
	s.dsDeadline = time.Time{}
	if state != DoubleSidedOff {
		s.dsDeadline = s.now().Add(s.cfg.DoubleSidedTimeout)
	}
	s.m.SetDoubleSidedState(state)
}

// endDoubleSided switches the mode off and drops held fronts; s.mu must be
// held.
func (s *Service) endDoubleSided(result string, note Note) {
	s.dsFronts = nil
	s.setDoubleSided(DoubleSidedOff, note)
	s.m.DoubleSided(result)
}

// expireDoubleSided ends the mode once its deadline has passed. Held fronts
// are saved as a normal scan: the user may have walked away believing the
// document was scanned. Only the Run goroutine calls it.
func (s *Service) expireDoubleSided() {
	s.mu.Lock()
	if s.ds == DoubleSidedOff || s.now().Before(s.dsDeadline) || (s.scanning != nil && s.scanning.Pass != DoubleSidedOff) {
		s.mu.Unlock()
		return
	}
	fronts := s.dsFronts
	s.log.Warn("double-sided mode timed out", "state", s.ds)
	note := Note{NoteWarn, "Double-sided timed out before the fronts were scanned. The next scan is a normal one."}
	if fronts != nil {
		note.Text = "Double-sided timed out waiting for the backs. Saving the fronts as a single-sided scan."
	}
	// End the mode before writing, so the next scan is a normal one.
	s.endDoubleSided(metrics.DoubleSidedTimeout, note)
	s.mu.Unlock()
	if fronts == nil {
		return
	}

	files, err := s.saveFronts(fronts)
	note.Text = "Double-sided timed out waiting for the backs. The fronts were saved as a single-sided scan: " + strings.Join(files, ", ") + "."
	if err != nil {
		note.Text = "Double-sided timed out waiting for the backs, and the fronts could not be saved: " + err.Error() + "."
	}
	s.mu.Lock()
	// Unless a scan or a new double-sided run has replaced the note.
	if s.ds == DoubleSidedOff && s.dsNote.Kind == NoteWarn {
		s.dsNote = note
	}
	s.mu.Unlock()
}

// saveFrontsAtShutdown saves held fronts before the process exits.
func (s *Service) saveFrontsAtShutdown() {
	s.mu.Lock()
	fronts := s.dsFronts
	s.dsFronts = nil
	s.setDoubleSided(DoubleSidedOff, Note{})
	s.mu.Unlock()
	if fronts != nil {
		_, _ = s.saveFronts(fronts)
	}
}

// saveFronts writes held fronts to OUTPUT_DIR as a normal scan, or to
// FAILED_DIR if that fails.
func (s *Service) saveFronts(f *frontsPass) ([]string, error) {
	files, err := s.write(s.out, f.docs, f.at, "")
	if err == nil {
		s.log.Info("double-sided fronts saved as a single-sided scan", "files", files, "pages", f.pages)
		return files, nil
	}
	s.log.Error("cannot save the double-sided fronts", "dir", s.out.Dir, "err", err)
	if s.failed != nil {
		if kept, kerr := s.write(s.failed, f.docs, f.at, "fronts"); kerr == nil {
			s.log.Warn("double-sided fronts kept in the failed directory", "files", kept)
		}
	}
	return nil, err
}

// write writes docs with w and returns the file names written so far.
func (s *Service) write(w *output.Writer, docs [][]byte, at time.Time, suffix string) ([]string, error) {
	var files []string
	for _, doc := range docs {
		name, err := w.Write(doc, at, suffix)
		if err != nil {
			return files, err
		}
		files = append(files, name)
	}
	return files, nil
}

// passFailed tells the user how to go on after a failed double-sided pass.
// The mode stays on, with the full timeout again.
func (s *Service) passFailed(pass int, rec *Record) {
	if pass == DoubleSidedOff {
		return
	}
	text := fmt.Sprintf("The scan of the fronts failed (%s). Put the stack back into the feeder to scan the fronts again.", rec.Result)
	if pass == DoubleSidedBacks {
		text = fmt.Sprintf("The scan of the backs failed (%s). The fronts are still held: put the turned-over stack back into the feeder to scan the backs again.", rec.Result)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ds == pass {
		s.setDoubleSided(pass, Note{NoteWarn, text})
	}
}

// holdFronts keeps a successful fronts pass in memory until the backs
// arrive. If pdfcpu cannot read the scan, collating would fail later; then
// the scan is delivered as a normal one right away and the mode ends.
func (s *Service) holdFronts(rec *Record, docs [][]byte) {
	pages, err := collate.Pages(docs)
	if err != nil {
		s.log.Warn("double-sided: cannot read the scanned PDF, delivering it as a normal scan", "err", err)
		s.deliver(rec, docs)
		rec.Message = joinMsg(rec.Message, "double-sided not possible: "+err.Error())
		s.mu.Lock()
		s.endDoubleSided(metrics.DoubleSidedFailed, Note{NoteWarn,
			"Double-sided is not possible: the scanned PDF cannot be read (" + err.Error() + "). The scan was saved as a normal single-sided scan."})
		s.mu.Unlock()
		return
	}
	rec.DoubleSided = true
	rec.Message = joinMsg(rec.Message, "fronts held, waiting for the backs")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dsFronts = &frontsPass{docs: docs, pages: pages, at: rec.Time}
	s.setDoubleSided(DoubleSidedBacks, Note{})
	s.log.Info("double-sided fronts held, waiting for the backs", "pages", pages, "timeout", s.cfg.DoubleSidedTimeout)
}

// collateBacks merges a successful backs pass with the held fronts and
// delivers one PDF.
func (s *Service) collateBacks(rec *Record, docs [][]byte) {
	s.mu.Lock()
	fronts := s.dsFronts
	s.mu.Unlock()
	if fronts == nil { // only when a shutdown has already saved them
		s.deliver(rec, docs)
		return
	}

	pdf, pages, err := collate.Interleave(fronts.docs, docs)
	if err == nil {
		// Check the result like a scan, and count its pages a second way.
		var n int
		if n, err = output.ValidatePDF(pdf); err == nil && n != pages {
			err = fmt.Errorf("the collated PDF has %d pages instead of %d", n, pages)
		}
	}
	var mismatch *collate.PageCountError
	switch {
	case errors.As(err, &mismatch):
		// Not delivered: one of the passes is wrong. The fronts stay, so
		// the backs can be scanned again.
		rec.Result, rec.Message = metrics.ResultError, joinMsg(rec.Message, "double-sided: "+mismatch.Error())
		s.mu.Lock()
		s.setDoubleSided(DoubleSidedBacks, Note{NoteWarn, fmt.Sprintf(
			"The fronts have %s but the backs have %s, so the feeder probably pulled two sheets at once or skipped one. "+
				"The fronts are still held: put the turned-over stack back into the feeder to scan the backs again, or cancel and start over.",
			plural(mismatch.Fronts, "page"), plural(mismatch.Backs, "page"))})
		s.mu.Unlock()
		return
	case err != nil:
		// Never lose a scan because of the PDF library: deliver both passes
		// as they came from the scanner.
		s.log.Error("double-sided: cannot collate, delivering fronts and backs as separate scans", "err", err)
		files, werr := s.saveFronts(fronts)
		rec.Files = files
		if werr == nil {
			s.deliver(rec, docs)
		} else {
			rec.Result, rec.Message = metrics.ResultError, joinMsg(rec.Message, "write: "+werr.Error())
		}
		note := "Fronts and backs could not be combined (" + err.Error() + "). They were saved as separate single-sided scans: " + strings.Join(rec.Files, ", ") + "."
		if rec.Result != metrics.ResultOK {
			note = "Fronts and backs could not be combined (" + err.Error() + "), and saving them separately failed: " + rec.Message + "."
		}
		rec.Message = joinMsg(rec.Message, "double-sided: could not collate: "+err.Error())
		s.mu.Lock()
		s.endDoubleSided(metrics.DoubleSidedFailed, Note{NoteWarn, note})
		s.mu.Unlock()
		return
	}

	name, err := s.out.Write(pdf, rec.Time, "")
	if err != nil {
		rec.Result, rec.Message = metrics.ResultError, joinMsg(rec.Message, "write: "+err.Error())
		s.mu.Lock()
		s.setDoubleSided(DoubleSidedBacks, Note{NoteWarn,
			"The double-sided document could not be saved (" + err.Error() + "). The fronts are still held: scan the backs again."})
		s.mu.Unlock()
		return
	}
	rec.Files, rec.DoubleSided = []string{name}, true
	rec.Message = joinMsg(rec.Message, fmt.Sprintf("collated with the fronts: %d pages", pages))
	s.log.Info("double-sided document delivered", "file", name, "pages", pages)
	s.mu.Lock()
	s.endDoubleSided(metrics.DoubleSidedDelivered, Note{NoteOK, fmt.Sprintf("Double-sided document saved: %s (%d pages).", name, pages)})
	s.mu.Unlock()
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
