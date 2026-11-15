package escl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escltest"
)

func fingerprintOf(t *testing.T, s *escltest.Server) []byte {
	t.Helper()
	sum := sha256.Sum256(s.Certificate().Raw)
	return sum[:]
}

func newClient(t *testing.T, s *escltest.Server, mod func(*Options)) *Client {
	t.Helper()
	o := Options{
		URL:           s.URL,
		Fingerprint:   hex.EncodeToString(fingerprintOf(t, s)),
		Timeout:       2 * time.Second,
		RetryInterval: time.Millisecond,
	}
	if mod != nil {
		mod(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct{ in, want, err string }{
		{in: "https://192.0.2.10", want: "https://192.0.2.10:443"},
		{in: "https://192.0.2.10/", want: "https://192.0.2.10:443"},
		{in: "https://192.0.2.10/eSCL", want: "https://192.0.2.10:443"},
		{in: "https://192.0.2.10:443/eSCL/", want: "https://192.0.2.10:443"},
		{in: "https://scanner.example.com:8443", want: "https://scanner.example.com:8443"},
		{in: "http://192.0.2.10", want: "http://192.0.2.10:80"},
		{in: "https://[2001:db8::10]", want: "https://[2001:db8::10]:443"},
		{in: "  https://192.0.2.10  ", want: "https://192.0.2.10:443"},
		{in: "192.0.2.10", err: "scheme"},
		{in: "ftp://192.0.2.10", err: "scheme"},
		{in: "https://user:pw@192.0.2.10", err: "credentials"},
		{in: "https://", err: "host"},
	}
	for _, tt := range tests {
		u, err := NormalizeURL(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("NormalizeURL(%q) err = %v, want %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || u.String() != tt.want {
			t.Errorf("NormalizeURL(%q) = %v, %v; want %q", tt.in, u, err, tt.want)
		}
	}
}

// The configured URL has no port; the Host header must still carry ":443"
// (Epson quirk). The dialer is redirected to the fake, so the URL host stays
// the device address.
func TestHostHeaderCarriesPort(t *testing.T) {
	s := escltest.New(t)
	s.QueueJob(escltest.Step{Body: []byte("%PDF-")})
	c := newClient(t, s, func(o *Options) {
		o.URL = "https://192.0.2.10"
		o.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, s.Listener.Addr().String())
		}
	})
	ctx := context.Background()
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := c.CreateJob(ctx, ScanSettings{Source: Feeder, Intent: "Document", ColorMode: "Grayscale8", Resolution: 300})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(job, "https://192.0.2.10:443/eSCL/ScanJobs/") {
		t.Errorf("job URL = %q", job)
	}
	if _, err := c.Documents(ctx, job); err != nil {
		t.Fatal(err)
	}
	reqs := s.Requests()
	if len(reqs) < 3 {
		t.Fatalf("got %d requests", len(reqs))
	}
	for _, r := range reqs {
		if r.Host != "192.0.2.10:443" {
			t.Errorf("%s %s: Host = %q, want 192.0.2.10:443", r.Method, r.Path, r.Host)
		}
	}
}

func TestFingerprintPinning(t *testing.T) {
	s := escltest.New(t)
	fp := fingerprintOf(t, s)
	ctx := context.Background()

	for _, pin := range []string{hex.EncodeToString(fp), FormatFingerprint(fp), strings.ToLower(FormatFingerprint(fp))} {
		c := newClient(t, s, func(o *Options) { o.Fingerprint = pin })
		if _, err := c.Status(ctx); err != nil {
			t.Errorf("pin %q: %v", pin, err)
		}
	}

	wrong := bytes.Clone(fp)
	wrong[0] ^= 0xff
	c := newClient(t, s, func(o *Options) { o.Fingerprint = hex.EncodeToString(wrong) })
	if _, err := c.Status(ctx); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Errorf("wrong pin: err = %v, want fingerprint mismatch", err)
	}

	// Neither pin nor insecure: normal verification fails for a self-signed cert.
	c = newClient(t, s, func(o *Options) { o.Fingerprint = "" })
	if _, err := c.Status(ctx); err == nil {
		t.Error("unpinned self-signed certificate accepted")
	}

	c = newClient(t, s, func(o *Options) { o.Fingerprint, o.Insecure = "", true })
	if _, err := c.Status(ctx); err != nil {
		t.Errorf("insecure: %v", err)
	}

	if _, err := New(Options{URL: s.URL, Fingerprint: "abc"}); err == nil {
		t.Error("short fingerprint accepted")
	}
	if _, err := New(Options{URL: s.URL, Fingerprint: hex.EncodeToString(fp), Insecure: true}); err == nil {
		t.Error("fingerprint plus insecure accepted")
	}
}

