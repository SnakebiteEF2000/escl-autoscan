package output

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/pdftest"
)

func TestValidatePDF(t *testing.T) {
	classic := pdftest.New(pdftest.Options{Pages: 3})
	// Cut before the xref table, like the WF-2960's PDF after a paper jam.
	truncated := classic[:bytes.LastIndex(classic, []byte("xref"))]

	tests := []struct {
		name  string
		data  []byte
		pages int
		bad   string // substring of the error; empty when valid
	}{
		{name: "complete", data: classic, pages: 3},
		{name: "xref stream, compact /Type/Page", data: pdftest.New(pdftest.Options{Pages: 2, XRefStream: true, Compact: true}), pages: 2},
		{name: "one page", data: pdftest.New(pdftest.Options{Pages: 1}), pages: 1},
		{name: "trailing newline after EOF", data: append(append([]byte{}, classic...), "\r\n"...), pages: 3},
		{name: "truncated before xref", data: truncated, bad: "%%EOF"},
		{name: "truncated mid stream", data: classic[:len(classic)/2], bad: "%%EOF"},
		{name: "no pages", data: pdftest.New(pdftest.Options{Pages: 0}), bad: "no pages"},
		{name: "garbage", data: []byte("<html>503 Service Unavailable</html>"), bad: "header"},
		{name: "empty", data: nil, bad: "empty"},
		{name: "startxref beyond end", data: bytes.Replace(classic, []byte("startxref\n"), []byte("startxref\n9"), 1), bad: "outside"},
		{name: "startxref to wrong place", data: offByOne(classic), bad: "cross-reference"},
		{name: "xref without trailer", data: bytes.Replace(classic, []byte("trailer"), []byte("tr4iler"), 1), bad: "trailer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pages, err := ValidatePDF(tt.data)
			if tt.bad == "" {
				if err != nil || pages != tt.pages {
					t.Fatalf("got pages=%d err=%v, want %d pages", pages, err, tt.pages)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrIncomplete) || !strings.Contains(err.Error(), tt.bad) {
				t.Fatalf("got pages=%d err=%v, want ErrIncomplete containing %q", pages, err, tt.bad)
			}
		})
	}
}

// offByOne makes startxref point one byte past the xref keyword.
func offByOne(pdf []byte) []byte {
	i := bytes.LastIndex(pdf, []byte("startxref\n")) + len("startxref\n")
	j := i + bytes.Index(pdf[i:], []byte("\n"))
	n, _ := strconv.Atoi(string(pdf[i:j]))
	out := append([]byte{}, pdf[:i]...)
	out = strconv.AppendInt(out, int64(n+1), 10)
	return append(out, pdf[j:]...)
}

func TestWriterAtomicNamesAndMode(t *testing.T) {
	dir := t.TempDir()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	w := &Writer{Dir: dir, Location: berlin}
	at := time.Date(2026, 3, 1, 22, 30, 5, 0, time.UTC) // 23:30:05 in Berlin
	data := pdftest.New(pdftest.Options{Pages: 1})

	var names []string
	for range 3 {
		name, err := w.Write(data, at, "")
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	want := []string{"scan-20260301-233005.pdf", "scan-20260301-233005-2.pdf", "scan-20260301-233005-3.pdf"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("name %d = %q, want %q", i, names[i], want[i])
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("dir has %d entries, want 3 (no .part left behind)", len(entries))
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover temp file %s", e.Name())
		}
		fi, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != FileMode {
			t.Errorf("%s mode = %v, want %v", e.Name(), fi.Mode().Perm(), FileMode)
		}
		got, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if !bytes.Equal(got, data) {
			t.Errorf("%s content differs", e.Name())
		}
	}
}

func TestWriterSuffixAndDefaultUTC(t *testing.T) {
	w := &Writer{Dir: t.TempDir()}
	name, err := w.Write([]byte("x"), time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 3600)), "aborted")
	if err != nil {
		t.Fatal(err)
	}
	if name != "scan-20260102-020405-aborted.pdf" {
		t.Errorf("name = %q", name)
	}
}

func TestWriterMissingDir(t *testing.T) {
	w := &Writer{Dir: filepath.Join(t.TempDir(), "missing")}
	if _, err := w.Write([]byte("x"), time.Now(), ""); err == nil {
		t.Fatal("want error for missing directory")
	}
}

func TestCheckDirs(t *testing.T) {
	root := t.TempDir()
	consume := filepath.Join(root, "consume")
	out := filepath.Join(consume, "scanner")
	failed := filepath.Join(root, "failed")
	inside := filepath.Join(consume, "failed")
	for _, d := range []string{out, failed, inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckDirs(out, failed, consume); err != nil {
		t.Errorf("valid dirs: %v", err)
	}
	if err := CheckDirs(out, "", ""); err != nil {
		t.Errorf("no failed dir: %v", err)
	}
	if err := CheckDirs(out, inside, consume); err == nil {
		t.Error("FAILED_DIR inside BACKLOG_DIR accepted")
	}
	if err := CheckDirs(out, out, ""); err == nil {
		t.Error("FAILED_DIR == OUTPUT_DIR accepted")
	}
	if err := CheckDirs(filepath.Join(root, "nope"), "", ""); err == nil {
		t.Error("missing OUTPUT_DIR accepted")
	}
	// A sibling whose name starts with ".." of the consume dir is not inside it.
	if within(filepath.Join(root, "consume..x"), consume) {
		t.Error("within: sibling treated as inside")
	}
}

// Writes may come from two goroutines (a scan, and fronts saved after a
// double-sided timeout), even with the same time stamp. No file may replace
// another.
func TestWriteConcurrently(t *testing.T) {
	dir := t.TempDir()
	w := &Writer{Dir: dir}
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	const n = 40
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if _, err := w.Write(fmt.Appendf(nil, "document %d", i), at, ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		seen[string(data)] = true
	}
	if len(entries) != n || len(seen) != n {
		t.Fatalf("%d files with %d different contents, want %d of each", len(entries), len(seen), n)
	}
}
