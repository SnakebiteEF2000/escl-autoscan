// Package metrics defines the Prometheus metrics of escl-autoscan.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "escl_autoscan"

// Scan results, the "result" label of scans_total.
const (
	ResultOK            = "ok"
	ResultAborted       = "aborted"        // the device aborted the job (410, jam)
	ResultIncompletePDF = "incomplete_pdf" // the job ended normally but the PDF failed validation
	ResultBusy          = "busy"           // the device refused the job (503/409)
	ResultError         = "error"          // anything else, e.g. network errors or no documents
	ResultWakeGlitch    = "wake_glitch"    // Epson wake bug: 404 without documents while paper is loaded
	ResultSkipped       = "skipped"        // autoscan settle ended without a scan
)

// Double-sided outcomes, the "result" label of double_sided_total.
const (
	DoubleSidedDelivered = "delivered"
	DoubleSidedTimeout   = "timeout"
	DoubleSidedCancelled = "cancelled" // by the user
	DoubleSidedFailed    = "failed"    // a pass failed
)

// Results lists all result label values.
var Results = []string{ResultOK, ResultAborted, ResultIncompletePDF, ResultBusy, ResultError, ResultWakeGlitch, ResultSkipped}

// Sources lists the "source" label values.
var Sources = []string{"feeder", "platen"}

// Metrics holds the service metrics.
type Metrics struct {
	up          prometheus.Gauge
	adfState    *prometheus.GaugeVec
	armed       prometheus.Gauge
	paused      prometheus.Gauge
	scans       *prometheus.CounterVec
	pages       prometheus.Counter
	lastSuccess prometheus.Gauge
	duration    prometheus.Histogram
	dsState     prometheus.Gauge
	dsTotal     *prometheus.CounterVec

	mu     sync.Mutex
	states map[string]bool // adf_state label values created so far
}

// New creates the metrics and registers them with reg. knownAdfStates are
// exported as 0 from the start so dashboards see every series.
func New(reg prometheus.Registerer, knownAdfStates []string) *Metrics {
	m := &Metrics{
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "scanner_up",
			Help: "1 if the last scanner status request succeeded.",
		}),
		adfState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "adf_state",
			Help: "Current ADF state of the scanner (1 for the current state, 0 otherwise).",
		}, []string{"state"}),
		armed: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "armed",
			Help: "1 if autoscan will scan the next stack put into the feeder.",
		}),
		paused: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "paused",
			Help: "1 if autoscan is paused.",
		}),
		scans: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "scans_total",
			Help: "Scan attempts by input source and result.",
		}, []string{"source", "result"}),
		pages: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "pages_total",
			Help: "Pages delivered to the output directory.",
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "last_success_timestamp_seconds",
			Help: "Unix time of the last delivered scan.",
		}),
		duration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "scan_duration_seconds",
			Help:    "Duration of scan jobs, from job creation to the last document.",
			Buckets: []float64{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600},
		}),
		dsState: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "double_sided_state",
			Help: "Double-sided mode: 0 off, 1 waiting for the fronts, 2 waiting for the backs.",
		}),
		dsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "double_sided_total",
			Help: "Finished double-sided runs by result.",
		}, []string{"result"}),
		states: map[string]bool{},
	}
	reg.MustRegister(m.up, m.adfState, m.armed, m.paused, m.scans, m.pages, m.lastSuccess, m.duration, m.dsState, m.dsTotal)
	for _, r := range []string{DoubleSidedDelivered, DoubleSidedTimeout, DoubleSidedCancelled, DoubleSidedFailed} {
		m.dsTotal.WithLabelValues(r)
	}
	for _, s := range knownAdfStates {
		m.adfState.WithLabelValues(s).Set(0)
		m.states[s] = true
	}
	for _, src := range Sources {
		for _, r := range Results {
			m.scans.WithLabelValues(src, r)
		}
	}
	return m
}

// SetUp records whether the scanner answered.
func (m *Metrics) SetUp(up bool) { m.up.Set(b2f(up)) }

// SetAdfState sets the series of state to 1 and all others to 0. An empty
// state (scanner unreachable) sets all to 0.
func (m *Metrics) SetAdfState(state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state != "" {
		m.states[state] = true
	}
	for s := range m.states {
		m.adfState.WithLabelValues(s).Set(b2f(s == state))
	}
}

// SetArmed records the autoscan arm state.
func (m *Metrics) SetArmed(armed bool) { m.armed.Set(b2f(armed)) }

// SetPaused records the pause flag.
func (m *Metrics) SetPaused(paused bool) { m.paused.Set(b2f(paused)) }

// Scan records one scan attempt. Pages and the success timestamp are only
// recorded for delivered scans; duration only for scans that ran a job.
func (m *Metrics) Scan(source, result string, pages int, d time.Duration, at time.Time) {
	m.scans.WithLabelValues(source, result).Inc()
	if d > 0 {
		m.duration.Observe(d.Seconds())
	}
	if result == ResultOK {
		m.pages.Add(float64(pages))
		m.lastSuccess.Set(float64(at.Unix()))
	}
}

// SetDoubleSidedState records the double-sided state (0, 1 or 2).
func (m *Metrics) SetDoubleSidedState(state int) { m.dsState.Set(float64(state)) }

// DoubleSided counts a finished double-sided run.
func (m *Metrics) DoubleSided(result string) { m.dsTotal.WithLabelValues(result).Inc() }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