func TestParseStatusFixtures(t *testing.T) {
	parse := func(name string) Status {
		var st Status
		if err := xml.Unmarshal(escltest.Fixture(name), &st); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return st
	}

	idle := parse("status-idle.xml")
	if idle.Version != "2.9" || idle.State != StateIdle || idle.AdfState != AdfEmpty || idle.AdfLoaded() || len(idle.Jobs) != 0 {
		t.Errorf("idle = %+v", idle)
	}
	if loaded := parse("status-loaded.xml"); !loaded.AdfLoaded() || loaded.State != StateIdle {
		t.Errorf("loaded = %+v", loaded)
	}

	jam := parse("status-jam.xml")
	if jam.State != StateStopped || jam.AdfState != AdfJam || !AdfIsFault(jam.AdfState) || len(jam.Jobs) != 2 {
		t.Fatalf("jam = %+v", jam)
	}
	j, ok := jam.Job("https://192.0.2.10:443/eSCL/ScanJobs/11111111-2222-4333-8444-555555555503")
	if !ok || j.JobState != JobAborted || j.ImagesCompleted != 2 || len(j.JobStateReasons) != 1 || j.JobStateReasons[0] != "AbortedBySystem" {
		t.Errorf("jam job = %+v, %v", j, ok)
	}

	// The device keeps old jobs in the list and sends no JobStateReasons.
	hist := parse("status-history.xml")
	if j, ok := hist.Job("/eSCL/ScanJobs/11111111-2222-4333-8444-555555555501"); !ok || j.JobState != JobCompleted || j.ImagesCompleted != 3 || j.JobStateReasons != nil {
		t.Errorf("history job 1 = %+v, %v", j, ok)
	}
	if j, ok := hist.Job("11111111-2222-4333-8444-555555555502"); !ok || j.JobState != JobAborted {
		t.Errorf("history job 2 = %+v, %v", j, ok)
	}
	if _, ok := hist.Job("https://192.0.2.10:443/eSCL/ScanJobs/00000000-0000-4000-8000-000000000009"); ok {
		t.Error("unknown job matched")
	}
}

func TestAdfStateHelpers(t *testing.T) {
	for _, s := range KnownAdfStates {
		if s == AdfLoaded {
			continue
		}
		st := Status{AdfState: s}
		if st.AdfLoaded() {
			t.Errorf("%s counted as loaded", s)
		}
	}
	if (&Status{AdfState: "ScannerAdfSomethingNew"}).AdfLoaded() {
		t.Error("unknown state counted as loaded")
	}
	if !AdfIsEmpty(AdfHopperEmpty) || AdfIsEmpty("ScannerAdfSomethingNew") || AdfIsFault(AdfEmpty) || AdfIsFault(AdfLoaded) {
		t.Error("AdfIsEmpty/AdfIsFault")
	}
}

