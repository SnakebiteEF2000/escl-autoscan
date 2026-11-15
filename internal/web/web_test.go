package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/autoscan"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
)

// fakeScanner implements Scanner.
type fakeScanner struct {
	mu     sync.Mutex
	busy   bool
	paused bool
	scans  []string
}

func (f *fakeScanner) Snapshot() autoscan.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return autoscan.Snapshot{Reachable: true, State: "Idle", AdfState: "ScannerAdfEmpty", AutoscanEnabled: true,
		Armed: true, Paused: f.paused, ColorMode: "Grayscale8",
		History: []autoscan.Record{{Time: time.Unix(1_700_000_000, 0), Source: "feeder", Trigger: "auto", Pages: 3,
			Result: "ok", Files: []string{"scan-20231114-221320.pdf"}}}}
}

func (f *fakeScanner) Scan(src escl.InputSource, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return autoscan.ErrBusy
	}
	f.scans = append(f.scans, string(src)+"/"+mode)
	return nil
}

func (f *fakeScanner) SetPaused(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = p
}

func (f *fakeScanner) setBusy(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = b
}

func (f *fakeScanner) isPaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

func (f *fakeScanner) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.scans...)
}

// provider is a fake OpenID Connect provider.
type provider struct {
	*httptest.Server
	t   *testing.T
	key *rsa.PrivateKey

	mu         sync.Mutex
	groups     any    // groups claim; nil: none
	nonce      string // override for the nonce claim
	challenges map[string]string
	authorize  url.Values // last authorization request
	verifiers  []string   // code_verifier values received
}

var (
	keyOnce sync.Once
	rsaKey  *rsa.PrivateKey
)

