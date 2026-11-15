// Package autoscan watches an eSCL scanner and scans automatically when paper
// is put into the document feeder.
//
// State machine, driven by status polls:
//
//	disarmed --(ADF empty seen)--> armed
//	armed --(ADF loaded, scanner idle)--> settling (AUTOSCAN_SETTLE)
//	settling --(still loaded and idle)--> scanning --> disarmed
//	settling --(paper removed / scanner busy)--> skipped --> disarmed
//
// "Disarmed until the feeder was empty again" guarantees that a stack is never
// scanned twice and that a jam is cleared before the next automatic scan. The
// service starts disarmed, so a stack that is already loaded at startup is
// not scanned automatically.
package autoscan

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escl"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/metrics"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/output"
)

// Errors returned by Scan.
var (
	ErrBusy     = errors.New("a scan is already running")
	ErrStopping = errors.New("shutting down")
)

// unreachableAfter is the number of failed polls before "unreachable" is
// logged; single timeouts while the printer wakes up are normal.
const unreachableAfter = 3

// Config configures the service.
type Config struct {
	PollInterval time.Duration // POLL_INTERVAL
	MaxBackoff   time.Duration // longest poll interval while unreachable
	Settle       time.Duration // AUTOSCAN_SETTLE
	Enabled      bool          // AUTOSCAN_ENABLED
	Paused       bool          // AUTOSCAN_PAUSED (initial value)

	Intent     string      // SCAN_INTENT
	ColorMode  string      // SCAN_COLOR_MODE, default for automatic and manual scans
	Resolution int         // SCAN_RESOLUTION
	Region     escl.Region // SCAN_WIDTH, SCAN_HEIGHT, SCAN_X_OFFSET

	WakeWorkaround bool // WAKE_WORKAROUND

	BusyRetries   int           // job creation retries while the scanner is busy
	BusyBackoff   time.Duration // first wait between those retries, doubled each time
	DeleteTimeout time.Duration // bound for the best-effort DELETE after a job
	HistorySize   int

	DoubleSidedTimeout time.Duration // DOUBLE_SIDED_TIMEOUT

	Now func() time.Time // clock; time.Now when nil (tests)
}

func (c *Config) defaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
	if c.MaxBackoff < c.PollInterval {
		c.MaxBackoff = max(30*time.Second, c.PollInterval)
	}
	if c.Settle <= 0 {
		c.Settle = 10 * time.Second
	}
	if c.BusyRetries <= 0 {
		c.BusyRetries = 3
	}
	if c.BusyBackoff <= 0 {
		c.BusyBackoff = 2 * time.Second
	}
	if c.DeleteTimeout <= 0 {
		c.DeleteTimeout = 10 * time.Second
	}
	if c.DoubleSidedTimeout <= 0 {
		c.DoubleSidedTimeout = 30 * time.Minute
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.HistorySize <= 0 {
		c.HistorySize = 10
	}
}

// Service is the autoscan state machine plus the scan pipeline.
type Service struct {
	cfg    Config
	dev    *escl.Client
	out    *output.Writer
	failed *output.Writer // nil: rejected scans are discarded
	m      *metrics.Metrics
	log    *slog.Logger
	now    func() time.Time

	// jobCtx bounds running scans. It is not derived from the signal
	// context: on shutdown a running job may finish (see Shutdown).
	jobCtx    context.Context
	jobCancel context.CancelFunc
	wg        sync.WaitGroup
	described bool          // capabilities logged; Run goroutine only
	pollNow   chan struct{} // asks Run for a status poll at once

	mu       sync.Mutex // guards everything below; it also serializes scans
	status   statusView
	armed    bool
	paused   bool
	settling bool
	scanning *Current
	stopping bool
	history  []Record

	ds         int // DoubleSidedOff, ...Fronts, ...Backs
	dsDeadline time.Time
	dsNote     Note       // what happened last in double-sided mode
	dsHeld     *heldPages // the fronts in DoubleSidedBacks, the pages so far in DoubleSidedFlatbed
	failures   int
	lastState  [2]string
}

type statusView struct {
	reachable bool
	state     string
	adf       string
	polled    time.Time
	err       string
	model     string
}

