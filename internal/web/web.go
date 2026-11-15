// Package web serves the scanner UI, protected by OpenID Connect login.
package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/autoscan"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
)

//go:embed templates static
var assets embed.FS

// MinSessionKeyLen is the minimum length of SESSION_KEY.
const MinSessionKeyLen = 32

// Scanner is what the UI needs from the autoscan service.
type Scanner interface {
	Snapshot() autoscan.Snapshot
	Scan(source escl.InputSource, colorMode string) error
	SetPaused(paused bool)
	StartDoubleSided(start bool) error
}

// Config configures the UI.
type Config struct {
	OIDC           *OIDCConfig // nil: no login
	NoAuth         bool        // UI_NO_AUTH: explicitly allow serving without login
	SessionKey     []byte
	SessionTTL     time.Duration
	CookieInsecure bool           // COOKIE_INSECURE: allow cookies over plain HTTP (local tests)
	Location       *time.Location // for displayed times
}

// Server is the UI handler.
type Server struct {
	cfg     Config
	scanner Scanner
	log     *slog.Logger
	now     func() time.Time

	oidc          *oidcAuth // nil without OIDC
	signer        signer
	origin        string // scheme://host of the redirect URL
	sessionCookie string
	loginCookie   string
	tmpl          *template.Template
	mux           *http.ServeMux
}

// New validates the configuration and returns the UI handler.
func New(cfg Config, scanner Scanner, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, scanner: scanner, log: log, now: time.Now}
	if s.cfg.Location == nil {
		s.cfg.Location = time.UTC
	}
	if s.cfg.SessionTTL <= 0 {
		s.cfg.SessionTTL = 12 * time.Hour
	}

	switch {
	case cfg.OIDC != nil && cfg.NoAuth:
		return nil, errors.New("UI_NO_AUTH=true conflicts with OIDC configuration; use one of them")
	case cfg.OIDC != nil:
		o := cfg.OIDC
		if o.Issuer == "" || o.ClientID == "" || o.RedirectURL == "" {
			return nil, errors.New("OIDC needs OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_REDIRECT_URL")
		}
		u, err := url.Parse(o.RedirectURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, fmt.Errorf("invalid OIDC_REDIRECT_URL %q", o.RedirectURL)
		}
		if len(cfg.SessionKey) < MinSessionKeyLen {
			return nil, fmt.Errorf("SESSION_KEY must be at least %d bytes when OIDC is enabled", MinSessionKeyLen)
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "profile", "email", "groups"}
		}
		if len(o.AllowedGroups) == 0 {
			log.Warn("OIDC_ALLOWED_GROUPS is empty: every user of the identity provider can use the scanner")
		}
		s.oidc = &oidcAuth{cfg: *o, log: log}
		s.origin = u.Scheme + "://" + u.Host
		s.signer = signer{key: cfg.SessionKey}
	case cfg.NoAuth:
		log.Warn("UI_NO_AUTH=true: the UI is served WITHOUT login; anyone who can reach it can scan")
		// Only CSRF tokens are signed; a per-process key is enough.
		s.signer = signer{key: []byte(rand.Text() + rand.Text())}
	default:
		return nil, errors.New("the UI needs OIDC login (OIDC_ISSUER, ...); set UI_NO_AUTH=true to serve it without login (unsafe), or UI_ENABLED=false")
	}

	s.sessionCookie, s.loginCookie = "escl_session", "escl_login"
	if !cfg.CookieInsecure {
		// __Host- cookies must be Secure, host-only and Path=/.
		s.sessionCookie, s.loginCookie = "__Host-"+s.sessionCookie, "__Host-"+s.loginCookie
	}

	tmpl, err := template.New("").Funcs(template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "–"
			}
			return t.In(s.cfg.Location).Format("2006-01-02 15:04:05")
		},
		"fmtDur": func(d time.Duration) string { return d.Round(time.Second).String() },
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = tmpl

	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /scan", s.withPost(s.handleScan))
	mux.HandleFunc("POST /pause", s.withPost(s.handlePause))
	if scanner.Snapshot().DoubleSidedEnabled {
		mux.HandleFunc("POST /double-sided", s.withPost(s.handleDoubleSided))
	}
	if s.oidc != nil {
		mux.HandleFunc("GET /auth/login", s.handleLogin)
		mux.HandleFunc("GET /auth/callback", s.handleCallback)
		mux.HandleFunc("POST /auth/logout", s.withPost(s.handleLogout))
	}
	s.mux = mux
	return s, nil
}

// ServeHTTP adds security headers and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	s.mux.ServeHTTP(w, r)
}