func newProvider(t *testing.T) *provider {
	keyOnce.Do(func() {
		var err error
		if rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	p := &provider{t: t, key: rsaKey, challenges: map[string]string{}, groups: []string{"admin"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/authorize",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(p.key.N.Bytes()), "e": b64(big.NewInt(int64(p.key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := rand.Text()
		p.mu.Lock()
		p.authorize = q
		p.challenges[code] = q.Get("code_challenge") + "|" + q.Get("nonce")
		p.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		defer p.mu.Unlock()
		v := r.PostForm.Get("code_verifier")
		p.verifiers = append(p.verifiers, v)
		challenge, nonce, _ := strings.Cut(p.challenges[r.PostForm.Get("code")], "|")
		sum := sha256.Sum256([]byte(v))
		if challenge == "" || b64(sum[:]) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant"})
			return
		}
		if p.nonce != "" {
			nonce = p.nonce
		}
		claims := map[string]any{
			"iss": p.URL, "sub": "user-1", "aud": "escl-autoscan", "nonce": nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
			"name": "Test User", "email": "user@example.com",
		}
		if p.groups != nil {
			claims["groups"] = p.groups
		}
		writeJSON(w, map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": p.sign(claims),
		})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *provider) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	signing := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return signing + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ui is a UI server with a browser-like client.
type ui struct {
	*httptest.Server
	t       *testing.T
	scanner *fakeScanner
	client  *http.Client
	srv     *Server
}

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newUI(t *testing.T, mod func(*Config, string)) *ui {
	t.Helper()
	u := &ui{t: t, scanner: &fakeScanner{}}
	var h http.Handler
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(u.Close)
	cfg := Config{SessionKey: testKey, CookieInsecure: true}
	mod(&cfg, u.URL)
	srv, err := New(cfg, u.scanner, slog.New(slog.NewTextHandler(t.Output(), nil)))
	if err != nil {
		t.Fatal(err)
	}
	u.srv, h = srv, srv
	jar, _ := cookiejar.New(nil)
	u.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return u
}

func withOIDC(p *provider, groups ...string) func(*Config, string) {
	return func(c *Config, uiURL string) {
		c.OIDC = &OIDCConfig{
			Issuer: p.URL, ClientID: "escl-autoscan", ClientSecret: "secret",
			RedirectURL: uiURL + "/auth/callback", AllowedGroups: groups,
		}
	}
}

func (u *ui) get(target string) *http.Response {
	u.t.Helper()
	resp, err := u.client.Get(target)
	if err != nil {
		u.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// login runs the authorization code flow and returns the callback response.
func (u *ui) login() *http.Response {
	u.t.Helper()
	resp := u.get(u.URL + "/")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/auth/login" {
		u.t.Fatalf("GET / without session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = u.get(u.URL + "/auth/login")
	if resp.StatusCode != http.StatusFound {
		u.t.Fatalf("login: %d", resp.StatusCode)
	}
	resp = u.get(resp.Header.Get("Location")) // provider authorize
	return u.get(resp.Header.Get("Location")) // callback
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (u *ui) page() (int, string) {
	u.t.Helper()
	resp, err := u.client.Get(u.URL + "/")
	if err != nil {
		u.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (u *ui) csrf() string {
	u.t.Helper()
	code, body := u.page()
	m := csrfRe.FindStringSubmatch(body)
	if code != http.StatusOK || m == nil {
		u.t.Fatalf("index: %d, no CSRF token", code)
	}
	return m[1]
}

func (u *ui) post(path string, form url.Values, origin string) *http.Response {
	u.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestLoginFlowWithPKCE(t *testing.T) {
	p := newProvider(t)
	u := newUI(t, withOIDC(p, "admin"))

	resp := u.login()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	p.mu.Lock()
	q, verifiers := p.authorize, p.verifiers
	p.mu.Unlock()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Errorf("authorization request lacks PKCE/nonce/state: %v", q)
	}
	if q.Get("scope") != "openid profile email groups" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	if len(verifiers) != 1 || verifiers[0] == "" {
		t.Errorf("token request without code_verifier: %v", verifiers)
	}

	code, body := u.page()
	if code != http.StatusOK || !strings.Contains(body, "Test User") || !strings.Contains(body, "scan-20231114-221320.pdf") {
		t.Fatalf("index: %d\n%s", code, body)
	}

	csrf := u.csrf()
	resp = u.post("/scan", url.Values{"csrf": {csrf}, "source": {"platen"}, "color": {"color"}}, u.URL)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /scan: %d", resp.StatusCode)
	}
	if got := strings.Join(u.scanner.calls(), ","); got != "Platen/RGB24" {
		t.Errorf("scans = %s", got)
	}
	if resp := u.post("/pause", url.Values{"csrf": {csrf}, "action": {"pause"}}, u.URL); resp.StatusCode != http.StatusSeeOther || !u.scanner.isPaused() {
		t.Errorf("POST /pause: %d paused=%v", resp.StatusCode, u.scanner.isPaused())
	}

	u.scanner.setBusy(true)
	req, _ := http.NewRequest(http.MethodPost, u.URL+"/scan", strings.NewReader(url.Values{"csrf": {csrf}, "source": {"feeder"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", u.URL)
	busy, err := u.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	busyBody, _ := io.ReadAll(busy.Body)
	busy.Body.Close()
	if busy.StatusCode != http.StatusConflict || !strings.Contains(string(busyBody), "already running") {
		t.Errorf("busy scan: %d, want 409 with message", busy.StatusCode)
	}
	// The 409 page is served at /scan; its refresh must go to /, not GET /scan.
	if !strings.Contains(string(busyBody), `content="30; url=/"`) {
		t.Errorf("busy page refresh does not target /")
	}

	if resp := u.post("/auth/logout", url.Values{"csrf": {csrf}}, u.URL); resp.StatusCode != http.StatusOK {
		t.Errorf("logout: %d", resp.StatusCode)
	}
	if resp := u.get(u.URL + "/"); resp.StatusCode != http.StatusFound {
		t.Errorf("after logout: %d, want redirect to login", resp.StatusCode)
	}
}

func TestGroups(t *testing.T) {
	tests := []struct {
		name    string
		claim   any
		allowed []string
		want    int
	}{
		{"allowed list", []string{"family", "admin"}, []string{"admin"}, http.StatusSeeOther},
		{"single string claim", "admin", []string{"admin"}, http.StatusSeeOther},
		{"denied", []string{"family"}, []string{"admin"}, http.StatusForbidden},
		{"no groups claim", nil, []string{"admin"}, http.StatusForbidden},
		{"no restriction", nil, nil, http.StatusSeeOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t)
			p.groups = tt.claim
			u := newUI(t, withOIDC(p, tt.allowed...))
			if resp := u.login(); resp.StatusCode != tt.want {
				t.Fatalf("callback: %d, want %d", resp.StatusCode, tt.want)
			}
			code, _ := u.page()
			if (tt.want == http.StatusSeeOther) != (code == http.StatusOK) {
				t.Errorf("index after login: %d", code)
			}
		})
	}
}

func TestCallbackRejections(t *testing.T) {
	t.Run("nonce mismatch", func(t *testing.T) {
		p := newProvider(t)
		p.nonce = "something-else"
		u := newUI(t, withOIDC(p))
		if resp := u.login(); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("callback: %d, want 403", resp.StatusCode)
		}
	})
	t.Run("state mismatch", func(t *testing.T) {
		p := newProvider(t)
		u := newUI(t, withOIDC(p))
		resp := u.get(u.URL + "/auth/login")
		resp = u.get(resp.Header.Get("Location"))
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		q.Set("state", "forged")
		loc.RawQuery = q.Encode()
		if resp := u.get(loc.String()); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("callback: %d, want 400", resp.StatusCode)
		}
	})
	t.Run("no login cookie", func(t *testing.T) {
		p := newProvider(t)
		u := newUI(t, withOIDC(p))
		if resp := u.get(u.URL + "/auth/callback?code=x&state=y"); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("callback: %d, want 400", resp.StatusCode)
		}
	})
	t.Run("provider error", func(t *testing.T) {
		p := newProvider(t)
		u := newUI(t, withOIDC(p))
		u.get(u.URL + "/auth/login")
		if resp := u.get(u.URL + "/auth/callback?error=access_denied"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("callback: %d, want 403", resp.StatusCode)
		}
	})
}

func TestPostProtection(t *testing.T) {
	p := newProvider(t)
	u := newUI(t, withOIDC(p, "admin"))
	u.login()
	csrf := u.csrf()
	scan := func(csrf, origin string) int {
		return u.post("/scan", url.Values{"csrf": {csrf}, "source": {"feeder"}}, origin).StatusCode
	}

	if code := scan("", u.URL); code != http.StatusForbidden {
		t.Errorf("missing CSRF: %d", code)
	}
	if code := scan("bogus", u.URL); code != http.StatusForbidden {
		t.Errorf("invalid CSRF: %d", code)
	}
	if code := scan(csrf, "https://evil.example.com"); code != http.StatusForbidden {
		t.Errorf("cross-origin: %d", code)
	}
	if code := scan(csrf, ""); code != http.StatusForbidden {
		t.Errorf("no Origin/Referer: %d", code)
	}
	// Referer instead of Origin.
	req, _ := http.NewRequest(http.MethodPost, u.URL+"/scan", strings.NewReader(url.Values{"csrf": {csrf}, "source": {"feeder"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", u.URL+"/")
	resp, err := u.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("same-origin Referer: %d", resp.StatusCode)
	}
	if len(u.scanner.calls()) != 1 {
		t.Errorf("scans = %v, want only the valid request", u.scanner.calls())
	}

	// A token from another session is useless.
	other := newUI(t, withOIDC(p, "admin"))
	other.login()
	if code := scan(other.csrf(), u.URL); code != http.StatusForbidden {
		t.Errorf("CSRF of another session: %d", code)
	}

	// Without a session: 401, nothing scanned.
	anon := newUI(t, withOIDC(p, "admin"))
	if code := anon.post("/scan", url.Values{"csrf": {csrf}, "source": {"feeder"}}, anon.URL).StatusCode; code != http.StatusUnauthorized {
		t.Errorf("no session: %d", code)
	}
}

func TestSessionCookieTamperAndExpiry(t *testing.T) {
	p := newProvider(t)
	u := newUI(t, withOIDC(p, "admin"))
	u.login()
	base, _ := url.Parse(u.URL)
	cookies := u.client.Jar.Cookies(base)
	if len(cookies) != 1 || cookies[0].Name != "escl_session" {
		t.Fatalf("cookies = %v", cookies)
	}
	payload, sig, _ := strings.Cut(cookies[0].Value, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	forged := strings.Replace(string(raw), `"user-1"`, `"user-2"`, 1)
	jar, _ := cookiejar.New(nil)
	jar.SetCookies(base, []*http.Cookie{{Name: "escl_session", Value: b64([]byte(forged)) + "." + sig}})
	u.client.Jar = jar
	if code, _ := u.page(); code != http.StatusFound {
		t.Errorf("forged session: %d, want redirect", code)
	}

	// A login-state cookie is not a session cookie (domain separation).
	st, _ := json.Marshal(session{Sub: "user-1", Exp: time.Now().Add(time.Hour).Unix(), Groups: []string{"admin"}})
	jar.SetCookies(base, []*http.Cookie{{Name: "escl_session", Value: u.srv.signer.sign(purposeLogin, st)}})
	if code, _ := u.page(); code != http.StatusFound {
		t.Errorf("cookie signed for another purpose: %d", code)
	}

	u.srv.now = func() time.Time { return time.Now().Add(13 * time.Hour) }
	jar.SetCookies(base, []*http.Cookie{{Name: "escl_session", Value: u.srv.signer.sign(purposeSession, st)}})
	if code, _ := u.page(); code != http.StatusFound {
		t.Errorf("expired session: %d", code)
	}
}

func TestSecureCookieAttributes(t *testing.T) {
	p := newProvider(t)
	u := newUI(t, func(c *Config, uiURL string) {
		withOIDC(p)(c, uiURL)
		c.CookieInsecure = false
	})
	resp := u.get(u.URL + "/auth/login")
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"__Host-escl_login=", "Path=/", "HttpOnly", "Secure", "SameSite=Lax", "Max-Age=600"} {
		if !strings.Contains(sc, want) {
			t.Errorf("Set-Cookie %q lacks %q", sc, want)
		}
	}
	if strings.Contains(strings.ToLower(sc), "domain=") {
		t.Errorf("Set-Cookie %q has a Domain", sc)
	}
}

func TestConfigValidation(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &fakeScanner{}
	if _, err := New(Config{}, s, log); err == nil || !strings.Contains(err.Error(), "UI_NO_AUTH") {
		t.Errorf("no auth configured: err = %v, want refusal mentioning UI_NO_AUTH", err)
	}
	oidcCfg := &OIDCConfig{Issuer: "https://id.example.com", ClientID: "c", RedirectURL: "https://scanner.example.com/auth/callback"}
	if _, err := New(Config{OIDC: oidcCfg, SessionKey: []byte("short")}, s, log); err == nil {
		t.Error("short session key accepted")
	}
	if _, err := New(Config{OIDC: oidcCfg, NoAuth: true, SessionKey: testKey}, s, log); err == nil {
		t.Error("OIDC plus UI_NO_AUTH accepted")
	}
	if _, err := New(Config{OIDC: &OIDCConfig{Issuer: "https://id.example.com", ClientID: "c", RedirectURL: "/relative"}, SessionKey: testKey}, s, log); err == nil {
		t.Error("relative redirect URL accepted")
	}
	srv, err := New(Config{OIDC: oidcCfg, SessionKey: testKey}, s, log)
	if err != nil {
		t.Fatal(err)
	}
	if srv.origin != "https://scanner.example.com" {
		t.Errorf("origin = %q", srv.origin)
	}
}

func TestNoAuthMode(t *testing.T) {
	u := newUI(t, func(c *Config, _ string) { c.NoAuth = true; c.SessionKey = nil })
	code, body := u.page()
	if code != http.StatusOK || strings.Contains(body, "Log out") {
		t.Fatalf("index: %d", code)
	}
	csrf := u.csrf()
	if resp := u.post("/scan", url.Values{"csrf": {csrf}, "source": {"feeder"}, "color": {"gray"}}, u.URL); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("POST /scan: %d", resp.StatusCode)
	}
	if resp := u.post("/scan", url.Values{"source": {"feeder"}}, u.URL); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF: %d", resp.StatusCode)
	}
	if resp := u.post("/scan", url.Values{"csrf": {csrf}, "source": {"feeder"}}, "http://evil.example.com"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", resp.StatusCode)
	}
	if resp := u.get(u.URL + "/auth/login"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/auth/login without OIDC: %d", resp.StatusCode)
	}
	if got := strings.Join(u.scanner.calls(), ","); got != "Feeder/Grayscale8" {
		t.Errorf("scans = %s", got)
	}
}

func TestMetricsOnlyOnMetricsListener(t *testing.T) {
	u := newUI(t, func(c *Config, _ string) { c.NoAuth = true })
	for _, path := range []string{"/metrics", "/healthz", "/readyz"} {
		if resp := u.get(u.URL + path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("UI %s: %d, want 404", path, resp.StatusCode)
		}
	}
	if resp := u.get(u.URL + "/static/style.css"); resp.StatusCode != http.StatusOK {
		t.Errorf("stylesheet: %d", resp.StatusCode)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "escl_autoscan_test"}))
	var notReady error
	m := httptest.NewServer(MetricsHandler(reg, func() error { return notReady }))
	defer m.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(m.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/metrics"); code != http.StatusOK || !strings.Contains(body, "escl_autoscan_test") {
		t.Errorf("/metrics: %d", code)
	}
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Errorf("/healthz: %d", code)
	}
	if code, _ := get("/readyz"); code != http.StatusOK {
		t.Errorf("/readyz: %d", code)
	}
	notReady = errors.New("output dir missing")
	if code, _ := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz not ready: %d", code)
	}
}

func TestStringListClaim(t *testing.T) {
	var c idClaims
	for in, want := range map[string]string{
		`{"groups":["a","b"]}`: "a,b",
		`{"groups":"a"}`:       "a",
		`{}`:                   "",
	} {
		c = idClaims{}
		if err := json.Unmarshal([]byte(in), &c); err != nil || strings.Join(c.Groups, ",") != want {
			t.Errorf("%s: %v %v", in, c.Groups, err)
		}
	}
	if err := json.Unmarshal([]byte(`{"groups":42}`), &c); err == nil {
		t.Error("numeric groups claim accepted")
	}
}
