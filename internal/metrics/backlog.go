package metrics

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// consumable lists the extensions Paperless-ngx consumes.
var consumable = map[string]bool{
	".pdf": true, ".png": true, ".jpg": true, ".jpeg": true, ".tif": true,
	".tiff": true, ".webp": true, ".txt": true, ".eml": true,
}

// Backlog watches the Paperless consume directory: files that stay there
// for long mean Paperless is not consuming.
type Backlog struct {
	Dir      string
	Interval time.Duration
	Log      *slog.Logger
	Now      func() time.Time

	files  prometheus.Gauge
	oldest prometheus.Gauge
}

// NewBacklog creates the backlog metrics and registers them with reg.
func NewBacklog(reg prometheus.Registerer, dir string, log *slog.Logger) *Backlog {
	b := &Backlog{
		Dir:      dir,
		Interval: time.Minute,
		Log:      log,
		Now:      time.Now,
		files: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "backlog_files",
			Help: "Consumable files waiting in the Paperless consume directory (recursive).",
		}),
		oldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "backlog_oldest_age_seconds",
			Help: "Age of the oldest consumable file in the consume directory, 0 when empty.",
		}),
	}
	reg.MustRegister(b.files, b.oldest)
	return b
}

// Run updates the metrics every Interval until ctx is done.
func (b *Backlog) Run(ctx context.Context) {
	t := time.NewTicker(b.Interval)
	defer t.Stop()
	failing := false
	for {
		n, oldest, err := ScanBacklog(b.Dir, b.Now())
		switch {
		case err != nil && !failing:
			b.Log.Warn("backlog scan failed", "dir", b.Dir, "err", err)
			failing = true
		case err == nil:
			failing = false
			b.files.Set(float64(n))
			b.oldest.Set(oldest.Seconds())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ScanBacklog counts consumable files below dir and the age of the oldest.
// Dotfiles, dot-directories and ".part" files are ignored.
func ScanBacklog(dir string, now time.Time) (files int, oldest time.Duration, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path != dir && errors.Is(err, fs.ErrNotExist) {
				return nil // removed while walking
			}
			return err
		}
		name := d.Name()
		if path != dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() || !consumable[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // removed meanwhile, e.g. consumed by Paperless
		}
		files++
		if age := now.Sub(info.ModTime()); age > oldest {
			oldest = age
		}
		return nil
	})
	return files, oldest, err
}
