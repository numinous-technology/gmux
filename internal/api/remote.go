package api

import (
	"bufio"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/numinous-technology/gmux/internal/daemon"
	"github.com/numinous-technology/gmux/internal/session"
)

// remote endpoints: a GPU-less client syncs a workspace and runs a command on
// this host's GPUs under a real gmux share. The command is the network
// boundary, not the CUDA call.

func (s *Server) requireSessions(w http.ResponseWriter) bool {
	if s.sessions == nil {
		fail(w, 501, "this daemon does not accept remote sessions (start it with --addr and --token)")
		return false
	}
	return true
}

func (s *Server) sessionsCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSessions(w) || r.Method != http.MethodPost {
		if r.Method != http.MethodPost {
			fail(w, 405, "method not allowed")
		}
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	id := req.ID
	if id == "" {
		id = "s" + randHex()
	}
	if _, err := s.sessions.Workspace(id, true); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

// /v1/sessions/{id}/{verb}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if !s.requireSessions(w) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	verb := ""
	if len(parts) == 2 {
		verb = parts[1]
	}
	switch {
	case verb == "" && r.Method == http.MethodDelete:
		if err := s.sessions.Delete(id); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"deleted": id})
	case verb == "manifest" && r.Method == http.MethodPost:
		s.manifest(w, r)
	case verb == "apply" && r.Method == http.MethodPost:
		s.apply(w, r, id)
	case verb == "exec" && r.Method == http.MethodPost:
		s.exec(w, r, id)
	case verb == "fetch" && r.Method == http.MethodPost:
		s.fetchWs(w, r, id)
	default:
		fail(w, 404, "no such session route")
	}
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files []session.File `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"missing": s.sessions.Missing(req.Files)})
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	if !s.requireSessions(w) || r.Method != http.MethodPut {
		if r.Method != http.MethodPut {
			fail(w, 405, "method not allowed")
		}
		return
	}
	sha := strings.TrimPrefix(r.URL.Path, "/v1/blobs/")
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<30))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if err := s.sessions.PutBlob(sha, data); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"sha": sha, "size": len(data)})
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Files []session.File `json:"files"`
		Prune bool           `json:"prune"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if err := s.sessions.Apply(id, req.Files, req.Prune); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "files": len(req.Files)})
}

func (s *Server) fetchWs(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Globs []string `json:"globs"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	data, err := s.sessions.Fetch(id, req.Globs)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(200)
	w.Write(data)
}

// exec runs the client's command in the session workspace under a gmux share
// and streams NDJSON: {"type":"o"/"e","d":line} lines, then a final
// {"type":"exit","code":n}.
func (s *Server) exec(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Command []string `json:"command"`
		Share   float64  `json:"share"`
		MemMiB  int      `json:"mem_mib"`
		Name    string   `json:"name"`
		Allow   []string `json:"allow"`
		DenyNet bool     `json:"deny_net"`
		Wait    bool     `json:"wait"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ws, err := s.sessions.Workspace(id, false)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)

	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	events := make(chan map[string]any, 256)
	var wg sync.WaitGroup
	scan := func(rc io.Reader, kind string) {
		defer wg.Done()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			events <- map[string]any{"type": kind, "d": sc.Text()}
		}
	}
	wg.Add(2)
	go scan(outR, "o")
	go scan(errR, "e")

	job, done, err := s.d.SubmitStreaming(daemon.SubmitRequest{
		Command: req.Command, Share: req.Share, MemMiB: req.MemMiB, Name: req.Name,
		Allow: req.Allow, DenyNet: req.DenyNet, Wait: req.Wait, Dir: ws,
	}, outW, errW)
	if err != nil {
		emit(w, flusher, map[string]any{"type": "error", "msg": err.Error()})
		outW.Close()
		errW.Close()
		return
	}
	emit(w, flusher, map[string]any{"type": "status", "msg": "running", "job": job.ID, "state": job.State})

	// One goroutine owns the done channel: when the job ends it closes the
	// pipes so the scanners drain, waits for them, reports the exit code, then
	// closes the event stream.
	final := make(chan int, 1)
	go func() {
		code := <-done
		outW.Close()
		errW.Close()
		wg.Wait()
		final <- code
		close(events)
	}()
	for ev := range events {
		emit(w, flusher, ev)
	}
	emit(w, flusher, map[string]any{"type": "exit", "code": <-final})
}

func emit(w io.Writer, f http.Flusher, obj map[string]any) {
	b, _ := json.Marshal(obj)
	w.Write(append(b, '\n'))
	f.Flush()
}

func randHex() string { return fmt.Sprintf("%08x", rndUint32()) }

func timeNow() int64 { return time.Now().UnixNano() }

func rndUint32() uint32 {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		return uint32(timeNow())
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
