package autoscan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
)

// scan runs one scan job end to end and delivers or rejects the result.
func (s *Service) scan(ctx context.Context, source escl.InputSource, colorMode string) Record {
	rec := Record{Time: s.now(), Source: sourceLabel(source), Color: colorMode}
	settings := escl.ScanSettings{
		Source: source, Intent: s.cfg.Intent, ColorMode: colorMode,
		Resolution: s.cfg.Resolution, Region: s.cfg.Region,
	}

	docs, job, err := s.runJob(ctx, settings)
	if source == escl.Feeder && err == nil && len(docs) == 0 && s.paperStillLoaded(ctx) {
		// Epson wake bug (sane-airscan #398): after waking from standby a
		// feeder job ends with 404 and no document although paper is loaded.
		s.log.Warn("wake glitch: feeder job returned no document while paper is loaded",
			"workaround", s.cfg.WakeWorkaround)
		if !s.cfg.WakeWorkaround {
			rec.Result, rec.Message = metrics.ResultWakeGlitch, "no document although paper is loaded (scanner wake bug)"
			rec.Duration = s.now().Sub(rec.Time)
			return rec
		}
		s.m.Scan(rec.Source, metrics.ResultWakeGlitch, 0, 0, rec.Time)
		s.deleteJob(job)
		if werr := s.wakeScan(ctx); werr != nil {
			s.log.Warn("wake workaround platen scan failed", "err", werr)
		}
		docs, job, err = s.runJob(ctx, settings)
		rec.Message = "retried after wake glitch"
	}
	rec.Duration = s.now().Sub(rec.Time)

	result, msg := s.classify(ctx, source, job, docs, err)
	if job != "" {
		// Only now: deleting first could change how the device lists the job.
		s.deleteJob(job)
	}
	pages := 0
	if result == metrics.ResultOK {
		pages, result, msg = validateAll(docs)
	}
	rec.Result, rec.Pages = result, pages
	if msg != "" {
		rec.Message = joinMsg(rec.Message, msg)
	}

	if result == metrics.ResultOK {
		for _, doc := range docs {
			name, err := s.out.Write(doc, rec.Time, "")
			if err != nil {
				rec.Result, rec.Message = metrics.ResultError, joinMsg(rec.Message, "write: "+err.Error())
				break
			}
			rec.Files = append(rec.Files, name)
		}
		if rec.Result == metrics.ResultOK {
			return rec
		}
	}
	s.reject(&rec, docs)
	return rec
}

// runJob creates a job and fetches its documents. It returns the documents
// received even when err is not nil. The caller deletes the job.
func (s *Service) runJob(ctx context.Context, settings escl.ScanSettings) ([][]byte, string, error) {
	job, err := s.createJob(ctx, settings)
	if err != nil {
		return nil, "", err
	}
	s.log.Debug("scan job created", "job", job, "source", settings.Source, "color", settings.ColorMode)
	docs, err := s.dev.Documents(ctx, job)
	return docs, job, err
}

// createJob posts the job, retrying with backoff while the device is busy.
func (s *Service) createJob(ctx context.Context, settings escl.ScanSettings) (string, error) {
	wait := s.cfg.BusyBackoff
	for attempt := 1; ; attempt++ {
		job, err := s.dev.CreateJob(ctx, settings)
		if err == nil || !errors.Is(err, escl.ErrBusy) || attempt > s.cfg.BusyRetries {
			return job, err
		}
		st, serr := s.dev.Status(ctx)
		if serr == nil {
			s.log.Info("scanner busy, retrying", "attempt", attempt, "state", st.State, "adf", st.AdfState, "wait", wait)
			if settings.Source == escl.Feeder && !st.AdfLoaded() {
				return "", fmt.Errorf("%w; feeder no longer loaded", err)
			}
		}
		if err := sleep(ctx, wait); err != nil {
			return "", err
		}
		wait *= 2
	}
}

// deleteJob deletes a job, best effort. It uses its own context, so a job
// cancelled at shutdown is still deleted on the device.
func (s *Service) deleteJob(job string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DeleteTimeout)
	defer cancel()
	if err := s.dev.Delete(ctx, job); err != nil {
		s.log.Debug("delete job failed", "job", job, "err", err)
	}
}

func (s *Service) paperStillLoaded(ctx context.Context) bool {
	st, err := s.dev.Status(ctx)
	return err == nil && st.AdfLoaded()
}

// wakeScan runs a tiny platen scan and throws the result away. On the
// WF-2960 this wakes the scanner unit, after which the feeder works again.
func (s *Service) wakeScan(ctx context.Context) error {
	docs, job, err := s.runJob(ctx, escl.ScanSettings{
		Source: escl.Platen, Intent: "Preview", ColorMode: "Grayscale8", Resolution: 100,
		Region: escl.Region{Width: 300, Height: 300}, // one square inch
	})
	if job != "" {
		s.deleteJob(job)
	}
	if err == nil && len(docs) == 0 {
		err = errors.New("no document")
	}
	return err
}

// classify decides the outcome of a finished job before the PDFs are
// looked at. The precedence is fixed: aborted beats everything, because a
// PDF from an aborted job must never be delivered even if it looks complete.
func (s *Service) classify(ctx context.Context, source escl.InputSource, job string, docs [][]byte, err error) (result, msg string) {
	switch {
	case errors.Is(err, escl.ErrJobAborted):
		return metrics.ResultAborted, "job aborted by the scanner (HTTP 410)"
	case errors.Is(err, escl.ErrBusy):
		return metrics.ResultBusy, err.Error()
	case errors.Is(err, context.Canceled):
		return metrics.ResultError, "cancelled"
	case err != nil:
		return metrics.ResultError, err.Error()
	}

	// The job ended normally (404). Ask the device how it ended: after a jam
	// the WF-2960 lists the job as Aborted and the feeder as jammed.
	st, serr := s.dev.Status(ctx)
	if serr != nil {
		s.log.Warn("cannot verify job state", "job", job, "err", serr)
	} else {
		if info, ok := st.Job(job); ok && (info.JobState == escl.JobAborted || info.JobState == escl.JobCanceled) {
			return metrics.ResultAborted, fmt.Sprintf("job %s by the scanner after %d images", info.JobState, info.ImagesCompleted)
		}
		if source == escl.Feeder && escl.AdfIsFault(st.AdfState) {
			return metrics.ResultAborted, "feeder fault: " + st.AdfState
		}
	}
	if len(docs) == 0 {
		return metrics.ResultError, "scanner returned no document"
	}
	return metrics.ResultOK, ""
}

// validateAll validates every document and returns the total page count.
func validateAll(docs [][]byte) (pages int, result, msg string) {
	for i, doc := range docs {
		n, err := output.ValidatePDF(doc)
		if err != nil {
			return 0, metrics.ResultIncompletePDF, fmt.Sprintf("document %d: %v", i+1, err)
		}
		pages += n
	}
	return pages, metrics.ResultOK, ""
}

// reject keeps the documents of a failed scan in FAILED_DIR, if configured.
// They never go to OUTPUT_DIR.
func (s *Service) reject(rec *Record, docs [][]byte) {
	if s.failed == nil {
		return
	}
	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		name, err := s.failed.Write(doc, rec.Time, rec.Result)
		if err != nil {
			s.log.Error("cannot keep rejected scan", "dir", s.failed.Dir, "err", err)
			continue
		}
		rec.Files = append(rec.Files, "failed/"+name)
	}
}

func joinMsg(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
