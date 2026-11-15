package main

import (
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

var minimal = map[string]string{"ESCL_URL": "https://192.0.2.10", "OUTPUT_DIR": "/out"}

func with(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range minimal {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestDefaults(t *testing.T) {
	c, err := parseConfig(nil, env(minimal), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.PollInterval != 3*time.Second || c.AutoscanSettle != 10*time.Second || !c.AutoscanEnabled || c.AutoscanPaused ||
		c.ScanResolution != 300 || c.ScanColorMode != "Grayscale8" || c.ScanIntent != "Document" ||
		c.ScanWidth != 2550 || c.ScanHeight != 3508 || c.ScanXOffset != 0 ||
		c.HTTPAddr != ":8080" || c.MetricsAddr != ":9090" || !c.UIEnabled || c.UINoAuth ||
		c.SessionTTL != 12*time.Hour || c.LogFormat != "json" || c.location != time.UTC || c.WakeWorkaround {
		t.Errorf("defaults = %+v", c)
	}
	if got := strings.Join(c.scopes(), " "); got != "openid profile email groups" {
		t.Errorf("scopes = %q", got)
	}
}

func TestEnvAndFlags(t *testing.T) {
	c, err := parseConfig([]string{"-poll-interval=1s", "-scan-color-mode", "RGB24"}, env(with(map[string]string{
		"POLL_INTERVAL":        "7s", // the flag wins
		"POLL_MAX_BACKOFF":     "10s",
		"AUTOSCAN_ENABLED":     "false",
		"SCAN_X_OFFSET":        "70",
		"TZ":                   "Europe/Berlin",
		"OIDC_ALLOWED_GROUPS":  "admin, scanner users ,,",
		"OIDC_SCOPES":          "openid,groups",
		"ESCL_TLS_FINGERPRINT": "AA:BB",
	})), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.PollMaxBackoff != 10*time.Second {
		t.Errorf("PollMaxBackoff = %v, want 10s", c.PollMaxBackoff)
	}
	if c.PollInterval != time.Second || c.ScanColorMode != "RGB24" || c.AutoscanEnabled || c.ScanXOffset != 70 ||
		c.location.String() != "Europe/Berlin" || c.TLSFingerprint != "AA:BB" {
		t.Errorf("config = %+v", c)
	}
	if got := strings.Join(c.allowedGroups(), "|"); got != "admin|scanner users" {
		t.Errorf("groups = %q", got)
	}
	if got := strings.Join(c.scopes(), " "); got != "openid groups" {
		t.Errorf("scopes = %q", got)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"required", map[string]string{}, "ESCL_URL is required"},
		{"output dir", map[string]string{"ESCL_URL": "https://192.0.2.10"}, "OUTPUT_DIR is required"},
		{"color mode", with(map[string]string{"SCAN_COLOR_MODE": "Color"}), "SCAN_COLOR_MODE"},
		{"intent", with(map[string]string{"SCAN_INTENT": "Fast"}), "SCAN_INTENT"},
		{"bad duration", with(map[string]string{"POLL_INTERVAL": "soon"}), "POLL_INTERVAL"},
		{"bad bool", with(map[string]string{"UI_ENABLED": "maybe"}), "UI_ENABLED"},
		{"tz", with(map[string]string{"TZ": "Mars/Base"}), "TZ"},
		{"pin and insecure", with(map[string]string{"ESCL_TLS_FINGERPRINT": "aa", "ESCL_TLS_INSECURE": "true"}), "not both"},
		{"secret twice", with(map[string]string{"OIDC_CLIENT_SECRET": "x", "OIDC_CLIENT_SECRET_FILE": "/x"}), "not both"},
		{"log format", with(map[string]string{"LOG_FORMAT": "xml"}), "LOG_FORMAT"},
		{"region", with(map[string]string{"SCAN_WIDTH": "0"}), "SCAN_WIDTH"},
		{"backoff below poll", with(map[string]string{"POLL_INTERVAL": "5s", "POLL_MAX_BACKOFF": "2s"}), "POLL_MAX_BACKOFF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseConfig(nil, env(tt.env), io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestHelpAndVersionSkipValidation(t *testing.T) {
	var out strings.Builder
	if _, err := parseConfig([]string{"--help"}, env(nil), &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("--help: err = %v", err)
	}
	if !strings.Contains(out.String(), "-escl-url, ESCL_URL") {
		t.Errorf("usage lacks env names:\n%s", out.String())
	}
	c, err := parseConfig([]string{"-version"}, env(nil), io.Discard)
	if err != nil || !c.Version {
		t.Errorf("-version: %v", err)
	}
}

func TestSecretFiles(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig(nil, env(with(map[string]string{"SESSION_KEY_FILE": key, "OIDC_CLIENT_SECRET": "s3cret"})), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.SessionKey != "0123456789abcdef0123456789abcdef" || c.OIDCClientSecret != "s3cret" {
		t.Errorf("secrets not loaded (key %d bytes)", len(c.SessionKey))
	}
	if _, err := parseConfig(nil, env(with(map[string]string{"SESSION_KEY_FILE": filepath.Join(dir, "missing")})), io.Discard); err == nil {
		t.Error("missing key file accepted")
	}
}

// The UI refuses to start without OIDC unless UI_NO_AUTH=true.
func TestServeRefusesUIWithoutAuth(t *testing.T) {
	c, err := parseConfig(nil, env(map[string]string{
		"ESCL_URL": "https://192.0.2.10", "OUTPUT_DIR": t.TempDir(),
		"METRICS_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	err = serve(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "UI_NO_AUTH") {
		t.Fatalf("serve: err = %v, want refusal", err)
	}
}

func TestServeRejectsFailedDirInsideOutput(t *testing.T) {
	out := t.TempDir()
	if err := os.Mkdir(filepath.Join(out, "failed"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig(nil, env(map[string]string{
		"ESCL_URL": "https://192.0.2.10", "OUTPUT_DIR": out, "FAILED_DIR": filepath.Join(out, "failed"),
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(c, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "FAILED_DIR") {
		t.Fatalf("serve: err = %v", err)
	}
}

func TestDoubleSidedConfig(t *testing.T) {
	c, err := parseConfig(nil, env(minimal), io.Discard)
	if err != nil || c.DoubleSidedSubdir != "double-sided" || c.DoubleSidedTimeout != 30*time.Minute {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	if c, err := parseConfig(nil, env(with(map[string]string{"DOUBLE_SIDED_SUBDIR": ""})), io.Discard); err != nil || c.DoubleSidedSubdir != "" {
		t.Errorf("empty subdir (off): %v", err)
	}
	for _, bad := range []string{"a/b", "/abs", ".", "..", ".hidden", `a\b`} {
		if _, err := parseConfig(nil, env(with(map[string]string{"DOUBLE_SIDED_SUBDIR": bad})), io.Discard); err == nil || !strings.Contains(err.Error(), "DOUBLE_SIDED_SUBDIR") {
			t.Errorf("subdir %q: err = %v", bad, err)
		}
	}
	if _, err := parseConfig(nil, env(with(map[string]string{"DOUBLE_SIDED_TIMEOUT": "0s"})), io.Discard); err == nil {
		t.Error("zero timeout accepted")
	}
}

func TestPrepareDirsCreatesDoubleSidedFolder(t *testing.T) {
	out := t.TempDir()
	c, err := parseConfig(nil, env(map[string]string{"ESCL_URL": "https://192.0.2.10", "OUTPUT_DIR": out, "DOUBLE_SIDED_SUBDIR": "duplex"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareDirs(c); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(out, "duplex")); err != nil || !fi.IsDir() {
		t.Fatalf("double-sided folder not created: %v", err)
	}
	if err := prepareDirs(c); err != nil {
		t.Errorf("second start: %v", err)
	}
	// A file in the way is a clear startup error.
	c.DoubleSidedSubdir = "blocked"
	if err := os.WriteFile(filepath.Join(out, "blocked"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDirs(c); err == nil || !strings.Contains(err.Error(), "DOUBLE_SIDED_SUBDIR") {
		t.Errorf("blocked folder: err = %v", err)
	}
}
