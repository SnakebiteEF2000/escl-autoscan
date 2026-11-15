package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"
)

// config holds every setting. Each flag has an environment variable of the
// same name in upper case with "_" for "-" (e.g. -escl-url / ESCL_URL);
// a flag on the command line wins over the environment.
type config struct {
	ESCLURL             string
	TLSFingerprint      string
	TLSInsecure         bool
	ESCLTimeout         time.Duration
	ESCLDocumentTimeout time.Duration
	PollInterval        time.Duration
	PollMaxBackoff      time.Duration

	AutoscanEnabled bool
	AutoscanSettle  time.Duration
	AutoscanPaused  bool
	WakeWorkaround  bool

	ScanResolution int
	ScanColorMode  string
	ScanIntent     string
	ScanWidth      int
	ScanHeight     int
	ScanXOffset    int

	OutputDir  string
	FailedDir  string
	BacklogDir string
	TZ         string
	location   *time.Location

	HTTPAddr    string
	MetricsAddr string
	UIEnabled   bool
	UINoAuth    bool

	OIDCIssuer           string
	OIDCClientID         string
	OIDCClientSecret     string
	OIDCClientSecretFile string
	OIDCRedirectURL      string
	OIDCScopes           string
	OIDCAllowedGroups    string
	SessionKey           string
	SessionKeyFile       string
	SessionTTL           time.Duration
	CookieInsecure       bool

	LogLevel  string
	LogFormat string
	Version   bool
}

var (
	colorModes = []string{"BlackAndWhite1", "Grayscale8", "RGB24"}
	intents    = []string{"Document", "TextAndGraphic", "Photo", "Preview"}
)

// envName maps a flag name to its environment variable.
func envName(flagName string) string {
	return strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// parseConfig parses flags, then fills unset flags from the environment.
// It returns flag.ErrHelp for -h/-help.
func parseConfig(args []string, getenv func(string) (string, bool), out io.Writer) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("escl-autoscan", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&c.ESCLURL, "escl-url", "", "scanner base URL, e.g. https://192.0.2.10 (required; the port is made explicit)")
	fs.StringVar(&c.TLSFingerprint, "escl-tls-fingerprint", "", "pin the scanner's TLS certificate: SHA-256 fingerprint in hex, colons optional")
	fs.BoolVar(&c.TLSInsecure, "escl-tls-insecure", false, "do not verify the scanner's TLS certificate at all")
	fs.DurationVar(&c.ESCLTimeout, "escl-timeout", 10*time.Second, "timeout for status, capabilities, job creation and deletion")
	fs.DurationVar(&c.ESCLDocumentTimeout, "escl-document-timeout", 5*time.Minute, "timeout for one NextDocument request (blocks while the feeder scans)")
	fs.DurationVar(&c.PollInterval, "poll-interval", 3*time.Second, "scanner status poll interval (backs off up to -poll-max-backoff while unreachable)")
	fs.DurationVar(&c.PollMaxBackoff, "poll-max-backoff", 30*time.Second, "longest poll interval while the scanner is unreachable (switched off, asleep); also bounds how long a switched-on scanner goes unnoticed")

	fs.BoolVar(&c.AutoscanEnabled, "autoscan-enabled", true, "scan automatically when paper is put into the feeder")
	fs.DurationVar(&c.AutoscanSettle, "autoscan-settle", 10*time.Second, "wait this long after paper is detected before scanning")
	fs.BoolVar(&c.AutoscanPaused, "autoscan-paused", false, "start with autoscan paused")
	fs.BoolVar(&c.WakeWorkaround, "wake-workaround", false, "on the Epson wake bug, run a tiny flatbed scan and retry the feeder once")

	fs.IntVar(&c.ScanResolution, "scan-resolution", 300, "resolution in dpi")
	fs.StringVar(&c.ScanColorMode, "scan-color-mode", "Grayscale8", "default color mode: "+strings.Join(colorModes, ", "))
	fs.StringVar(&c.ScanIntent, "scan-intent", "Document", "scan intent: "+strings.Join(intents, ", "))
	fs.IntVar(&c.ScanWidth, "scan-width", 2550, "scan width in 1/300 inch (2550 = 8.5in, the full feeder width)")
	fs.IntVar(&c.ScanHeight, "scan-height", 3508, "scan height in 1/300 inch (3508 = A4)")
	fs.IntVar(&c.ScanXOffset, "scan-x-offset", 0, "horizontal offset in 1/300 inch")

	fs.StringVar(&c.OutputDir, "output-dir", "", "directory for finished PDFs, e.g. <paperless consume>/scanner (required)")
	fs.StringVar(&c.FailedDir, "failed-dir", "", "keep rejected or partial scans here (must be outside the consume directory); default: discard")
	fs.StringVar(&c.BacklogDir, "backlog-dir", "", "Paperless consume root to export backlog metrics for (optional)")
	fs.StringVar(&c.TZ, "tz", "", "time zone for file names and the UI, e.g. Europe/Berlin (default UTC)")

	fs.StringVar(&c.HTTPAddr, "http-addr", ":8080", "UI listen address")
	fs.StringVar(&c.MetricsAddr, "metrics-addr", ":9090", "listen address for /metrics, /healthz and /readyz")
	fs.BoolVar(&c.UIEnabled, "ui-enabled", true, "serve the web UI")
	fs.BoolVar(&c.UINoAuth, "ui-no-auth", false, "serve the UI WITHOUT login (unsafe; only for trusted networks)")

	fs.StringVar(&c.OIDCIssuer, "oidc-issuer", "", "OpenID Connect issuer URL; enables login")
	fs.StringVar(&c.OIDCClientID, "oidc-client-id", "", "OIDC client ID")
	fs.StringVar(&c.OIDCClientSecret, "oidc-client-secret", "", "OIDC client secret (prefer the _FILE variant; empty for a public client)")
	fs.StringVar(&c.OIDCClientSecretFile, "oidc-client-secret-file", "", "file containing the OIDC client secret")
	fs.StringVar(&c.OIDCRedirectURL, "oidc-redirect-url", "", "OIDC redirect URL, https://<ui host>/auth/callback")
	fs.StringVar(&c.OIDCScopes, "oidc-scopes", "openid profile email groups", "OIDC scopes, separated by spaces or commas")
	fs.StringVar(&c.OIDCAllowedGroups, "oidc-allowed-groups", "", "comma-separated groups allowed to use the UI (empty: any authenticated user)")
	fs.StringVar(&c.SessionKey, "session-key", "", "session signing key, at least 32 bytes (prefer the _FILE variant)")
	fs.StringVar(&c.SessionKeyFile, "session-key-file", "", "file containing the session signing key")
	fs.DurationVar(&c.SessionTTL, "session-ttl", 12*time.Hour, "session lifetime")
	fs.BoolVar(&c.CookieInsecure, "cookie-insecure", false, "send cookies without Secure, for local tests over plain HTTP")

	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", "json", "log format: json or text")
	fs.BoolVar(&c.Version, "version", false, "print the version and exit")

	fs.Usage = func() {
		fmt.Fprintf(out, "escl-autoscan scans from an eSCL (AirPrint) scanner's document feeder automatically.\n\n")
		fmt.Fprintf(out, "Usage: escl-autoscan [flags]\n\nEvery flag can also be set by the environment variable shown; flags win.\n\n")
		fs.VisitAll(func(f *flag.Flag) {
			if f.Name == "version" {
				fmt.Fprintf(out, "  -%s\n    \t%s\n", f.Name, f.Usage)
				return
			}
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = fmt.Sprintf(" (default %s)", f.DefValue)
			}
			fmt.Fprintf(out, "  -%s, %s\n    \t%s%s\n", f.Name, envName(f.Name), f.Usage, def)
		})
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || f.Name == "version" {
			return
		}
		if v, ok := getenv(envName(f.Name)); ok {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", envName(f.Name), err))
			}
		}
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if c.Version {
		return c, nil
	}
	return c, c.validate()
}