// Current describes the running scan.
type Current struct {
	Source  string
	Trigger string // "auto" or "manual"
	Color   string
	Started time.Time
	// Pass is the role of the scan in a double-sided run: DoubleSidedFronts
	// or DoubleSidedBacks for a feeder scan, DoubleSidedFlatbed for a
	// flatbed scan, DoubleSidedOff for a normal scan. It is fixed when the
	// scan starts.
	Pass int
}

// Record is one entry of the scan history.
type Record struct {
	Time     time.Time
	Source   string
	Trigger  string
	Color    string
	Pages    int
	Result   string
	Files    []string
	Message  string
	Duration time.Duration
	// DoubleSided is set when the scan became part of a document made of
	// several scans: fronts or flatbed pages that are held, or backs
	// collated with their fronts.
	DoubleSided bool
}

// Snapshot is the state shown in the UI.
type Snapshot struct {
	Reachable bool
	State     string
	AdfState  string
	Model     string
	LastPoll  time.Time
	LastError string

	AutoscanEnabled bool
	Armed           bool
	Paused          bool
	Settling        bool
	Settle          time.Duration // delay between detecting paper and scanning
	Scanning        *Current
	ColorMode       string   // preselected: SCAN_COLOR_MODE, or the mode of the scans held for a double-sided document
	History         []Record // newest first

	DoubleSided         int       // DoubleSidedOff, ...Fronts, ...Backs, ...Flatbed
	DoubleSidedDeadline time.Time // when the current step expires
	DoubleSidedHeld     int       // pages held: the fronts, or the flatbed pages so far
	DoubleSidedNote     Note
}

// New creates a service. failed may be nil.
func New(cfg Config, dev *escl.Client, out, failed *output.Writer, m *metrics.Metrics, log *slog.Logger) *Service {
	cfg.defaults()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg: cfg, dev: dev, out: out, failed: failed, m: m, log: log, now: cfg.Now,
		jobCtx: ctx, jobCancel: cancel, pollNow: make(chan struct{}, 1),
		paused: cfg.Paused,
	}
	m.SetPaused(s.paused)
	m.SetArmed(false)
	m.SetUp(false)
	return s
}

// Snapshot returns the current state.
func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{
		Reachable: s.status.reachable, State: s.status.state, AdfState: s.status.adf,
		Model: s.status.model, LastPoll: s.status.polled, LastError: s.status.err,
		AutoscanEnabled: s.cfg.Enabled, Armed: s.armed, Paused: s.paused, Settling: s.settling,
		Settle: s.cfg.Settle, ColorMode: s.cfg.ColorMode,
		History: make([]Record, len(s.history)),

		DoubleSided: s.ds, DoubleSidedDeadline: s.dsDeadline, DoubleSidedNote: s.dsNote,
	}
	if s.dsHeld != nil {
		snap.DoubleSidedHeld = s.dsHeld.pages
		snap.ColorMode = s.dsHeld.color
	}
	if s.scanning != nil {
		cur := *s.scanning
		snap.Scanning = &cur
	}
	for i, r := range s.history {
		snap.History[len(s.history)-1-i] = r
	}
	return snap
}

// SetPaused pauses or resumes automatic scanning. A pending settle timer is
// dropped when it fires while paused.
func (s *Service) SetPaused(paused bool) {
	s.mu.Lock()
	changed := s.paused != paused
	s.paused = paused
	s.mu.Unlock()
	s.m.SetPaused(paused)
	if changed {
		s.log.Info("autoscan paused", "paused", paused)
	}
}

// Run polls the scanner and drives automatic scans until ctx is done.
func (s *Service) Run(ctx context.Context) {
	poll := time.NewTimer(0)
	defer poll.Stop()
	var settle *time.Timer
	var settleC <-chan time.Time
	defer func() {
		if settle != nil {
			settle.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
			s.expireDoubleSided()
			next, startSettle := s.poll(ctx)
			if startSettle {
				settle = time.NewTimer(s.cfg.Settle)
				settleC = settle.C
			}
			poll.Reset(next)
		case <-settleC:
			settleC = nil
			s.settled(ctx)
			poll.Reset(0)
		case <-s.pollNow:
			poll.Reset(0)
		}
	}
}

