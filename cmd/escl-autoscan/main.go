// Command escl-autoscan watches an eSCL scanner and scans automatically when
// paper is put into its document feeder.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in a distroless image without zoneinfo

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/autoscan"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// shutdownScanWait is how long a running scan may continue after SIGTERM
// before its job is cancelled.
const shutdownScanWait = 60 * time.Second

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := parseConfig(args, os.LookupEnv, os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintf(os.Stderr, "escl-autoscan: %v\n", err)
		return 2
	case cfg.Version:
		fmt.Println(version)
		return 0
	}

	log := newLogger(os.Stderr, cfg)
	if err := serve(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		return 1
	}
	return 0
}

func newLogger(w io.Writer, cfg *config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.logLevel()}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func serve(cfg *config, log *slog.Logger) error {
	if err := output.CheckDirs(cfg.OutputDir, cfg.FailedDir, cfg.BacklogDir); err != nil {
		return err
	}
	dev, err := escl.New(escl.Options{
		URL:             cfg.ESCLURL,
		Fingerprint:     cfg.TLSFingerprint,
		Insecure:        cfg.TLSInsecure,
		Timeout:         cfg.ESCLTimeout,
		DocumentTimeout: cfg.ESCLDocumentTimeout,
	})
	if err != nil {
		return err
	}
	if cfg.TLSFingerprint == "" && cfg.TLSInsecure {
		log.Warn("ESCL_TLS_INSECURE=true: the scanner's certificate is not verified; consider ESCL_TLS_FINGERPRINT")
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg, escl.KnownAdfStates)

	var failed *output.Writer
	if cfg.FailedDir != "" {
		failed = &output.Writer{Dir: cfg.FailedDir, Location: cfg.location, Log: log}
	}
	svc := autoscan.New(autoscan.Config{
		PollInterval:   cfg.PollInterval,
		MaxBackoff:     cfg.PollMaxBackoff,
		Settle:         cfg.AutoscanSettle,
		Enabled:        cfg.AutoscanEnabled,
		Paused:         cfg.AutoscanPaused,
		Intent:         cfg.ScanIntent,
		ColorMode:      cfg.ScanColorMode,
		Resolution:     cfg.ScanResolution,
		Region:         escl.Region{XOffset: cfg.ScanXOffset, Width: cfg.ScanWidth, Height: cfg.ScanHeight},
		WakeWorkaround: cfg.WakeWorkaround,
	}, dev, &output.Writer{Dir: cfg.OutputDir, Location: cfg.location, Log: log}, failed, m, log)

	var ui *web.Server
	if cfg.UIEnabled {
		if ui, err = web.New(web.Config{
			OIDC:           oidcConfig(cfg),
			NoAuth:         cfg.UINoAuth,
			SessionKey:     []byte(cfg.SessionKey),
			SessionTTL:     cfg.SessionTTL,
			CookieInsecure: cfg.CookieInsecure,
			Location:       cfg.location,
		}, svc, log); err != nil {
			return err
		}
	}

	// Listen before starting anything, so a busy port fails fast.
	metricsLn, err := net.Listen("tcp", cfg.MetricsAddr)
	if err != nil {
		return fmt.Errorf("metrics listener: %w", err)
	}
	metricsSrv := newHTTPServer(web.MetricsHandler(reg, func() error { return dirReady(cfg.OutputDir) }), log)
	var uiSrv *http.Server
	var uiLn net.Listener
	if ui != nil {
		if uiLn, err = net.Listen("tcp", cfg.HTTPAddr); err != nil {
			metricsLn.Close()
			return fmt.Errorf("UI listener: %w", err)
		}
		uiSrv = newHTTPServer(ui, log)
		ui.Prewarm()
	}

	log.Info("starting escl-autoscan",
		"version", version, "scanner", dev.BaseURL(), "tls_pinned", cfg.TLSFingerprint != "",
		"output_dir", cfg.OutputDir, "failed_dir", cfg.FailedDir, "backlog_dir", cfg.BacklogDir, "tz", cfg.location.String(),
		"autoscan", cfg.AutoscanEnabled, "paused", cfg.AutoscanPaused, "settle", cfg.AutoscanSettle,
		"resolution", cfg.ScanResolution, "color_mode", cfg.ScanColorMode,
		"region", fmt.Sprintf("x=%d w=%d h=%d", cfg.ScanXOffset, cfg.ScanWidth, cfg.ScanHeight),
		"ui", cfg.UIEnabled, "ui_addr", cfg.HTTPAddr, "metrics_addr", cfg.MetricsAddr,
		"oidc_issuer", cfg.OIDCIssuer, "wake_workaround", cfg.WakeWorkaround)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- serveHTTP(metricsSrv, metricsLn, "metrics") }()
	if uiSrv != nil {
		go func() { errc <- serveHTTP(uiSrv, uiLn, "UI") }()
	}
	if cfg.BacklogDir != "" {
		go metrics.NewBacklog(reg, cfg.BacklogDir, log).Run(ctx)
	}
	runDone := make(chan struct{})
	go func() { svc.Run(ctx); close(runDone) }()

	var serveErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case serveErr = <-errc:
		stop()
	}

	// Stop starting scans at once; a running scan may finish (or is
	// cancelled and deleted after shutdownScanWait).
	scansDone := make(chan struct{})
	go func() { svc.Shutdown(shutdownScanWait); close(scansDone) }()
	if uiSrv != nil {
		shutdownHTTP(uiSrv)
	}
	<-runDone
	<-scansDone
	shutdownHTTP(metricsSrv)
	log.Info("stopped")
	return serveErr
}

func oidcConfig(cfg *config) *web.OIDCConfig {
	if cfg.OIDCIssuer == "" {
		return nil
	}
	return &web.OIDCConfig{
		Issuer:        cfg.OIDCIssuer,
		ClientID:      cfg.OIDCClientID,
		ClientSecret:  cfg.OIDCClientSecret,
		RedirectURL:   cfg.OIDCRedirectURL,
		Scopes:        cfg.scopes(),
		AllowedGroups: cfg.allowedGroups(),
	}
}

func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}

func shutdownHTTP(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func serveHTTP(srv *http.Server, ln net.Listener, name string) error {
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s server: %w", name, err)
	}
	return nil
}

// dirReady reports whether the output directory is reachable. A hung NFS
// mount blocks stat, hence the timeout.
func dirReady(dir string) error {
	done := make(chan error, 1)
	go func() {
		fi, err := os.Stat(dir)
		if err == nil && !fi.IsDir() {
			err = fmt.Errorf("%s is not a directory", dir)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return fmt.Errorf("stat %s timed out", dir)
	}
}
