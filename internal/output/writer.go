package output

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// FileMode is the mode of delivered files.
const FileMode fs.FileMode = 0o644

// Writer writes scans into a directory without ever exposing a partial file
// under its final name.
type Writer struct {
	Dir      string         // OUTPUT_DIR
	Location *time.Location // time zone for file names; UTC when nil
	Log      *slog.Logger   // optional
}

// Write stores data as scan-YYYYMMDD-HHMMSS[-N][-suffix].pdf and returns the
// file name. The data is written to a ".part" file first (Paperless only
// consumes known extensions, so it ignores it), synced, and then renamed.
func (w *Writer) Write(data []byte, at time.Time, suffix string) (string, error) {
	loc := w.Location
	if loc == nil {
		loc = time.UTC
	}
	base := "scan-" + at.In(loc).Format("20060102-150405")
	if suffix != "" {
		base += "-" + suffix
	}

	tmp, err := os.CreateTemp(w.Dir, base+"-*.part")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write %s: %w", tmpName, err)
	}
	// CreateTemp uses 0600; Paperless may run as another user.
	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return "", fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := syncDir(w.Dir); err != nil {
		return "", err
	}

	// Only this process writes scan-* names, one scan at a time, so a
	// check-then-rename cannot race with another writer.
	name := base + ".pdf"
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(w.Dir, name)); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return "", fmt.Errorf("stat %s: %w", name, err)
		}
		if i > 1000 {
			return "", fmt.Errorf("too many files named %s*", base)
		}
		name = fmt.Sprintf("%s-%d.pdf", base, i)
	}
	if err := os.Rename(tmpName, filepath.Join(w.Dir, name)); err != nil {
		return "", fmt.Errorf("rename to %s: %w", name, err)
	}
	ok = true
	// The file is complete and visible now; a failed sync only weakens
	// durability across a crash, so it must not turn the scan into a failure.
	if err := syncDir(w.Dir); err != nil && w.Log != nil {
		w.Log.Warn("directory sync after rename failed", "dir", w.Dir, "err", err)
	}
	return name, nil
}

// syncDir makes a create or rename durable. Some file systems (and NFS
// clients) reject fsync on a directory; that is not an error here.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("sync dir: %w", err)
	}
	return nil
}

// CheckDirs validates OUTPUT_DIR, FAILED_DIR and BACKLOG_DIR: the output
// directory must exist, and rejected scans must not end up anywhere Paperless
// consumes from.
func CheckDirs(outputDir, failedDir, backlogDir string) error {
	if err := isDir(outputDir); err != nil {
		return fmt.Errorf("OUTPUT_DIR: %w", err)
	}
	if failedDir == "" {
		return nil
	}
	if err := isDir(failedDir); err != nil {
		return fmt.Errorf("FAILED_DIR: %w", err)
	}
	for _, consume := range []string{outputDir, backlogDir} {
		if consume != "" && within(failedDir, consume) {
			return fmt.Errorf("FAILED_DIR %q must not be inside %q: Paperless would consume rejected scans", failedDir, consume)
		}
	}
	return nil
}

func isDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

// within reports whether path is dir or below it.
func within(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if real, err := filepath.EvalSymlinks(d); err == nil {
		d = real
	}
	rel, err := filepath.Rel(d, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
