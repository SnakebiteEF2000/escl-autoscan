package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Purposes for HMAC domain separation: a value signed for one purpose is
// never valid for another.
const (
	purposeSession = "session"
	purposeLogin   = "login"
	purposeCSRF    = "csrf"
)

const loginTTL = 10 * time.Minute

// signer signs small payloads with HMAC-SHA256.
type signer struct{ key []byte }

func (s signer) mac(purpose string, data []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write(data)
	return m.Sum(nil)
}

func (s signer) sign(purpose string, payload []byte) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(s.mac(purpose, payload))
}

func (s signer) verify(purpose, value string) ([]byte, bool) {
	enc := base64.RawURLEncoding
	p, m, ok := strings.Cut(value, ".")
	if !ok {
		return nil, false
	}
	payload, err1 := enc.DecodeString(p)
	sig, err2 := enc.DecodeString(m)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, s.mac(purpose, payload)) {
		return nil, false
	}
	return payload, true
}

// session is the content of the session cookie.
type session struct {
	ID     string   `json:"sid"` // random; makes every session (and its CSRF token) unique
	Sub    string   `json:"sub"`
	Name   string   `json:"name,omitempty"`
	Email  string   `json:"email,omitempty"`
	Groups []string `json:"groups,omitempty"`
	Exp    int64    `json:"exp"`
}

// display returns the best human-readable name.
func (s *session) display() string {
	for _, v := range []string{s.Name, s.Email, s.Sub} {
		if v != "" {
			return v
		}
	}
	return "anonymous"
}

// loginState is the content of the short-lived cookie between /auth/login
// and /auth/callback.
type loginState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	Exp      int64  `json:"exp"`
}

// stringList accepts a JSON array of strings or a single string, because
// providers differ in how they send a one-element groups claim.
type stringList []string

func (l *stringList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*l = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("groups claim is neither a string nor a list of strings")
	}
	*l = many
	return nil
}

type idClaims struct {
	Sub               string     `json:"sub"`
	Name              string     `json:"name"`
	PreferredUsername string     `json:"preferred_username"`
	Email             string     `json:"email"`
	Groups            stringList `json:"groups"`
}

// OIDCConfig configures login with an OpenID Connect provider.
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string // empty for a public client (PKCE only)
	RedirectURL   string // https://scanner.example.com/auth/callback
	Scopes        []string
	AllowedGroups []string // empty: every authenticated user
}

// oidcAuth runs the authorization code flow with PKCE (S256).
type oidcAuth struct {
	cfg OIDCConfig
	log *slog.Logger

	mu       sync.Mutex // guards the lazily discovered provider
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// setup discovers the provider once. Discovery is lazy so that an
// unreachable provider does not stop automatic scanning.
func (a *oidcAuth) setup(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oauth != nil {
		return a.oauth, a.verifier, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	a.oauth = &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.ClientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  a.cfg.RedirectURL,
		Scopes:       a.cfg.Scopes,
	}
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.cfg.ClientID})
	return a.oauth, a.verifier, nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	oc, _, err := s.oidc.setup(r.Context())
	if err != nil {
		s.log.Error("login unavailable", "err", err)
		s.renderError(w, http.StatusServiceUnavailable, "The login provider is not reachable. Try again later.")
		return
	}
	st := loginState{
		State:    rand.Text(),
		Nonce:    rand.Text(),
		Verifier: oauth2.GenerateVerifier(),
		Exp:      s.now().Add(loginTTL).Unix(),
	}
	payload, _ := json.Marshal(st)
	s.setCookie(w, s.loginCookie, s.signer.sign(purposeLogin, payload), loginTTL)
	http.Redirect(w, r, oc.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	st, ok := s.readLoginState(r)
	s.clearCookie(w, s.loginCookie)
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.log.Info("login refused by provider", "error", e, "description", q.Get("error_description"))
		s.renderError(w, http.StatusForbidden, "Login failed: "+e)
		return
	}
	if !ok || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		s.log.Info("login callback with missing or wrong state")
		s.renderError(w, http.StatusBadRequest, "Login expired or invalid. Please log in again.")
		return
	}
	oc, verifier, err := s.oidc.setup(r.Context())
	if err != nil {
		s.log.Error("login unavailable", "err", err)
		s.renderError(w, http.StatusServiceUnavailable, "The login provider is not reachable. Try again later.")
		return
	}
	tok, err := oc.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.log.Warn("token exchange failed", "err", err)
		s.renderError(w, http.StatusForbidden, "Login failed.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		s.log.Warn("ID token rejected", "err", err)
		s.renderError(w, http.StatusForbidden, "Login failed.")
		return
	}
	// go-oidc does not check the nonce; that is the caller's job.
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		s.log.Warn("ID token nonce mismatch")
		s.renderError(w, http.StatusForbidden, "Login failed.")
		return
	}
	var c idClaims
	if err := idt.Claims(&c); err != nil {
		s.log.Warn("cannot read ID token claims", "err", err)
		s.renderError(w, http.StatusForbidden, "Login failed.")
		return
	}
	if !s.groupAllowed(c.Groups) {
		s.log.Info("login denied: no allowed group", "sub", c.Sub, "groups", []string(c.Groups))
		s.renderError(w, http.StatusForbidden, "Your account is not allowed to use this scanner.")
		return
	}
	name := c.Name
	if name == "" {
		name = c.PreferredUsername
	}
	sess := session{ID: rand.Text(), Sub: c.Sub, Name: name, Email: c.Email, Groups: c.Groups, Exp: s.now().Add(s.cfg.SessionTTL).Unix()}
	payload, _ := json.Marshal(sess)
	s.setCookie(w, s.sessionCookie, s.signer.sign(purposeSession, payload), s.cfg.SessionTTL)
	s.log.Info("login", "sub", c.Sub, "name", sess.display())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) groupAllowed(groups []string) bool {
	if len(s.oidc.cfg.AllowedGroups) == 0 {
		return true
	}
	for _, g := range groups {
		if slices.Contains(s.oidc.cfg.AllowedGroups, g) {
			return true
		}
	}
	return false
}

