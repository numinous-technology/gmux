package remote_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/numinous-technology/gmux/internal/api"
	"github.com/numinous-technology/gmux/internal/daemon"
	"github.com/numinous-technology/gmux/internal/remote"
	"github.com/numinous-technology/gmux/internal/session"
)

// startHost brings up a real gmux daemon with a fake GPU and a TCP remote
// listener, and returns "token@host:port". No GPU is needed.
func startHost(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	self, _ := os.Executable()
	d, err := daemon.New(context.Background(), daemon.Config{StateDir: dir, SelfPath: self, Fake: "1xA100:80G"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close(context.Background()) })
	store, err := session.Open(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(d, store, "secret-token")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeListener(ln)
	return "secret-token@" + ln.Addr().String()
}

func TestRemoteRunRoundTrip(t *testing.T) {
	target := startHost(t)
	c, err := remote.Dial(target)
	if err != nil {
		t.Fatal(err)
	}

	// a workspace to ship
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "input.txt"), []byte("payload"), 0o644)

	sid, err := c.EnsureSession("")
	if err != nil {
		t.Fatal(err)
	}
	nf, up, err := c.Sync(work, sid)
	if err != nil || nf != 1 || up != 1 {
		t.Fatalf("sync: files=%d up=%d err=%v", nf, up, err)
	}
	// a second sync uploads nothing new (content addressed)
	if _, up2, _ := c.Sync(work, sid); up2 != 0 {
		t.Fatalf("re-sync should upload 0 blobs, got %d", up2)
	}

	// run a command on the host that reads the synced file and writes a result
	var out, errb bytes.Buffer
	exit, err := c.Exec(sid, remote.ExecRequest{
		Command: []string{"sh", "-c", "cat input.txt; echo; echo done > result.txt"},
		Share:   0.25,
	}, &out, &errb)
	if err != nil {
		t.Fatalf("exec: %v (stderr=%s)", err, errb.String())
	}
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%s", exit, errb.String())
	}
	if !strings.Contains(out.String(), "payload") {
		t.Fatalf("streamed output missing the input: %q", out.String())
	}

	// pull the result back
	dest := t.TempDir()
	n, err := c.Pull(sid, []string{"result.txt"}, dest)
	if err != nil || n != 1 {
		t.Fatalf("pull: n=%d err=%v", n, err)
	}
	if body, _ := os.ReadFile(filepath.Join(dest, "result.txt")); strings.TrimSpace(string(body)) != "done" {
		t.Fatalf("pulled result wrong: %q", body)
	}

	if err := c.Delete(sid); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteExecReportsExitCode(t *testing.T) {
	c, _ := remote.Dial(startHost(t))
	sid, _ := c.EnsureSession("")
	c.Sync(t.TempDir(), sid)
	var out, errb bytes.Buffer
	exit, err := c.Exec(sid, remote.ExecRequest{Command: []string{"sh", "-c", "exit 7"}, Share: 0.25}, &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 7 {
		t.Fatalf("want exit 7, got %d", exit)
	}
}

func TestRemoteRequiresToken(t *testing.T) {
	target := startHost(t)
	// strip the token
	at := strings.Index(target, "@")
	c, _ := remote.Dial(target[at+1:])
	if _, err := c.EnsureSession(""); err == nil {
		t.Fatal("a request without the bearer token must be refused")
	}
}

var _ = time.Second
