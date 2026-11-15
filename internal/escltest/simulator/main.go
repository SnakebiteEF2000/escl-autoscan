// Command simulator is a scanner for trying escl-autoscan without hardware:
// the fake eSCL device of package escltest, plus a web page that plays the
// person standing at the scanner (put a stack in, turn it over, cause a jam).
//
//	go run ./internal/escltest/simulator
//
// The eSCL side uses a self-signed certificate, so escl-autoscan needs
// ESCL_TLS_INSECURE=true. compose.yaml starts both together.
//
// Every page of a scanned PDF says what it is ("A2 back": stack A, sheet 2,
// back side), so the order of a collated double-sided document can be seen.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SnakebiteEF2000/escl-autoscan/internal/escltest"
	"github.com/SnakebiteEF2000/escl-autoscan/internal/pdftest"
)

func main() {
	esclAddr := flag.String("escl-addr", ":8443", "listen address of the simulated scanner (eSCL over TLS)")
	httpAddr := flag.String("http-addr", ":8081", "listen address of the control page")
	sheetTime := flag.Duration("sheet-time", 1500*time.Millisecond, "time the feeder takes per sheet")
	flag.Parse()

	dev, err := escltest.Listen(*esclAddr)
	if err != nil {
		log.Fatal(err)
	}
	s := &simulator{dev: dev, sheetTime: *sheetTime, sheets: 3}
	dev.Script(s.job)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("POST /do", s.handleDo)
	log.Printf("simulated scanner on %s (eSCL, TLS), control page on %s", *esclAddr, *httpAddr)
	log.Fatal(http.ListenAndServe(*httpAddr, mux))
}

// stack is a stack of paper printed on both sides.
type stack struct {
	Name    string // A, B, C, ...
	Sheets  int
	Flipped bool // the backs face the scanner
}

// Side is the side that faces the scanner.
func (s *stack) Side() string {
	if s.Flipped {
		return "back"
	}
	return "front"
}

// pages returns what the feeder sees, sheet by sheet. A stack that was
// turned over as a whole comes in reverse order.
func (s *stack) pages() []string {
	var out []string
	for i := 1; i <= s.Sheets; i++ {
		n := i
		if s.Flipped {
			n = s.Sheets + 1 - i
		}
		out = append(out, fmt.Sprintf("%s%d %s", s.Name, n, s.Side()))
	}
	return out
}

type simulator struct {
	dev       *escltest.Server
	sheetTime time.Duration

	// mu is taken before the lock inside dev (SetStatus), never after.
	mu         sync.Mutex
	feeder     *stack // in the feeder
	hand       *stack // taken out of the feeder or the output tray
	sheets     int    // size of the next new stack
	stacks     int
	flatbeds   int
	jamNext    bool // the next feeder scan jams
	doubleNext bool // the next feeder scan pulls two sheets at once
	jammed     bool
	asleep     bool
	scanning   bool
	events     []string // newest first
}

func (s *simulator) event(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	s.events = append([]string{time.Now().Format("15:04:05") + "  " + msg}, s.events...)
	if len(s.events) > 12 {
		s.events = s.events[:12]
	}
}

// job answers a new scan job: it returns what NextDocument will say.
func (s *simulator) job(settings string) []escltest.Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	idle := func(*escltest.Server) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.scanning = false
		s.setStatus()
	}

	if !strings.Contains(settings, ">Feeder<") {
		s.flatbeds++
		s.scanning = true
		s.event("flatbed scan %d", s.flatbeds)
		s.dev.SetStatus("Processing", s.adf())
		return []escltest.Step{{
			Delay: 2 * s.sheetTime, Before: idle,
			Body: pdftest.New(pdftest.Options{Labels: []string{fmt.Sprintf("Flatbed scan %d", s.flatbeds)}}),
		}}
	}
	if s.feeder == nil || s.jammed {
		s.event("feeder scan requested, but there is no paper to feed")
		return nil // NextDocument answers 404 at once
	}

	pages := s.feeder.pages()
	s.scanning = true
	s.dev.SetStatus("Processing", "ScannerAdfProcessing")
	if s.jamNext {
		s.jamNext = false
		s.event("scanning stack %s: paper jam after the first sheet", s.feeder.Name)
		pdf := pdftest.New(pdftest.Options{Labels: pages})
		return []escltest.Step{
			{Delay: s.sheetTime, Body: pdf[:bytes.LastIndex(pdf, []byte("xref"))], Before: func(*escltest.Server) {
				s.mu.Lock()
				defer s.mu.Unlock()
				s.scanning, s.jammed = false, true
				s.setStatus()
			}},
			{Code: http.StatusGone},
		}
	}
	if s.doubleNext && len(pages) > 1 {
		s.doubleNext = false
		drop := len(pages) / 2
		s.event("scanning stack %s: two sheets pulled at once, %q is missing", s.feeder.Name, pages[drop])
		pages = append(pages[:drop:drop], pages[drop+1:]...)
	} else {
		s.event("scanning stack %s: %s", s.feeder.Name, strings.Join(pages, ", "))
	}
	return []escltest.Step{{
		Delay: time.Duration(s.feeder.Sheets) * s.sheetTime,
		Body:  pdftest.New(pdftest.Options{Labels: pages}),
		Before: func(d *escltest.Server) {
			s.mu.Lock()
			s.hand, s.feeder = s.feeder, nil // the stack is in the output tray now
			s.mu.Unlock()
			idle(d)
		},
	}}
}

func (s *simulator) adf() string {
	switch {
	case s.jammed:
		return "ScannerAdfJam"
	case s.feeder != nil:
		return "ScannerAdfLoaded"
	}
	return "ScannerAdfEmpty"
}

