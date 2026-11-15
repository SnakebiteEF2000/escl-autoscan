// Package escl is a small client for the eSCL (AirPrint scan) protocol, with
// the quirks needed by Epson devices built in.
package escl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults for Options.
const (
	DefaultTimeout          = 10 * time.Second
	DefaultDocumentTimeout  = 5 * time.Minute
	DefaultMaxDocumentBytes = 256 << 20
	DefaultRetries          = 30 // NextDocument 503 retries, as in sane-airscan
	DefaultRetryInterval    = time.Second
)

// Errors returned by the client.
var (
	// ErrBusy means the device refused a new job (HTTP 503 or 409).
	ErrBusy = errors.New("scanner busy")
	// ErrJobAborted means the device aborted the job (HTTP 410 on
	// NextDocument), e.g. after a paper jam. Documents received earlier in
	// the same job may be truncated and must not be delivered.
	ErrJobAborted = errors.New("scan job aborted by the scanner")
	// ErrNoMoreDocuments means the job has no further documents (HTTP 404).
	ErrNoMoreDocuments = errors.New("no more documents")
	// ErrNotReady means the next document is not ready yet (HTTP 503).
	ErrNotReady = errors.New("document not ready")
	// ErrDocumentTooLarge means a document exceeded Options.MaxDocumentBytes.
	ErrDocumentTooLarge = errors.New("document too large")
)

// StatusError is an unexpected HTTP status from the device.
type StatusError struct {
	Op   string
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: unexpected HTTP status %d", e.Op, e.Code)
}

// Options configures a Client.
type Options struct {
	// URL is the device base URL, e.g. https://192.0.2.10. A missing port is
	// made explicit (see NormalizeURL).
	URL string
	// Fingerprint pins the SHA-256 fingerprint of the device's leaf
	// certificate (hex, colons optional). Epson devices use self-signed
	// certificates, so either Fingerprint or Insecure is normally needed.
	Fingerprint string
	// Insecure disables certificate verification entirely.
	Insecure bool
	// Timeout bounds status, capabilities, job creation and deletion.
	Timeout time.Duration
	// DocumentTimeout bounds one NextDocument request. The WF-2960 holds the
	// first request of a feeder job open until all sheets are scanned (about
	// 15 s per sheet), so this must be generous.
	DocumentTimeout time.Duration
	// MaxDocumentBytes caps the size of one document held in memory.
	MaxDocumentBytes int64
	// Retries and RetryInterval control how often NextDocument is retried
	// while the device answers 503.
	Retries       int
	RetryInterval time.Duration

	// dial overrides the transport's dialer (tests only).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Client talks to one eSCL device.
type Client struct {
	base *url.URL
	hc   *http.Client
	o    Options
}

// New validates the options and returns a client.
func New(o Options) (*Client, error) {
	base, err := NormalizeURL(o.URL)
	if err != nil {
		return nil, err
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.DocumentTimeout <= 0 {
		o.DocumentTimeout = DefaultDocumentTimeout
	}
	if o.MaxDocumentBytes <= 0 {
		o.MaxDocumentBytes = DefaultMaxDocumentBytes
	}
	if o.Retries <= 0 {
		o.Retries = DefaultRetries
	}
	if o.RetryInterval <= 0 {
		o.RetryInterval = DefaultRetryInterval
	}
	tlsConf, err := tlsConfig(o.Fingerprint, o.Insecure)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: o.Timeout, KeepAlive: 30 * time.Second}
	// No ResponseHeaderTimeout: NextDocument blocks while the device scans.
	// Every request is bounded by its own context instead (see request).
	tr := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSClientConfig:     tlsConf,
		TLSHandshakeTimeout: o.Timeout,
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
	}
	if o.dial != nil {
		tr.DialContext = o.dial
	}
	return &Client{
		base: base,
		o:    o,
		hc: &http.Client{
			Transport: tr,
			// Never follow redirects: they would drop the explicit port.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// BaseURL returns the normalized device URL.
func (c *Client) BaseURL() string { return c.base.String() }

// NormalizeURL parses a device URL and makes its port explicit.
//
// Epson devices need the port in the Host header (sane-airscan calls this
// quirk_port_in_host): without it they return a broken job Location. Go sends
// the URL's host verbatim, so an explicit ":443" in the URL ends up in the
// Host header.
func NormalizeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid scanner URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("invalid scanner URL %q: scheme must be https or http", raw)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("invalid scanner URL %q: missing host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid scanner URL %q: must not contain credentials, query or fragment", raw)
	}
	if u.Port() == "" {
		port := "443"
		if u.Scheme == "http" {
			port = "80"
		}
		u.Host = net.JoinHostPort(u.Hostname(), port)
	}
	// Accept both https://host and https://host/eSCL as base.
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/eSCL")
	u.RawPath = ""
	return u, nil
}

// ParseFingerprint decodes a SHA-256 fingerprint in hex, with or without
// colons, in any case.
func ParseFingerprint(s string) ([]byte, error) {
	clean := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	fp, err := hex.DecodeString(clean)
	if err != nil || len(fp) != sha256.Size {
		return nil, errors.New("invalid TLS fingerprint: want 64 hex digits (SHA-256), colons optional")
	}
	return fp, nil
}

func tlsConfig(fingerprint string, insecure bool) (*tls.Config, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case fingerprint != "" && insecure:
		return nil, errors.New("set either a TLS fingerprint or insecure mode, not both")
	case fingerprint != "":
		want, err := ParseFingerprint(fingerprint)
		if err != nil {
			return nil, err
		}
		// Chain verification is replaced by comparing the leaf certificate
		// with the pinned hash. VerifyConnection runs on every handshake,
		// including resumed sessions (unlike VerifyPeerCertificate).
		conf.InsecureSkipVerify = true
		conf.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("scanner presented no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return fmt.Errorf("scanner certificate fingerprint mismatch: got %s", FormatFingerprint(got[:]))
			}
			return nil
		}
	case insecure:
		conf.InsecureSkipVerify = true
	}
	return conf, nil
}

