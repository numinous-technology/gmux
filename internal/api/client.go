package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/numinous-technology/gmux/internal/daemon"
)

// Client talks to a daemon over its unix socket.
type Client struct {
	http *http.Client
}

// Dial returns a client for the socket at path.
func Dial(path string) *Client {
	return &Client{http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}}
}

func (c *Client) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://gmux"+path, r)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gmux daemon not reachable (is `gmux serve` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Submit starts a job.
func (c *Client) Submit(r daemon.SubmitRequest) (*daemon.Job, error) {
	var job daemon.Job
	return &job, c.do("POST", "/v1/jobs", r, &job)
}

// Jobs lists jobs.
func (c *Client) Jobs() ([]daemon.Job, error) {
	var out struct{ Jobs []daemon.Job }
	return out.Jobs, c.do("GET", "/v1/jobs", nil, &out)
}

// Stop ends a job.
func (c *Client) Stop(id string) error { return c.do("DELETE", "/v1/jobs/"+id, nil, nil) }

// Resize changes a job's share.
func (c *Client) Resize(id string, share float64, memMiB int) (*daemon.Job, error) {
	var job daemon.Job
	return &job, c.do("POST", "/v1/jobs/"+id+"/resize", map[string]any{"share": share, "mem_mib": memMiB}, &job)
}

// Cards reports the GPUs.
func (c *Client) Cards() ([]daemon.CardView, error) {
	var out struct{ Cards []daemon.CardView }
	return out.Cards, c.do("GET", "/v1/cards", nil, &out)
}

// Usage reports accounting.
func (c *Client) Usage(since, by string) (map[string]any, error) {
	var out map[string]any
	return out, c.do("GET", "/v1/usage?since="+since+"&by="+by, nil, &out)
}