// Prewarm discovers the OIDC provider in the background, so the first
// login is fast. Failures are retried on login.
func (s *Server) Prewarm() {
	if s.oidc == nil {
		return
	}
	go func() {
		if _, _, err := s.oidc.setup(context.Background()); err != nil {
			s.log.Warn("OIDC provider not reachable yet; retrying on login", "err", err)
		}
	}()
}

type pageData struct {
	Snap    autoscan.Snapshot
	User    string
	Auth    bool
	CSRF    string
	Gray    bool // preselect gray in the color choice
	Error   string
	Refresh int // seconds
	// DSRemaining is the time left in the current double-sided step.
	DSRemaining string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	sess, raw, ok := s.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	s.renderIndex(w, http.StatusOK, sess, raw, "")
}

func (s *Server) renderIndex(w http.ResponseWriter, code int, sess *session, raw, errMsg string) {
	snap := s.scanner.Snapshot()
	refresh := 30
	if snap.Scanning != nil || snap.Settling {
		refresh = 3
	}
	remaining := ""
	if snap.DoubleSided != autoscan.DoubleSidedOff {
		remaining = fmt.Sprintf("%d min", int(math.Ceil(max(time.Until(snap.DoubleSidedDeadline), 0).Minutes())))
	}
	s.render(w, code, "index.html", pageData{
		Snap: snap, User: sess.display(), Auth: s.oidc != nil, CSRF: s.csrfToken(raw),
		Gray: snap.ColorMode != "RGB24", Error: errMsg, Refresh: refresh, DSRemaining: remaining,
	})
}

// postHandler handles a POST that passed the session, origin and CSRF
// checks; raw is the session cookie value.
type postHandler func(w http.ResponseWriter, r *http.Request, sess *session, raw string)

// withPost wraps a POST handler with session, origin and CSRF checks.
func (s *Server) withPost(h postHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		sess, raw, ok := s.currentSession(r)
		if !ok {
			s.renderError(w, http.StatusUnauthorized, "Please log in.")
			return
		}
		if err := s.checkPost(r, raw); err != nil {
			s.log.Warn("POST rejected", "path", r.URL.Path, "reason", err, "user", sess.display())
			s.renderError(w, http.StatusForbidden, "Request rejected: "+err.Error()+".")
			return
		}
		h(w, r, sess, raw)
	}
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request, sess *session, raw string) {
	var src escl.InputSource
	switch r.PostFormValue("source") {
	case "feeder":
		src = escl.Feeder
	case "platen":
		src = escl.Platen
	default:
		s.renderError(w, http.StatusBadRequest, "Unknown source.")
		return
	}
	var mode string // empty: SCAN_COLOR_MODE
	switch r.PostFormValue("color") {
	case "gray":
		mode = "Grayscale8"
	case "color":
		mode = "RGB24"
	case "":
	default:
		s.renderError(w, http.StatusBadRequest, "Unknown color mode.")
		return
	}
	switch err := s.scanner.Scan(src, mode); {
	case errors.Is(err, autoscan.ErrBusy):
		s.renderIndex(w, http.StatusConflict, sess, raw, "A scan is already running.")
		return
	case err != nil:
		s.renderIndex(w, http.StatusServiceUnavailable, sess, raw, "Cannot scan: "+err.Error()+".")
		return
	}
	s.log.Info("manual scan requested", "source", src, "color", mode, "user", sess.display())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request, sess *session, _ string) {
	switch r.PostFormValue("action") {
	case "pause":
		s.scanner.SetPaused(true)
	case "resume":
		s.scanner.SetPaused(false)
	default:
		s.renderError(w, http.StatusBadRequest, "Unknown action.")
		return
	}
	s.log.Info("autoscan pause changed", "action", r.PostFormValue("action"), "user", sess.display())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleDoubleSided(w http.ResponseWriter, r *http.Request, sess *session, _ string) {
	var err error
	switch r.PostFormValue("action") {
	case "start":
		err = s.scanner.StartDoubleSided(true)
	case "cancel":
		err = s.scanner.StartDoubleSided(false)
	default:
		s.renderError(w, http.StatusBadRequest, "Unknown action.")
		return
	}
	if err != nil {
		s.renderError(w, http.StatusConflict, "Double-sided: "+err.Error()+".")
		return
	}
	s.log.Info("double-sided changed", "action", r.PostFormValue("action"), "user", sess.display())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) renderError(w http.ResponseWriter, code int, msg string) {
	s.render(w, code, "error.html", struct {
		Code    int
		Message string
		Auth    bool
	}{code, msg, s.oidc != nil})
}

func (s *Server) render(w http.ResponseWriter, code int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}