// poll reads the status once. It returns the delay until the next poll and
// whether a settle timer should start. Only the Run goroutine calls it.
func (s *Service) poll(ctx context.Context) (time.Duration, bool) {
	st, err := s.dev.Status(ctx)
	if err == nil && !s.described {
		s.described = true
		s.describeDevice(ctx)
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.polled = now
	if err != nil {
		s.failures++
		s.status.reachable, s.status.state, s.status.adf, s.status.err = false, "", "", err.Error()
		if s.failures == unreachableAfter {
			s.log.Warn("scanner unreachable", "err", err)
		}
		s.m.SetUp(false)
		s.m.SetAdfState("")
		// Back off while the printer sleeps or is switched off.
		next := s.cfg.PollInterval << min(s.failures, 10)
		return min(next, s.cfg.MaxBackoff), false
	}

	if s.failures >= unreachableAfter {
		s.log.Info("scanner reachable again", "state", st.State, "adf", st.AdfState)
	}
	s.failures = 0
	s.status.reachable, s.status.state, s.status.adf, s.status.err = true, st.State, st.AdfState, ""
	s.m.SetUp(true)
	s.m.SetAdfState(st.AdfState)
	if cur := [2]string{st.State, st.AdfState}; cur != s.lastState {
		s.log.Info("scanner status", "state", st.State, "adf", st.AdfState)
		s.lastState = cur
	}

	// Arm only between scans: the feeder may report empty between sheets
	// of a running job, and a job that then jams leaves paper behind.
	if escl.AdfIsEmpty(st.AdfState) && !s.armed && s.scanning == nil {
		s.setArmed(true, "feeder empty")
	}
	start := s.cfg.Enabled && s.armed && !s.paused && !s.settling && s.scanning == nil && !s.stopping &&
		st.State == escl.StateIdle && st.AdfLoaded()
	if start {
		s.settling = true
		s.log.Info("paper in feeder, scanning after settle time", "settle", s.cfg.Settle)
	}
	return s.cfg.PollInterval, start
}

// describeDevice logs the device model once and warns about a scan region
// the device cannot handle.
func (s *Service) describeDevice(ctx context.Context) {
	caps, err := s.dev.Capabilities(ctx)
	if err != nil {
		s.log.Warn("cannot read scanner capabilities", "err", err)
		return
	}
	s.mu.Lock()
	s.status.model = caps.MakeAndModel
	s.mu.Unlock()
	attrs := []any{"model", caps.MakeAndModel}
	if caps.Adf != nil {
		attrs = append(attrs, "adf_justification", caps.Adf.XImagePosition+"/"+caps.Adf.YImagePosition)
		if in := caps.Adf.Simplex; in != nil {
			s.checkRegion("feeder", in)
		}
	}
	if caps.Platen != nil {
		s.checkRegion("platen", caps.Platen)
	}
	s.log.Info("scanner capabilities", attrs...)
}

func (s *Service) checkRegion(source string, in *escl.InputCaps) {
	r := s.cfg.Region
	if r.XOffset+r.Width > in.MaxWidth || r.YOffset+r.Height > in.MaxHeight {
		s.log.Warn("scan region exceeds the scanner's maximum", "source", source,
			"x_offset", r.XOffset, "width", r.Width, "height", r.Height,
			"max_width", in.MaxWidth, "max_height", in.MaxHeight)
	}
}

func (s *Service) setArmed(armed bool, reason string) {
	if s.armed == armed {
		return
	}
	s.armed = armed
	s.m.SetArmed(armed)
	s.log.Info("autoscan armed", "armed", armed, "reason", reason)
}

// settled runs when the settle timer fires: scan if the feeder is still
// loaded and the scanner idle, otherwise skip (paper removed, or somebody
// started a copy).
func (s *Service) settled(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.settling = false
	drop := s.paused || s.stopping
	s.mu.Unlock()
	if drop {
		return
	}

	st, err := s.dev.Status(ctx)
	if err != nil {
		s.log.Info("autoscan postponed: status check failed", "err", err)
		return
	}
	reason := ""
	switch {
	case !st.AdfLoaded():
		reason = "paper removed"
	case st.State != escl.StateIdle:
		reason = "scanner " + st.State
	}
	if reason == "" {
		err = s.start(escl.Feeder, "", "auto")
		if err == nil {
			return
		}
		reason = err.Error()
	}

	s.mu.Lock()
	s.setArmed(false, "skipped")
	s.mu.Unlock()
	s.log.Info("autoscan skipped", "reason", reason, "state", st.State, "adf", st.AdfState)
	s.record(Record{
		Time: s.now(), Source: "feeder", Trigger: "auto", Color: s.cfg.ColorMode,
		Result: metrics.ResultSkipped, Message: reason,
	}, 0)
}

// Scan starts a manual scan in the background. It returns ErrBusy while
// another scan runs. An empty colorMode means the default.
func (s *Service) Scan(source escl.InputSource, colorMode string) error {
	return s.start(source, colorMode, "manual")
}

func (s *Service) start(source escl.InputSource, colorMode, trigger string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return ErrStopping
	}
	if s.scanning != nil {
		return ErrBusy
	}
	cur := &Current{Source: sourceLabel(source), Trigger: trigger, Started: s.now()}
	if source == escl.Feeder {
		// Whatever happens, this stack is done; wait for an empty feeder.
		s.setArmed(false, "scan started")
	}
	if (source == escl.Platen) == (s.ds == DoubleSidedFlatbed) {
		cur.Pass = s.ds // the scan takes part in the run from its source
	}
	// The default color mode is SCAN_COLOR_MODE; within a double-sided run
	// it is the mode of the scans before, so that the backs match the fronts.
	switch {
	case colorMode != "":
	case cur.Pass != DoubleSidedOff && s.dsHeld != nil:
		colorMode = s.dsHeld.color
	default:
		colorMode = s.cfg.ColorMode
	}
	cur.Color = colorMode
	if cur.Pass != DoubleSidedOff || s.ds == DoubleSidedOff {
		s.dsNote = Note{} // it was about an earlier scan
	}
	s.scanning = cur
	pass := cur.Pass
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		rec := s.scan(s.jobCtx, source, colorMode, pass)
		rec.Trigger = trigger
		s.mu.Lock()
		s.scanning = nil
		s.mu.Unlock()
		s.record(rec, rec.Duration)
		// The last status is from the middle of the scan. Poll again at
		// once: the UI is current, and an empty feeder re-arms autoscan.
		select {
		case s.pollNow <- struct{}{}:
		default:
		}
	}()
	return nil
}