func TestCapabilities(t *testing.T) {
	s := escltest.New(t)
	caps, err := newClient(t, s, nil).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.MakeAndModel != "EPSON WF-2960 Series" || caps.Platen == nil || caps.Adf == nil || caps.Adf.Simplex == nil {
		t.Fatalf("caps = %+v", caps)
	}
	if caps.Platen.MaxWidth != 2550 || caps.Platen.MaxHeight != 3510 {
		t.Errorf("platen = %+v", caps.Platen)
	}
	adf := caps.Adf
	if adf.Simplex.MaxWidth != 2550 || adf.Simplex.MaxHeight != 4200 || adf.FeederCapacity != 50 || adf.Duplex != nil {
		t.Errorf("adf = %+v / %+v", adf, adf.Simplex)
	}
	if adf.XImagePosition != "Right" || adf.YImagePosition != "Top" || len(adf.Options) != 1 || adf.Options[0] != "DetectPaperLoaded" {
		t.Errorf("adf justification/options = %+v", adf)
	}
	if got := strings.Join(adf.Simplex.ColorModes, ","); got != "BlackAndWhite1,Grayscale8,RGB24" {
		t.Errorf("color modes = %s", got)
	}
	if len(adf.Simplex.Resolutions) != 4 || adf.Simplex.Resolutions[3] != 600 || len(caps.Platen.Resolutions) != 5 {
		t.Errorf("resolutions adf=%v platen=%v", adf.Simplex.Resolutions, caps.Platen.Resolutions)
	}
}

// tokens returns the XML token stream without whitespace-only character data.
func tokens(t *testing.T, data []byte) []string {
	t.Helper()
	d := xml.NewDecoder(bytes.NewReader(data))
	var out []string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		switch v := tok.(type) {
		case xml.StartElement:
			out = append(out, "<"+v.Name.Space+" "+v.Name.Local)
		case xml.EndElement:
			out = append(out, ">"+v.Name.Local)
		case xml.CharData:
			if s := strings.TrimSpace(string(v)); s != "" {
				out = append(out, s)
			}
		}
	}
}

func TestScanSettingsMatchDeviceFixture(t *testing.T) {
	s := ScanSettings{
		Source: Feeder, Intent: "Document", ColorMode: "Grayscale8", Resolution: 300,
		Region: Region{Width: 2550, Height: 3508},
	}
	got, want := tokens(t, s.marshal()), tokens(t, escltest.Fixture("scan-settings-feeder.xml"))
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("settings differ from the known-good request\n got: %v\nwant: %v", got, want)
	}

	s.Source = Platen
	if body := string(s.marshal()); strings.Contains(body, "Duplex") || !strings.Contains(body, "<pwg:InputSource>Platen</pwg:InputSource>") {
		t.Errorf("platen settings = %s", body)
	}
}

