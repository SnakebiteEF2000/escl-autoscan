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

// Double-sided mode makes one PDF in OUTPUT_DIR out of several scans. It is
// switched on without saying how; the first scan decides:
//
// A feeder scan makes it a stack scanned twice by a feeder that scans one
// side only: the fronts, then the backs with the stack turned over.
//
//	off --on--> waiting --feeder ok--> backs --feeder ok--> off (one PDF delivered)
//
// A flatbed scan makes it a document scanned page after page, both sides of
// a sheet in reading order, until the user saves it.
//
//	off --on--> waiting --flatbed ok--> flatbed --flatbed ok--> flatbed ... --save--> off
//
// What is scanned so far is held in memory. A failed scan keeps the state,
// so it can be repeated. Discarding is its own action; switching the mode
// off is refused while something is held. A timeout or a shutdown saves what
// is held (fronts as a normal single-sided scan, flatbed pages as one
// document).
//
// A scan that passed validation is never thrown away because of this mode:
// it is held, collated or delivered as a normal scan.
//
// The role of a scan (Current.Pass) is fixed when the scan starts, and while
// such a pass runs neither the user nor the timeout changes the state. So
// the scan goroutine finds the state it started with. Once the first scan
// has decided, scans from the other source do not take part and are
// delivered as usual.

// Double-sided states, also the value of escl_autoscan_double_sided_state.
const (
	DoubleSidedOff     = 0
	DoubleSidedWaiting = 1 // on, nothing scanned yet: the first scan decides
	DoubleSidedBacks   = 2 // feeder: fronts held, waiting for the backs
	DoubleSidedFlatbed = 3 // flatbed: collecting pages
)

// ErrPagesHeld is returned when double-sided mode is to be switched off
// while it holds scans. They have to be saved or discarded first.
var ErrPagesHeld = errors.New("scans are held for a double-sided document")

// Note kinds.
const (
	NoteOK   = "ok"   // a document was delivered
	NoteInfo = "info" // the mode ended as asked
	NoteWarn = "warn" // something needs the user's attention
)

// Note is the last double-sided event, shown in the UI until the next scan
// that takes part starts.
type Note struct {
	Kind string // NoteOK, NoteInfo or NoteWarn; empty: no note
	Text string
}

// heldPages is what a run has scanned so far: the fronts of a feeder run, or
// the pages of a flatbed document.
type heldPages struct {
	docs  [][]byte
	pages int
	at    time.Time // time of the last scan; names the file
	// color is the color mode of the last scan. The next scan of the run
	// uses it unless the user chooses another: one document, one look.
	color string
}

// StartDoubleSided switches double-sided mode on. It returns ErrBusy while a
// scan runs, because the role of that scan is already fixed.
func (s *Service) StartDoubleSided() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.ds != DoubleSidedOff:
		return nil
	case s.scanning != nil:
		return ErrBusy
	}
	s.setDoubleSided(DoubleSidedWaiting, Note{})
	s.log.Info("double-sided mode started", "timeout", s.cfg.DoubleSidedTimeout)
	return nil
}

// CancelDoubleSided switches double-sided mode off. With discard it throws
// away what is held; without, it returns ErrPagesHeld if anything is held,
// so that "off" never loses a scan. It returns ErrBusy while a scan that
// takes part runs.
func (s *Service) CancelDoubleSided(discard bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.ds == DoubleSidedOff:
		return nil
	case s.passRunning():
		return ErrBusy
	case s.dsHeld != nil && !discard:
		return ErrPagesHeld
	}
	note := Note{NoteInfo, "Double-sided switched off."}
	if s.dsHeld != nil {
		what := plural(s.dsHeld.pages, "front")
		if s.ds == DoubleSidedFlatbed {
			what = plural(s.dsHeld.pages, "page")
		}
		note.Text = "Double-sided switched off. The " + what + " held were discarded."
	}
	s.log.Info("double-sided mode cancelled", "state", s.ds)
	s.endDoubleSided(metrics.DoubleSidedCancelled, note)
	return nil
}

// FinishDoubleSided saves the pages of a flatbed document as one PDF and
// ends the run. It returns ErrBusy while a page is being scanned, and the
// error if the document cannot be written; the pages stay held then.
func (s *Service) FinishDoubleSided() error {
	s.mu.Lock()
	switch {
	case s.ds != DoubleSidedFlatbed:
		s.mu.Unlock()
		return nil
	case s.passRunning():
		s.mu.Unlock()
		return ErrBusy
	case s.dsHeld == nil:
		s.endDoubleSided(metrics.DoubleSidedCancelled, Note{NoteInfo, "The flatbed document was ended without a page."})
		s.mu.Unlock()
		return nil
	}
	state, held, placeholder := s.take("Saving the flatbed document.")
	s.mu.Unlock()
	return s.save(state, held, placeholder, metrics.DoubleSidedDelivered, "")
}