// record counts a finished scan, logs it and puts it into the history.
func (s *Service) record(r Record, d time.Duration) {
	s.m.Scan(r.Source, r.Result, r.Pages, d, r.Time)
	attrs := []any{"source", r.Source, "trigger", r.Trigger, "result", r.Result, "pages", r.Pages,
		"files", r.Files, "duration", r.Duration.Round(time.Millisecond)}
	if r.Message != "" {
		attrs = append(attrs, "msg", r.Message)
	}
	switch r.Result {
	case metrics.ResultOK, metrics.ResultSkipped:
		s.log.Info("scan finished", attrs...)
	default:
		s.log.Warn("scan failed", attrs...)
	}
	s.remember(r)
}

// remember puts an entry into the history shown in the UI.
func (s *Service) remember(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, r)
	if len(s.history) > s.cfg.HistorySize {
		s.history = s.history[len(s.history)-s.cfg.HistorySize:]
	}
}

// Shutdown stops starting new scans and waits up to timeout for a running
// scan. After that the job is cancelled, which deletes it on the device.
// What is held for a double-sided document is saved.
func (s *Service) Shutdown(timeout time.Duration) {
	defer s.saveHeldAtShutdown()
	s.mu.Lock()
	s.stopping = true
	running := s.scanning != nil
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	if running {
		s.log.Info("waiting for running scan", "timeout", timeout)
	}
	select {
	case <-done:
		s.jobCancel()
		return
	case <-time.After(timeout):
	}
	s.log.Warn("scan still running at shutdown, cancelling the job")
	s.jobCancel()
	select {
	case <-done:
	case <-time.After(s.cfg.DeleteTimeout + time.Second):
	}
}

func sourceLabel(src escl.InputSource) string {
	if src == escl.Platen {
		return "platen"
	}
	return "feeder"
}
