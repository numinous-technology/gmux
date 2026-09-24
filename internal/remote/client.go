// Package remote is the GPU-less client: it syncs the working directory to a
// gmux GPU host, runs a command there on a share of a GPU, streams the output
// back, and pulls result files. The command is the network boundary, so it is
// latency tolerant and needs no CUDA interception.
package remote

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/numinous-technology/gmux/internal/daemon"
	"github.com/numinous-technology/gmux/internal/session"
)

// Client talks to a remote gmux host.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// Dial parses a target, TOKEN@HOST:PORT#FINGERPRINT, into a client. With a
// fingerprint the client speaks TLS and accepts only the host certificate with
// that SHA-256 fingerprint (what `gmux serve` prints). Without one it speaks
// plain HTTP, which is only for a host on the same trusted machine or network.
func Dial(target string) (*Client, error) {
	fp := ""
	if i := strings.LastIndex(target, "#"); i >= 0 {
		target, fp = target[:i], strings.ToLower(target[i+1:])
	}
	token := ""
	if at := strings.LastIndex(target, "@"); at >= 0 {
		token, target = target[:at], target[at+1:]
	}
	target = strings.TrimPrefix(strings.TrimPrefix(target, "https://"), "http://")
	if target == "" {
		return nil, fmt.Errorf("no host in remote target")
	}
	c := &Client{token: token, http: &http.Client{}}
	if fp == "" {
		c.base = "http://" + strings.TrimRight(target, "/")
		return c, nil
	}
	c.base = "https://" + strings.TrimRight(target, "/")
	c.http.Transport = &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // identity is checked by fingerprint below
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("gmux host sent no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if got := hex.EncodeToString(sum[:]); got != fp {
				return fmt.Errorf("gmux host certificate fingerprint is %s, expected %s", got, fp)
			}
			return nil
		},
	}}
	return c, nil
}

// Base is the host's address, for messages.
func (c *Client) Base() string { return c.base }

// SetTimeout bounds each request; control calls use it so an unreachable host
// fails fast. Exec streams have no timeout.
func (c *Client) SetTimeout(d time.Duration) { c.http.Timeout = d }

func (c *Client) req(method, path string, body io.Reader, ctype string) (*http.Request, error) {
	r, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	return r, nil
}

func (c *Client) doJSON(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := c.req(method, path, body, "application/json")
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach gmux host %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", firstNonEmpty(e.Error, resp.Status))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// EnsureSession creates (or reuses) a session and returns its id.
func (c *Client) EnsureSession(id string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.doJSON("POST", "/v1/sessions", map[string]string{"id": id}, &out)
	return out.ID, err
}

// Sync uploads the working directory to the session, sending only blobs the
// host does not already have.
func (c *Client) Sync(dir, id string) (files, uploaded int, err error) {
	manifest, err := buildManifest(dir)
	if err != nil {
		return 0, 0, err
	}
	var miss struct {
		Missing []string `json:"missing"`
	}
	if err := c.doJSON("POST", "/v1/sessions/"+id+"/manifest", map[string]any{"files": manifest}, &miss); err != nil {
		return 0, 0, err
	}
	bySHA := map[string]string{}
	for _, f := range manifest {
		bySHA[f.SHA] = filepath.Join(dir, filepath.FromSlash(f.Path))
	}
	for _, sha := range miss.Missing {
		data, err := os.ReadFile(bySHA[sha])
		if err != nil {
			return 0, 0, err
		}
		req, _ := c.req("PUT", "/v1/blobs/"+sha, bytes.NewReader(data), "application/octet-stream")
		resp, err := c.http.Do(req)
		if err != nil {
			return 0, 0, err
		}
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			return 0, 0, fmt.Errorf("uploading blob %s: %s", sha[:12], resp.Status)
		}
	}
	if err := c.doJSON("POST", "/v1/sessions/"+id+"/apply", map[string]any{"files": manifest, "prune": true}, nil); err != nil {
		return 0, 0, err
	}
	return len(manifest), len(miss.Missing), nil
}

// ExecRequest is a remote run.
type ExecRequest struct {
	Command []string `json:"command"`
	Share   float64  `json:"share"`
	MemMiB  int      `json:"mem_mib,omitempty"`
	Name    string   `json:"name,omitempty"`
	Allow   []string `json:"allow,omitempty"`
	DenyNet bool     `json:"deny_net,omitempty"`
	Wait    bool     `json:"wait,omitempty"`
}

// Exec runs the command on the host and streams output to stdout/stderr,
// returning the exit code. Cancelling ctx closes the stream, and the host stops
// the job.
func (c *Client) Exec(ctx context.Context, id string, r ExecRequest, stdout, stderr io.Writer) (int, error) {
	b, _ := json.Marshal(r)
	req, _ := c.req("POST", "/v1/sessions/"+id+"/exec", bytes.NewReader(b), "application/json")
	req = req.WithContext(ctx)
	resp, err := c.http.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return -1, fmt.Errorf("exec refused: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	exit := -1
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var ev struct {
			Type string `json:"type"`
			D    string `json:"d"`
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "o":
			fmt.Fprintln(stdout, ev.D)
		case "e":
			fmt.Fprintln(stderr, ev.D)
		case "exit":
			exit = ev.Code
		case "error":
			return -1, fmt.Errorf("%s", ev.Msg)
		}
	}
	return exit, sc.Err()
}

// Pull downloads workspace files matching globs into dir.
func (c *Client) Pull(id string, globs []string, dir string) (int, error) {
	b, _ := json.Marshal(map[string]any{"globs": globs})
	req, _ := c.req("POST", "/v1/sessions/"+id+"/fetch", bytes.NewReader(b), "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("fetch failed: %s", resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(os.PathSeparator)) {
			continue
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		f, err := os.Create(dst)
		if err != nil {
			return n, err
		}
		io.Copy(f, tr)
		f.Close()
		n++
	}
	return n, nil
}

// Delete removes the session on the host.
func (c *Client) Delete(id string) error {
	return c.doJSON("DELETE", "/v1/sessions/"+id, nil, nil)
}

func buildManifest(root string) ([]session.File, error) {
	var out []session.File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".gmux" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(root, p)
		out = append(out, session.File{Path: filepath.ToSlash(rel), SHA: hex.EncodeToString(sum[:]),
			Size: info.Size(), X: info.Mode()&0o100 != 0})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Control operations, the same endpoints the local CLI uses.

// Cards reports the host's GPUs and what is free on each.
func (c *Client) Cards() ([]daemon.CardView, error) {
	var out struct{ Cards []daemon.CardView }
	return out.Cards, c.doJSON("GET", "/v1/cards", nil, &out)
}

// Jobs lists the host's jobs.
func (c *Client) Jobs() ([]daemon.Job, error) {
	var out struct{ Jobs []daemon.Job }
	return out.Jobs, c.doJSON("GET", "/v1/jobs", nil, &out)
}

// Stop ends a job on the host.
func (c *Client) Stop(id string) error { return c.doJSON("DELETE", "/v1/jobs/"+id, nil, nil) }

// Resize changes a job's share on the host.
func (c *Client) Resize(id string, share float64, memMiB int) (*daemon.Job, error) {
	var job daemon.Job
	return &job, c.doJSON("POST", "/v1/jobs/"+id+"/resize", map[string]any{"share": share, "mem_mib": memMiB}, &job)
}

// Usage reports the host's accounting.
func (c *Client) Usage(since, by string) (map[string]any, error) {
	var out map[string]any
	return out, c.doJSON("GET", "/v1/usage?since="+since+"&by="+by, nil, &out)
}