// passRunning reports whether a scan that takes part in the run is running;
// s.mu must be held.
func (s *Service) passRunning() bool {
	return s.scanning != nil && s.scanning.Pass != DoubleSidedOff
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

// endDoubleSided switches the mode off and drops what is held; s.mu must be
// held.
func (s *Service) endDoubleSided(result string, note Note) {
	s.dsHeld = nil
	s.setDoubleSided(DoubleSidedOff, note)
	s.m.DoubleSided(result)
}

// take switches the mode off and hands out what was held, to be written
// without the lock by save; s.mu must be held. The mode is off first, so a
// scan that starts meanwhile is a normal one and is not added to pages that
// are being written.
func (s *Service) take(text string) (state int, held *heldPages, placeholder Note) {
	state, held, placeholder = s.ds, s.dsHeld, Note{NoteInfo, text}
	s.dsHeld = nil
	s.setDoubleSided(DoubleSidedOff, placeholder)
	return state, held, placeholder
}

// save writes pages handed out by take and settles the outcome. If they
// cannot be written they are not given up: they are held again, with the
// full timeout, and the error is returned. why says what ended the run when
// it was not the user ("... timed out ...").
func (s *Service) save(state int, held *heldPages, placeholder Note, result, why string) error {
	files, joinErr, err := s.writeHeld(state, held)

	what, source := "fronts", "feeder"
	if state == DoubleSidedFlatbed {
		what, source = "pages", "platen"
	}
	s.mu.Lock()
	switch {
	case err == nil:
	case s.ds == DoubleSidedOff:
		s.log.Error("cannot save the held "+what+", holding them again", "dir", s.out.Dir, "err", err)
		s.dsHeld = held
		s.setDoubleSided(state, Note{NoteWarn, why + "The " + what + " could not be saved (" + err.Error() + "). They are still held: check the output folder."})
		s.mu.Unlock()
		return err
	default:
		// A new run has started meanwhile and holds its own pages.
		s.log.Error("cannot save the held "+what, "dir", s.out.Dir, "err", err)
		s.m.DoubleSided(result)
		s.mu.Unlock()
		s.keepFailed(held) // writes: not under the lock
		return err
	}

	note := Note{NoteWarn, why + "The fronts were saved as a single-sided scan: " + strings.Join(files, ", ") + "."}
	message := "fronts of a double-sided scan, saved single-sided"
	switch {
	case state != DoubleSidedFlatbed:
	case joinErr != nil:
		result = metrics.DoubleSidedFailed
		note.Text = why + "The pages could not be joined (" + joinErr.Error() + "). They were saved as separate scans: " + strings.Join(files, ", ") + "."
		message = "flatbed document, pages not joined: " + joinErr.Error()
	default:
		note.Text = fmt.Sprintf("%sFlatbed document saved: %s (%s).", why, strings.Join(files, ", "), plural(held.pages, "page"))
		message = "flatbed document"
		if why == "" {
			note.Kind = NoteOK
		}
	}
	s.log.Info("double-sided: held "+what+" saved", "files", files, "pages", held.pages)
	s.m.DoubleSided(result)
	// Unless a scan or a new run has replaced the note meanwhile.
	if s.ds == DoubleSidedOff && s.dsNote == placeholder {
		s.dsNote = note
	}
	s.mu.Unlock()
	// The file is not the result of one scan: show it in the history, but
	// do not count a scan.
	trigger := "document"
	if why != "" {
		trigger = "timeout"
	}
	s.remember(Record{Time: s.now(), Source: source, Trigger: trigger, Pages: held.pages, Result: metrics.ResultOK, Files: files, Message: message})
	return nil
}

// writeHeld writes held pages to OUTPUT_DIR: fronts as they are, the pages
// of a flatbed document joined into one PDF. If joining fails (joinErr), the
// pages are written as they came from the scanner, one file per scan.
func (s *Service) writeHeld(state int, held *heldPages) (files []string, joinErr, err error) {
	if state == DoubleSidedFlatbed && len(held.docs) > 1 {
		pdf, pages, jerr := collate.Join(held.docs)
		if jerr == nil {
			jerr = verify(pdf, pages)
		}
		if jerr == nil {
			name, err := s.out.Write(pdf, held.at, "")
			if err != nil {
				return nil, nil, err
			}
			return []string{name}, nil, nil
		}
		s.log.Error("double-sided: cannot join the flatbed pages, saving them as separate scans", "err", jerr)
		joinErr = jerr
	}
	files, err = s.write(s.out, held.docs, held.at, "")
	return files, joinErr, err
}

// verify checks a PDF made by pdfcpu like a scan, and counts its pages a
// second way.
func verify(pdf []byte, pages int) error {
	n, err := output.ValidatePDF(pdf)
	if err == nil && n != pages {
		err = fmt.Errorf("the PDF has %d pages instead of %d", n, pages)
	}
	return err
}

// expireDoubleSided ends the run once its deadline has passed. What is held
// is saved: the user may have walked away believing the document was
// scanned. Only the Run goroutine calls it.
//
// It waits while a pass runs or, for the feeder, is about to (paper
// detected, settle time running): that pass still belongs to the run.
func (s *Service) expireDoubleSided() {
	s.mu.Lock()
	if s.ds == DoubleSidedOff || s.now().Before(s.dsDeadline) || s.passRunning() ||
		(s.settling && s.ds != DoubleSidedFlatbed) {
		s.mu.Unlock()
		return
	}
	s.log.Warn("double-sided mode timed out", "state", s.ds)
	why := "Double-sided timed out waiting for the backs. "
	if s.ds == DoubleSidedFlatbed {
		why = "The flatbed document timed out waiting for the next page. "
	}
	if s.dsHeld == nil {
		s.endDoubleSided(metrics.DoubleSidedTimeout, Note{NoteWarn, "Double-sided timed out before anything was scanned. The next scan is a normal one."})
		s.mu.Unlock()
		return
	}
	state, held, placeholder := s.take(why + "Saving what was scanned.")
	s.mu.Unlock()
	_ = s.save(state, held, placeholder, metrics.DoubleSidedTimeout, why)
}

// saveHeldAtShutdown saves what is held before the process exits. If a pass
// is still running then (it outlived Shutdown's patience), fronts may end up
// in its collated PDF as well: twice is better than never.
func (s *Service) saveHeldAtShutdown() {
	s.mu.Lock()
	state, held, _ := s.take("")
	s.dsNote = Note{}
	s.mu.Unlock()
	if held == nil {
		return
	}
	files, _, err := s.writeHeld(state, held)
	if err != nil {
		s.log.Error("cannot save the held pages", "dir", s.out.Dir, "err", err)
		s.keepFailed(held)
		return
	}
	s.log.Info("double-sided: held pages saved at shutdown", "files", files, "pages", held.pages)
}

// keepFailed puts pages that can neither be delivered nor held any longer
// into FAILED_DIR, if there is one.
func (s *Service) keepFailed(held *heldPages) {
	if s.failed == nil {
		s.log.Error("double-sided: held pages lost, no FAILED_DIR to keep them in", "pages", held.pages)
		return
	}
	kept, err := s.write(s.failed, held.docs, held.at, "held")
	if err != nil {
		s.log.Error("double-sided: held pages lost, cannot keep them in the failed directory", "dir", s.failed.Dir, "err", err)
		return
	}
	s.log.Warn("double-sided: held pages kept in the failed directory", "files", kept)
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

// passFailed tells the user how to go on after a failed pass. The run stays
// on, with the full timeout again.
func (s *Service) passFailed(pass int, rec *Record) {
	if pass == DoubleSidedOff {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ds != pass {
		return
	}
	var text string
	switch {
	case pass == DoubleSidedBacks:
		text = fmt.Sprintf("The scan of the backs failed (%s). The fronts are still held: put the turned-over stack back into the feeder to scan the backs again.", rec.Result)
	case rec.Source == "feeder":
		text = fmt.Sprintf("The scan of the fronts failed (%s). Put the stack back into the feeder to scan the fronts again.", rec.Result)
	default:
		text = fmt.Sprintf("The scan failed (%s). Scan this page again.", rec.Result)
		if s.dsHeld != nil {
			text += " The " + plural(s.dsHeld.pages, "page") + " scanned before are still held."
		}
	}
	s.setDoubleSided(pass, Note{NoteWarn, text})
}

// holdPage adds a successful flatbed scan to the document. The first one
// makes the run a flatbed document.
func (s *Service) holdPage(rec *Record, docs [][]byte) {
	s.mu.Lock()
	if s.ds != DoubleSidedFlatbed && s.ds != DoubleSidedWaiting { // only when a shutdown has ended the run
		s.mu.Unlock()
		s.deliver(rec, docs)
		return
	}
	if s.dsHeld == nil {
		s.dsHeld = &heldPages{}
	}
	s.dsHeld.docs = append(s.dsHeld.docs, docs...)
	s.dsHeld.pages += rec.Pages
	s.dsHeld.at, s.dsHeld.color = rec.Time, rec.Color
	pages := s.dsHeld.pages
	s.setDoubleSided(DoubleSidedFlatbed, Note{})
	s.mu.Unlock()
	rec.DoubleSided = true
	rec.Message = joinMsg(rec.Message, fmt.Sprintf("held for a flatbed document, %s so far", plural(pages, "page")))
	s.log.Info("double-sided: flatbed page held", "pages", pages, "timeout", s.cfg.DoubleSidedTimeout)
}

// holdFronts keeps a successful fronts pass in memory until the backs
// arrive; it makes the run a feeder run. If pdfcpu cannot read the scan, collating would fail later; then
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
	s.dsHeld = &heldPages{docs: docs, pages: pages, at: rec.Time, color: rec.Color}
	s.setDoubleSided(DoubleSidedBacks, Note{})
	s.log.Info("double-sided fronts held, waiting for the backs", "pages", pages, "timeout", s.cfg.DoubleSidedTimeout)
}

// collateBacks merges a successful backs pass with the held fronts and
// delivers one PDF.
func (s *Service) collateBacks(rec *Record, docs [][]byte) {
	s.mu.Lock()
	fronts := s.dsHeld
	s.mu.Unlock()
	if fronts == nil { // only when a shutdown has already saved them
		s.deliver(rec, docs)
		return
	}

	pdf, pages, err := collate.Interleave(fronts.docs, docs)
	if err == nil {
		err = verify(pdf, pages)
	}
	var mismatch *collate.PageCountError
	switch {
	case errors.As(err, &mismatch):
		// This scan is not the other side of the held fronts: the feeder
		// pulled two sheets at once, or somebody scanned something else in
		// between. It is a valid scan either way, so it is delivered as a
		// normal one. The fronts stay, and the backs can be scanned again.
		s.deliver(rec, docs)
		saved := "It was saved as a normal scan (" + strings.Join(rec.Files, ", ") + "); delete that in Paperless if it was meant to be the backs."
		if rec.Result != metrics.ResultOK {
			saved = "It could not be saved as a normal scan either."
		}
		rec.Message = joinMsg(rec.Message, "double-sided: "+mismatch.Error()+", not collated")
		s.mu.Lock()
		s.setDoubleSided(DoubleSidedBacks, Note{NoteWarn, fmt.Sprintf(
			"The fronts have %s but this scan had %s, so it cannot be their backs: the feeder probably pulled two sheets at once. %s "+
				"The fronts are still held: put the turned-over stack back into the feeder to scan the backs again, or cancel and start over.",
			plural(mismatch.Fronts, "page"), plural(mismatch.Backs, "page"), saved)})
		s.mu.Unlock()
		return
	case err != nil:
		// Never lose a scan because of the PDF library: deliver both passes
		// as they came from the scanner.
		s.log.Error("double-sided: cannot collate, delivering fronts and backs as separate scans", "err", err)
		files, werr := s.write(s.out, fronts.docs, fronts.at, "")
		if werr != nil {
			// The fronts stay held; this scan is rejected like any scan
			// that cannot be written.
			rec.Result, rec.Message = metrics.ResultError, joinMsg(rec.Message, "write: "+werr.Error())
			s.mu.Lock()
			s.setDoubleSided(DoubleSidedBacks, Note{NoteWarn,
				"Fronts and backs could not be combined (" + err.Error() + "), and saving them separately failed (" + werr.Error() + "). " +
					"The fronts are still held: check the output folder and scan the backs again."})
			s.mu.Unlock()
			return
		}
		rec.Files = files
		s.deliver(rec, docs)
		note := "Fronts and backs could not be combined (" + err.Error() + "). They were saved as separate single-sided scans: " + strings.Join(rec.Files, ", ") + "."
		if rec.Result != metrics.ResultOK {
			note = "Fronts and backs could not be combined (" + err.Error() + "). The fronts were saved as a single-sided scan (" +
				strings.Join(files, ", ") + "), but the backs could not be saved: scan them again."
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
