package webapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// State, events and health (reference 5.1 web-management, PLAN 18.2).

// StateSource gives the state documents (switchd: the structures behind
// the show commands).
type StateSource interface {
	StateNames() []string
	// State returns a document; ErrUnknown for a name it does not have.
	State(name string, q url.Values) (any, error)
}

// ErrUnknown: no such state document.
var ErrUnknown = errors.New("unknown")

// Health is what /readyz and /metrics read.
type Health interface {
	// Ready reports whether the switch applied its configuration and has a
	// master ("": ready, else the reason).
	Ready() string
	// Metrics writes the Prometheus text format.
	Metrics(w io.Writer) error
}

func (s *Server) sources() (StateSource, Health) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.health
}

// SetState gives the server its state and health (nil: 503).
func (s *Server) SetState(st StateSource, h Health) {
	s.mu.Lock()
	s.state, s.health = st, h
	s.mu.Unlock()
}

func (s *Server) stateList(w http.ResponseWriter, r *http.Request, _ User) {
	st, _ := s.sources()
	if st == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	reply(w, http.StatusOK, st.StateNames())
}

func (s *Server) stateGet(w http.ResponseWriter, r *http.Request, _ User) {
	st, _ := s.sources()
	if st == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	name := r.PathValue("name")
	v, err := st.State(name, r.URL.Query())
	switch {
	case errors.Is(err, ErrUnknown):
		fail(w, http.StatusNotFound, fmt.Errorf("no state %q (GET /api/v1/state lists them)", name))
	case err != nil && v == nil:
		fail(w, http.StatusServiceUnavailable, err)
	default:
		// A partial answer (a member did not answer) still has the rest.
		if err != nil {
			w.Header().Set("X-Partial", err.Error())
		}
		reply(w, http.StatusOK, v)
	}
}

// ---- events ----

// eventBuffer is how many events a slow client may fall behind.
const eventBuffer = 256

type hub struct {
	mu   sync.Mutex
	subs map[chan string]*bool // -> lost
}

// Publish sends a notice to every event stream (switchd calls it for every
// notice the CLI sessions get).
func (s *Server) Publish(text string) {
	s.events.mu.Lock()
	defer s.events.mu.Unlock()
	for ch, lost := range s.events.subs {
		select {
		case ch <- text:
		default:
			*lost = true
		}
	}
}

func (s *Server) eventStream(w http.ResponseWriter, r *http.Request, u User) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
		return
	}
	ch, lost := make(chan string, eventBuffer), new(bool)
	s.events.mu.Lock()
	if s.events.subs == nil {
		s.events.subs = map[chan string]*bool{}
	}
	s.events.subs[ch] = lost
	s.events.mu.Unlock()
	defer func() {
		s.events.mu.Lock()
		delete(s.events.subs, ch)
		s.events.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, ": cerOS events\n\n")
	fl.Flush()
	beat := time.NewTicker(30 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			io.WriteString(w, ": keepalive\n\n")
		case text := <-ch:
			s.events.mu.Lock()
			wasLost := *lost
			*lost = false
			s.events.mu.Unlock()
			if wasLost {
				io.WriteString(w, "event: lost\ndata: events were dropped (the client read too slowly)\n\n")
			}
			io.WriteString(w, "event: notice\n")
			for l := range strings.Lines(strings.TrimRight(text, "\n")) {
				io.WriteString(w, "data: "+strings.TrimRight(l, "\n")+"\n")
			}
			io.WriteString(w, "\n")
		}
		fl.Flush()
	}
}

// ---- health ----

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	_, h := s.sources()
	why := "starting"
	if h != nil {
		why = h.Ready()
	}
	if why != "" {
		reply(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "reason": why})
		return
	}
	reply(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request, _ User) {
	_, h := s.sources()
	if h == nil {
		fail(w, http.StatusServiceUnavailable, errNoCLI)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.WriteHeader(http.StatusOK)
	if err := h.Metrics(w); err != nil {
		fmt.Fprintf(w, "# error: %v\n", err)
	}
}
