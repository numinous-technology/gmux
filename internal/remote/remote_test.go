package remote_test

import (
	"bytes"
	"context"
	"crypto/tls"
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
	cert, fp, err := api.LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeListener(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}}))
	return "secret-token@" + ln.Addr().String() + "#" + fp
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
	exit, err := c.Exec(context.Background(), sid, remote.ExecRequest{
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
	exit, err := c.Exec(context.Background(), sid, remote.ExecRequest{Command: []string{"sh", "-c", "exit 7"}, Share: 0.25}, &out, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 7 {
		t.Fatalf("want exit 7, got %d", exit)
	}
}

func TestRemoteRequiresToken(t *testing.T) {
	target := startHost(t)
	at := strings.Index(target, "@")
	c, _ := remote.Dial(target[at+1:]) // no token
	if _, err := c.EnsureSession(""); err == nil {
		t.Fatal("a request without the bearer token must be refused")
	}
}

func TestRemoteRefusesAnotherCertificate(t *testing.T) {
	target := startHost(t)
	wrong := target[:strings.LastIndex(target, "#")+1] + strings.Repeat("0", 64)
	c, _ := remote.Dial(wrong)
	if _, err := c.Cards(); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("a host whose certificate does not match the pinned fingerprint must be refused, got %v", err)
	}
	plain, _ := remote.Dial(target[:strings.LastIndex(target, "#")]) // plain http to a TLS host
	if _, err := plain.Cards(); err == nil {
		t.Fatal("plain HTTP must not reach a TLS host")
	}
}

func TestRemoteControlOperations(t *testing.T) {
	c, _ := remote.Dial(startHost(t))
	cv, err := c.Cards()
	if err != nil || len(cv) != 1 || cv[0].Name != "A100" {
		t.Fatalf("cards: %+v %v", cv, err)
	}
	sid, _ := c.EnsureSession("")
	c.Sync(t.TempDir(), sid)
	go c.Exec(context.Background(), sid, remote.ExecRequest{Command: []string{"sleep", "30"}, Share: 0.5, Name: "long"}, &bytes.Buffer{}, &bytes.Buffer{})
	var id string
	for i := 0; i < 50 && id == ""; i++ {
		time.Sleep(50 * time.Millisecond)
		jobs, _ := c.Jobs()
		for _, j := range jobs {
			if j.Name == "long" && j.State == "running" {
				id = j.ID
			}
		}
	}
	if id == "" {
		t.Fatal("the remote job never showed as running")
	}
	if cv, _ := c.Cards(); cv[0].FreeSeats != 4 {
		t.Fatalf("a half share should leave 4 of 8 seats, got %d", cv[0].FreeSeats)
	}
	if err := c.Stop(id); err != nil {
		t.Fatal(err)
	}
	waitState(t, c, id, "exited")
}

func TestCancelledRemoteRunStopsTheJob(t *testing.T) {
	c, _ := remote.Dial(startHost(t))
	sid, _ := c.EnsureSession("")
	c.Sync(t.TempDir(), sid)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Exec(ctx, sid, remote.ExecRequest{Command: []string{"sleep", "60"}, Share: 0.25, Name: "ctrlc"}, &bytes.Buffer{}, &bytes.Buffer{})
		close(done)
	}()
	var id string
	for i := 0; i < 50 && id == ""; i++ {
		time.Sleep(50 * time.Millisecond)
		jobs, _ := c.Jobs()
		for _, j := range jobs {
			if j.Name == "ctrlc" && j.State == "running" {
				id = j.ID
			}
		}
	}
	cancel() // what Ctrl-C does on the client
	<-done
	waitState(t, c, id, "exited")
}

func waitState(t *testing.T, c *remote.Client, id, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		jobs, _ := c.Jobs()
		for _, j := range jobs {
			if j.ID == id && j.State == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %s", id, want)
}

var _ = time.Second

func TestUnchangedFilesAreNotReadAgainAndLargeFilesStream(t *testing.T) {
	t.Setenv("GMUX_HASH_CACHE", filepath.Join(t.TempDir(), "hashes.json"))
	target := startHost(t)
	c, _ := remote.Dial(target)
	work := t.TempDir()
	big := make([]byte, 24<<20) // larger than any single read buffer
	for i := range big {
		big[i] = byte(i * 7)
	}
	os.WriteFile(filepath.Join(work, "weights.bin"), big, 0o644)
	os.WriteFile(filepath.Join(work, "run.sh"), []byte("wc -c weights.bin\n"), 0o755)
	sid, _ := c.EnsureSession("")
	if _, up, err := c.Sync(work, sid); err != nil || up != 2 || c.Hashed != 2 {
		t.Fatalf("first sync: up=%d hashed=%d err=%v", up, c.Hashed, err)
	}
	// a new client (a new gmux command) with the same cache reads nothing
	c2, _ := remote.Dial(target)
	if _, up, err := c2.Sync(work, sid); err != nil || up != 0 || c2.Hashed != 0 {
		t.Fatalf("unchanged re-sync: up=%d hashed=%d err=%v, want nothing read or sent", up, c2.Hashed, err)
	}
	var out bytes.Buffer
	if code, err := c2.Exec(context.Background(), sid, remote.ExecRequest{Command: []string{"sh", "run.sh"}, Share: 0.25}, &out, &bytes.Buffer{}); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if !strings.Contains(out.String(), "25165824") {
		t.Fatalf("the streamed 24 MiB file arrived as %q", out.String())
	}
	// a changed file is read again
	os.WriteFile(filepath.Join(work, "run.sh"), []byte("echo changed\n"), 0o755)
	if _, up, _ := c2.Sync(work, sid); up != 1 || c2.Hashed != 1 {
		t.Fatalf("after one change: up=%d hashed=%d", up, c2.Hashed)
	}
}

func TestRemoteJobsCarryTheirOwner(t *testing.T) {
	c, _ := remote.Dial(startHost(t))
	sid, _ := c.EnsureSession("")
	c.Sync(t.TempDir(), sid)
	c.Exec(context.Background(), sid, remote.ExecRequest{Command: []string{"true"}, Share: 0.25, Name: "step1", Owner: "vit-sbx-1"}, &bytes.Buffer{}, &bytes.Buffer{})
	u, err := c.Usage("1h", "owner")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := u["rows"].([]any)
	found := false
	for _, r := range rows {
		if m, _ := r.(map[string]any); m["key"] == "vit-sbx-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("usage by owner should list vit-sbx-1: %v", rows)
	}
}
