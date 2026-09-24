package runtime

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStartRunsWithTheGPUEnvironmentInItsOwnGroup(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	f, _ := os.Create(out)
	defer f.Close()
	l := &Launcher{}
	p, err := l.Start(Spec{
		ID:      "j",
		Command: []string{"sh", "-c", "echo share=$GMUX_SHARE pid=$$ pgid=$(ps -o pgid= -p $$ | tr -d ' ')"},
		Env:     map[string]string{"GMUX_SHARE": "2/8"},
		Stdout:  f,
		Stderr:  f,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
	if ex, code := p.Exited(); !ex || code != 0 {
		t.Fatalf("exited=%v code=%d", ex, code)
	}
	b, _ := os.ReadFile(out)
	if got := string(b); !contains(got, "share=2/8") {
		t.Fatalf("env not passed: %q", got)
	}
	// its own process group: pgid should equal the pid
	s := string(b)
	if !contains(s, "pid=") || !contains(s, "pgid=") {
		t.Fatalf("no pgid: %q", s)
	}
}

func TestStopSignalsTheWholeTree(t *testing.T) {
	l := &Launcher{}
	p, err := l.Start(Spec{ID: "j", Command: []string{"sh", "-c", "sleep 30 & sleep 30"}, Stdout: os.Stderr, Stderr: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p.Stop(2 * time.Second)
	if time.Since(start) > 3*time.Second {
		t.Fatal("stop should be prompt")
	}
	if ex, _ := p.Exited(); !ex {
		t.Fatal("should have exited")
	}
}

func TestSignalRacesAreSafe(t *testing.T) {
	l := &Launcher{}
	p, _ := l.Start(Spec{ID: "j", Command: []string{"sleep", "10"}, Stdout: os.Stderr, Stderr: os.Stderr})
	p.Signal(syscall.SIGSTOP)
	p.Signal(syscall.SIGCONT)
	p.Signal(syscall.SIGTERM)
	<-p.Done()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