// FormatFingerprint renders a fingerprint as colon-separated upper-case hex.
func FormatFingerprint(fp []byte) string {
	parts := make([]string, len(fp))
	for i, b := range fp {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// Status fetches /eSCL/ScannerStatus.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var st Status
	if err := c.getXML(ctx, "status", "/eSCL/ScannerStatus", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Capabilities fetches /eSCL/ScannerCapabilities.
func (c *Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	var caps Capabilities
	if err := c.getXML(ctx, "capabilities", "/eSCL/ScannerCapabilities", &caps); err != nil {
		return nil, err
	}
	return &caps, nil
}

func (c *Client) getXML(ctx context.Context, op, path string, v any) error {
	resp, err := c.request(ctx, c.o.Timeout, http.MethodGet, c.endpoint(path), nil, 1<<20)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if resp.code != http.StatusOK {
		return &StatusError{Op: op, Code: resp.code}
	}
	if err := xml.Unmarshal(resp.body, v); err != nil {
		return fmt.Errorf("%s: parse: %w", op, err)
	}
	return nil
}

func (c *Client) endpoint(path string) string {
	u := *c.base
	u.Path = c.base.Path + path
	return u.String()
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

// request performs one HTTP request with its own timeout and reads at most
// limit bytes of the body. On a body read error the partial body is returned
// together with the error.
func (c *Client) request(ctx context.Context, timeout time.Duration, method, u string, body []byte, limit int64) (*response, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/xml")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	out := &response{code: res.StatusCode, header: res.Header, body: data}
	if err != nil {
		return out, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > limit {
		out.body = data[:limit]
		return out, ErrDocumentTooLarge
	}
	return out, nil
}