func (c *config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(c.ESCLURL != "", "ESCL_URL is required")
	check(c.OutputDir != "", "OUTPUT_DIR is required")
	check(!(c.TLSFingerprint != "" && c.TLSInsecure), "set ESCL_TLS_FINGERPRINT or ESCL_TLS_INSECURE, not both")
	check(c.ESCLTimeout > 0 && c.ESCLDocumentTimeout > 0, "ESCL_TIMEOUT and ESCL_DOCUMENT_TIMEOUT must be positive")
	check(c.PollInterval > 0, "POLL_INTERVAL must be positive")
	check(c.PollMaxBackoff >= c.PollInterval, "POLL_MAX_BACKOFF must not be shorter than POLL_INTERVAL")
	check(c.AutoscanSettle >= 0, "AUTOSCAN_SETTLE must not be negative")
	check(c.ScanResolution > 0, "SCAN_RESOLUTION must be positive")
	check(slices.Contains(colorModes, c.ScanColorMode), "SCAN_COLOR_MODE must be one of %v", colorModes)
	check(slices.Contains(intents, c.ScanIntent), "SCAN_INTENT must be one of %v", intents)
	check(c.ScanWidth > 0 && c.ScanHeight > 0 && c.ScanXOffset >= 0, "SCAN_WIDTH and SCAN_HEIGHT must be positive, SCAN_X_OFFSET not negative")
	check(c.SessionTTL > 0, "SESSION_TTL must be positive")
	check(slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel), "LOG_LEVEL must be debug, info, warn or error")
	check(c.LogFormat == "json" || c.LogFormat == "text", "LOG_FORMAT must be json or text")

	loc := time.UTC
	if c.TZ != "" {
		l, err := time.LoadLocation(c.TZ)
		check(err == nil, "TZ: unknown time zone %q", c.TZ)
		if err == nil {
			loc = l
		}
	}
	c.location = loc

	secret, err := secretValue("OIDC_CLIENT_SECRET", c.OIDCClientSecret, c.OIDCClientSecretFile)
	check(err == nil, "%v", err)
	c.OIDCClientSecret = secret
	key, err := secretValue("SESSION_KEY", c.SessionKey, c.SessionKeyFile)
	check(err == nil, "%v", err)
	c.SessionKey = key
	return errors.Join(errs...)
}

// secretValue returns a secret given directly or in a file. Trailing
// newlines from the file are removed.
func secretValue(name, value, file string) (string, error) {
	switch {
	case value != "" && file != "":
		return "", fmt.Errorf("set %s or %s_FILE, not both", name, name)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return value, nil
}

// scopes splits OIDC_SCOPES on spaces and commas.
func (c *config) scopes() []string {
	return strings.Fields(strings.ReplaceAll(c.OIDCScopes, ",", " "))
}

// allowedGroups splits OIDC_ALLOWED_GROUPS on commas; group names may
// contain spaces.
func (c *config) allowedGroups() []string {
	var out []string
	for g := range strings.SplitSeq(c.OIDCAllowedGroups, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func (c *config) logLevel() slog.Level {
	var l slog.Level
	_ = l.UnmarshalText([]byte(c.LogLevel))
	return l
}
