// Package api serves the daemon over a local unix socket as small JSON
// endpoints. The CLI and the SDK are both clients of it.
package api

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/numinous-technology/gmux/internal/daemon"
	"github.com/numinous-technology/gmux/internal/session"
)

// Server wraps a daemon with an HTTP mux. It serves the local unix socket (the
// CLI and SDK) and, when a token is set, a TCP port for remote clients that run
// commands on this host's GPUs.
type Server struct {
	d        *daemon.Daemon
	mux      *http.ServeMux
	sessions *session.Store
	token    string
}

// New builds the server. sessions and token may be nil/empty for a
// local-only daemon; set them to accept remote clients.
func New(d *daemon.Daemon, sessions *session.Store, token string) *Server {
	s := &Server{d: d, mux: http.NewServeMux(), sessions: sessions, token: token}
	s.mux.HandleFunc("/v1/jobs", s.jobs)
	s.mux.HandleFunc("/v1/jobs/", s.job)
	s.mux.HandleFunc("/v1/cards", s.cards)
	s.mux.HandleFunc("/v1/usage", s.usage)
	s.mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.mux.HandleFunc("/v1/sessions", s.sessionsCreate)
	s.mux.HandleFunc("/v1/sessions/", s.session)
	s.mux.HandleFunc("/v1/blobs/", s.blob)
	return s
}

// handler wraps the mux with bearer auth when a token is configured.
func (s *Server) handler() http.Handler {
	if s.token == "" {
		return s.mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Authorization"), "Bearer "+s.token) {
			fail(w, 401, "a valid bearer token is required")
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// Serve listens on a unix socket at path until the process ends. The local
// socket is trusted and never requires the token.
func (s *Server) Serve(path string) error {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	srv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// ServeTCP listens on addr for remote clients, requiring the bearer token.
func (s *Server) ServeTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 30 * time.Second}
	return srv.Serve(ln)
}

// ServeListener serves an already-open listener (used by tests).
func (s *Server) ServeListener(ln net.Listener) error {
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 30 * time.Second}
	return srv.Serve(ln)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"jobs": s.d.Jobs()})
	case http.MethodPost:
		var req daemon.SubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, 400, "invalid request: "+err.Error())
			return
		}
		job, err := s.d.Submit(req)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
		writeJSON(w, 201, job)
	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/v1/jobs/"):]
	// /v1/jobs/<id>/resize
	if n := len(id); n > 7 && id[n-7:] == "/resize" {
		s.resize(w, r, id[:n-7])
		return
	}
	switch r.Method {
	case http.MethodGet:
		if job, ok := s.d.Job(id); ok {
			writeJSON(w, 200, job)
		} else {
			fail(w, 404, "no such job")
		}
	case http.MethodDelete:
		if err := s.d.Stop(id); err != nil {
			fail(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"stopped": id})
	default:
		fail(w, 405, "method not allowed")
	}
}

func (s *Server) resize(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		fail(w, 405, "method not allowed")
		return
	}
	var req struct {
		Share  float64 `json:"share"`
		MemMiB int     `json:"mem_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	job, err := s.d.Resize(id, req.Share, req.MemMiB)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, job)
}

func (s *Server) cards(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"cards": s.d.Cards()})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	by := r.URL.Query().Get("by")
	until := time.Now()
	since := until.Add(-24 * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		if dur, err := time.ParseDuration(v); err == nil {
			since = until.Add(-dur)
		}
	}
	rows, err := s.d.Usage(since, until, by)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"since": since, "until": until, "rows": rows})
}