// setStatus publishes the status of an idle scanner; s.mu must be held.
func (s *simulator) setStatus() {
	state := "Idle"
	if s.jammed {
		state = "Stopped"
	}
	s.dev.SetStatus(state, s.adf())
}

func (s *simulator) handleDo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := r.PostFormValue("action")
	switch {
	case action == "sleep":
		s.asleep = !s.asleep
		s.dev.SetDown(s.asleep)
		s.event("scanner asleep: %v", s.asleep)
	case s.scanning:
		s.event("%q ignored: the scanner is scanning", action)
	case action == "insert" && s.feeder == nil:
		if n, err := strconv.Atoi(r.PostFormValue("sheets")); err == nil && n >= 1 && n <= 50 {
			s.sheets = n
		}
		s.stacks++
		s.feeder, s.hand = &stack{Name: string(rune('A' + (s.stacks-1)%26)), Sheets: s.sheets}, nil
		s.event("new stack %s with %d sheets put into the feeder, fronts first", s.feeder.Name, s.sheets)
	case (action == "putback" || action == "turnover") && s.feeder == nil && s.hand != nil:
		s.feeder, s.hand = s.hand, nil
		if action == "turnover" {
			s.feeder.Flipped = !s.feeder.Flipped
		}
		s.event("stack %s put into the feeder, %ss first", s.feeder.Name, s.feeder.Side())
	case action == "remove" && s.feeder != nil:
		s.hand, s.feeder, s.jammed = s.feeder, nil, false
		s.event("stack %s taken out of the feeder", s.hand.Name)
	case action == "jam":
		s.jamNext = !s.jamNext
	case action == "double":
		s.doubleNext = !s.doubleNext
	}
	s.setStatus()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *simulator) handlePage(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := "idle"
	switch {
	case s.asleep:
		state = "asleep (not reachable)"
	case s.scanning:
		state = "scanning"
	case s.jammed:
		state = "stopped: paper jam, take the stack out of the feeder"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, map[string]any{
		"State": state, "Scanning": s.scanning, "Feeder": s.feeder, "Hand": s.hand, "Sheets": s.sheets,
		"JamNext": s.jamNext, "DoubleNext": s.doubleNext, "Asleep": s.asleep, "Events": s.events,
	})
}

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="2">
<title>Scanner simulator</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 1rem/1.5 system-ui, sans-serif; max-width: 40rem; margin: 0 auto; padding: 1rem; }
  h1 { font-size: 1.4rem; margin: 0; } h2 { font-size: 1rem; margin: 1.5rem 0 .5rem; }
  dl { display: grid; grid-template-columns: max-content 1fr; gap: .25rem 1rem; margin: 1rem 0; }
  dt { opacity: .7; } dd { margin: 0; font-weight: 600; }
  form { display: flex; flex-wrap: wrap; gap: .5rem; align-items: center; margin: .5rem 0; }
  button { font: inherit; min-height: 2.5rem; padding: .4rem .9rem; }
  input[type=number] { font: inherit; width: 4.5rem; min-height: 2.5rem; padding: 0 .4rem; }
  ol { list-style: none; padding: 0; margin: 0; font: .875rem/1.6 ui-monospace, Menlo, monospace; }
  .on { font-weight: 700; }
</style>
</head>
<body>
<h1>Scanner simulator</h1>
<p>This page plays the person at the scanner. escl-autoscan talks to the simulated device as it would to a real one.</p>
<dl>
  <dt>Scanner</dt><dd>{{.State}}</dd>
  <dt>Feeder</dt><dd>{{with .Feeder}}stack {{.Name}}, {{.Sheets}} sheets, {{.Side}}s first{{else}}empty{{end}}</dd>
  <dt>Beside the scanner</dt><dd>{{with .Hand}}stack {{.Name}}, {{.Sheets}} sheets, as it came out ({{.Side}}s scanned){{else}}nothing{{end}}</dd>
</dl>

<h2>Paper</h2>
<form method="post" action="/do">
  <input type="number" name="sheets" min="1" max="50" value="{{.Sheets}}" aria-label="Sheets">
  <button name="action" value="insert"{{if or .Feeder .Scanning}} disabled{{end}}>Put a new stack into the feeder</button>
</form>
<form method="post" action="/do">
  <button name="action" value="turnover"{{if or .Feeder (not .Hand) .Scanning}} disabled{{end}}>Turn the stack over and put it back</button>
  <button name="action" value="putback"{{if or .Feeder (not .Hand) .Scanning}} disabled{{end}}>Put the stack back as it is</button>
  <button name="action" value="remove"{{if or (not .Feeder) .Scanning}} disabled{{end}}>Take the stack out of the feeder</button>
</form>

<h2>Trouble</h2>
<form method="post" action="/do">
  <button name="action" value="jam"{{if .JamNext}} class="on"{{end}}>Paper jam on the next feeder scan{{if .JamNext}}: armed{{end}}</button>
  <button name="action" value="double"{{if .DoubleNext}} class="on"{{end}}>Pull two sheets at once on the next feeder scan{{if .DoubleNext}}: armed{{end}}</button>
  <button name="action" value="sleep"{{if .Asleep}} class="on"{{end}}>{{if .Asleep}}Wake the scanner{{else}}Put the scanner to sleep{{end}}</button>
</form>

<h2>What happened</h2>
<ol>{{range .Events}}<li>{{.}}</li>{{else}}<li>Nothing yet.</li>{{end}}</ol>
</body>
</html>
`))