func (s *Server) handleLogout(w http.ResponseWriter, _ *http.Request, sess *session, _ string) {
	s.log.Info("logout", "user", sess.display())
	s.clearCookie(w, s.sessionCookie)
	s.render(w, http.StatusOK, "loggedout.html", nil)
}

func (s *Server) readLoginState(r *http.Request) (loginState, bool) {
	var st loginState
	c, err := r.Cookie(s.loginCookie)
	if err != nil {
		return st, false
	}
	payload, ok := s.signer.verify(purposeLogin, c.Value)
	if !ok || json.Unmarshal(payload, &st) != nil || s.now().Unix() > st.Exp || st.State == "" {
		return st, false
	}
	return st, true
}

// currentSession returns the session of the request and the raw cookie
// value, which the CSRF token is bound to. Without OIDC every request has
// an anonymous session.
func (s *Server) currentSession(r *http.Request) (*session, string, bool) {
	if s.oidc == nil {
		return &session{}, "anonymous", true
	}
	c, err := r.Cookie(s.sessionCookie)
	if err != nil {
		return nil, "", false
	}
	payload, ok := s.signer.verify(purposeSession, c.Value)
	var sess session
	if !ok || json.Unmarshal(payload, &sess) != nil || s.now().Unix() > sess.Exp || sess.Sub == "" {
		return nil, "", false
	}
	// Re-check groups: the allowed list may have changed since login.
	if !s.groupAllowed(sess.Groups) {
		return nil, "", false
	}
	return &sess, c.Value, true
}

func (s *Server) csrfToken(sessionValue string) string {
	return base64.RawURLEncoding.EncodeToString(s.signer.mac(purposeCSRF, []byte(sessionValue)))
}

// checkPost verifies the CSRF token and the request origin of a POST.
func (s *Server) checkPost(r *http.Request, sessionValue string) error {
	if !s.sameOrigin(r) {
		return errors.New("cross-origin request")
	}
	want := s.csrfToken(sessionValue)
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(want)) != 1 {
		return errors.New("missing or invalid CSRF token")
	}
	return nil
}

// sameOrigin checks Origin (or, if absent, Referer) against the origin of
// OIDC_REDIRECT_URL. Without OIDC there is no configured origin; then the
// host must match the request's Host.
func (s *Server) sameOrigin(r *http.Request) bool {
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" || src == "null" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	if s.origin != "" {
		return u.Scheme+"://"+u.Host == s.origin
	}
	return u.Host == r.Host
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: !s.cfg.CookieInsecure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: !s.cfg.CookieInsecure, SameSite: http.SameSiteLaxMode,
	})
}