func TestCreateJob(t *testing.T) {
	ctx := context.Background()
	settings := ScanSettings{Source: Feeder, Intent: "Document", ColorMode: "RGB24", Resolution: 300}

	s := escltest.New(t)
	c := newClient(t, s, nil)
	job, err := c.CreateJob(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	if want := s.URL + "/eSCL/ScanJobs/00000000-0000-4000-8000-000000000001"; job != want {
		t.Errorf("absolute Location: job = %q, want %q", job, want)
	}
	if jobs := s.Jobs(); len(jobs) != 1 || !strings.Contains(jobs[0].Settings, "<scan:ColorMode>RGB24</scan:ColorMode>") {
		t.Errorf("jobs = %+v", jobs)
	}

	s.RelativeLocation()
	job, err = c.CreateJob(ctx, settings)
	if err != nil || job != s.URL+"/eSCL/ScanJobs/00000000-0000-4000-8000-000000000002" {
		t.Errorf("relative Location: job = %q, %v", job, err)
	}

	s.QueueCreate(http.StatusServiceUnavailable, http.StatusConflict, http.StatusBadRequest)
	for _, code := range []int{503, 409} {
		if _, err := c.CreateJob(ctx, settings); !errors.Is(err, ErrBusy) {
			t.Errorf("HTTP %d: err = %v, want ErrBusy", code, err)
		}
	}
	var se *StatusError
	if _, err := c.CreateJob(ctx, settings); !errors.As(err, &se) || se.Code != 400 {
		t.Errorf("HTTP 400: err = %v", err)
	}
}

func TestDocuments(t *testing.T) {
	ctx := context.Background()
	s := escltest.New(t)
	c := newClient(t, s, func(o *Options) { o.Retries = 3 })
	settings := ScanSettings{Source: Feeder, Intent: "Document", ColorMode: "Grayscale8", Resolution: 300}

	// 503 twice, then a document, then 404.
	s.QueueJob(escltest.Step{Code: 503}, escltest.Step{Code: 503}, escltest.Step{Body: []byte("doc1"), Delay: 20 * time.Millisecond})
	job, _ := c.CreateJob(ctx, settings)
	docs, err := c.Documents(ctx, job)
	if err != nil || len(docs) != 1 || string(docs[0]) != "doc1" {
		t.Errorf("503,503,200,404: docs=%q err=%v", docs, err)
	}
	if err := c.Delete(ctx, job); err != nil {
		t.Errorf("delete after finish (404) = %v", err)
	}

	// Jam: a (truncated) document, then 410.
	s.QueueJob(escltest.Step{Body: []byte("%PDF-1.4 trunc")}, escltest.Step{Code: 410})
	job, _ = c.CreateJob(ctx, settings)
	docs, err = c.Documents(ctx, job)
	if !errors.Is(err, ErrJobAborted) || len(docs) != 1 {
		t.Errorf("200,410: docs=%d err=%v", len(docs), err)
	}

	// Never ready.
	s.QueueJob(escltest.Step{Code: 503}, escltest.Step{Code: 503}, escltest.Step{Code: 503}, escltest.Step{Code: 503})
	job, _ = c.CreateJob(ctx, settings)
	if _, err := c.Documents(ctx, job); !errors.Is(err, ErrNotReady) {
		t.Errorf("503 x4 with 3 retries: err = %v", err)
	}

	// Wake glitch shape: 404 without any document.
	job, _ = c.CreateJob(ctx, settings)
	if docs, err := c.Documents(ctx, job); err != nil || len(docs) != 0 {
		t.Errorf("404 at once: docs=%d err=%v", len(docs), err)
	}

	// Unexpected status.
	s.QueueJob(escltest.Step{Code: 500})
	job, _ = c.CreateJob(ctx, settings)
	var se *StatusError
	if _, err := c.Documents(ctx, job); !errors.As(err, &se) || se.Code != 500 {
		t.Errorf("500: err = %v", err)
	}

	// Delete of a running job.
	s.QueueJob(escltest.Step{Body: []byte("x")})
	job, _ = c.CreateJob(ctx, settings)
	if err := c.Delete(ctx, job); err != nil {
		t.Errorf("delete running job: %v", err)
	}
}

func TestDocumentLimitAndTimeout(t *testing.T) {
	ctx := context.Background()
	s := escltest.New(t)
	c := newClient(t, s, func(o *Options) {
		o.MaxDocumentBytes = 8
		o.DocumentTimeout = 50 * time.Millisecond
	})
	settings := ScanSettings{Source: Feeder, Intent: "Document", ColorMode: "Grayscale8", Resolution: 300}

	s.QueueJob(escltest.Step{Body: []byte("0123456789")})
	job, _ := c.CreateJob(ctx, settings)
	if _, err := c.Documents(ctx, job); !errors.Is(err, ErrDocumentTooLarge) {
		t.Errorf("err = %v, want ErrDocumentTooLarge", err)
	}

	s.QueueJob(escltest.Step{Body: []byte("x"), Delay: time.Second})
	job, _ = c.CreateJob(ctx, settings)
	if _, err := c.Documents(ctx, job); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}
