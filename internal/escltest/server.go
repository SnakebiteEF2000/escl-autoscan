// Package escltest provides a scriptable fake eSCL scanner for tests. It
// mimics the behaviour observed on an Epson WF-2960 (eSCL 2.9).
package escltest

import (
	"embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

//go:embed testdata
var fixtures embed.FS

// Fixture returns a file from testdata.
func Fixture(name string) []byte {
	b, err := fixtures.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// Step is one answer to a NextDocument request.
type Step struct {
	Code  int           // HTTP status; 200 when zero
	Body  []byte        // response body for 200
	Delay time.Duration // wait before answering (the device blocks while scanning)
	// Before runs right before the answer is sent, e.g. to change the
	// scanner status the way the device does at the end of a job.
	Before func(*Server)
}

// Job is a scan job the fake has accepted.
type Job struct {
	UUID     string
	Settings string // request body
	Deleted  bool
	finished bool // the device has discarded the job
	steps    []Step
}

// Request records one request the fake received.
type Request struct {
	Method, Path, Host string
}

// Server is a fake eSCL scanner served over TLS.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	state    string
	adf      string
	jobInfos []string // rendered scan:JobInfo elements
	down     bool
	creates  []int    // queued answers for POST ScanJobs; 201 when empty
	scripts  [][]Step // NextDocument scripts for the next created jobs
	jobs     []*Job
	requests []Request
	relative bool // answer with a relative Location
	onCreate func(*Server, *Job)
}

// New starts a fake scanner that is Idle with an empty feeder.
func New(t testing.TB) *Server {
	s := &Server{state: "Idle", adf: "ScannerAdfEmpty"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /eSCL/ScannerStatus", s.handleStatus)
	mux.HandleFunc("GET /eSCL/ScannerCapabilities", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write(Fixture("capabilities-wf2960.xml"))
	})
	mux.HandleFunc("POST /eSCL/ScanJobs", s.handleCreate)
	mux.HandleFunc("GET /eSCL/ScanJobs/{uuid}/NextDocument", s.handleNext)
	mux.HandleFunc("DELETE /eSCL/ScanJobs/{uuid}", s.handleDelete)
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Host: r.Host})
		down := s.down
		s.mu.Unlock()
		if down {
			// Drop the connection, like a sleeping or switched-off device.
			panic(http.ErrAbortHandler)
		}
		mux.ServeHTTP(w, r)
	}))
	// Silence "TLS handshake error" logs from fingerprint tests.
	s.Server.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// SetStatus sets pwg:State and scan:AdfState.
func (s *Server) SetStatus(state, adf string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state, s.adf = state, adf
}

// SetDown makes the fake drop every connection.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// QueueCreate queues HTTP statuses for the next POST ScanJobs requests
// (e.g. 503 or 409 for a busy device).
func (s *Server) QueueCreate(codes ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, codes...)
}

// QueueJob queues the NextDocument answers for the next created job. A job
// without a script answers 404 at once. After the script, NextDocument
// answers 404.
func (s *Server) QueueJob(steps ...Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts = append(s.scripts, steps)
}

// RelativeLocation makes the fake answer job creation with a relative
// Location instead of the absolute one the WF-2960 sends.
func (s *Server) RelativeLocation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.relative = true
}

// OnCreate registers a hook that runs when a job has been created.
func (s *Server) OnCreate(f func(*Server, *Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCreate = f
}

// AddJobInfo adds an entry to the job list in ScannerStatus.
func (s *Server) AddJobInfo(uuid, state string, images int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobInfos = append([]string{fmt.Sprintf(
		`<scan:JobInfo><pwg:JobUri>/eSCL/ScanJobs/%[1]s</pwg:JobUri><pwg:JobUuid>%[1]s</pwg:JobUuid>`+
			`<scan:Age>1</scan:Age><pwg:ImagesCompleted>%[3]d</pwg:ImagesCompleted><pwg:JobState>%[2]s</pwg:JobState></scan:JobInfo>`,
		uuid, state, images)}, s.jobInfos...)
}

// Jobs returns the jobs created so far.
func (s *Server) Jobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, len(s.jobs))
	for i, j := range s.jobs {
		out[i] = *j
		out[i].steps = nil
	}
	return out
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><scan:ScannerStatus xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">`+
		`<pwg:Version>2.9</pwg:Version><pwg:State>%s</pwg:State><scan:AdfState>%s</scan:AdfState>`, s.state, s.adf)
	if len(s.jobInfos) > 0 {
		body += "<scan:Jobs>" + strings.Join(s.jobInfos, "") + "</scan:Jobs>"
	}
	body += "</scan:ScannerStatus>"
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, body)
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	if len(s.creates) > 0 {
		code := s.creates[0]
		s.creates = s.creates[1:]
		if code != http.StatusCreated {
			s.mu.Unlock()
			w.WriteHeader(code)
			return
		}
	}
	job := &Job{UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", len(s.jobs)+1), Settings: string(body)}
	if len(s.scripts) > 0 {
		job.steps = s.scripts[0]
		s.scripts = s.scripts[1:]
	}
	s.jobs = append(s.jobs, job)
	loc := "/eSCL/ScanJobs/" + job.UUID
	if !s.relative {
		loc = "https://" + r.Host + loc
	}
	hook := s.onCreate
	s.mu.Unlock()
	if hook != nil {
		hook(s, job)
	}
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) job(uuid string) *Job {
	for _, j := range s.jobs {
		if j.UUID == uuid {
			return j
		}
	}
	return nil
}

func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	job := s.job(r.PathValue("uuid"))
	if job == nil || job.Deleted || len(job.steps) == 0 {
		if job != nil {
			job.finished = true
		}
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	step := job.steps[0]
	job.steps = job.steps[1:]
	if step.Code == http.StatusGone {
		job.finished = true
	}
	s.mu.Unlock()

	if step.Delay > 0 {
		select {
		case <-time.After(step.Delay):
		case <-r.Context().Done():
			return
		}
	}
	code := step.Code
	if code == 0 {
		code = http.StatusOK
	}
	if step.Before != nil {
		step.Before(s)
	}
	if code == http.StatusOK {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(code)
		_, _ = w.Write(step.Body)
	} else {
		w.WriteHeader(code)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.job(r.PathValue("uuid"))
	if job == nil || job.Deleted || job.finished {
		// The WF-2960 has already discarded a finished job.
		http.NotFound(w, r)
		return
	}
	job.Deleted = true
	w.WriteHeader(http.StatusOK)
}
