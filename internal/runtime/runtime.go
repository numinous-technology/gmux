// Package runtime launches and controls a job's process.
//
// A job is a command gmux runs with the GPU environment its share needs, in
// its own process group so the whole tree can be signalled at once. When the
// job has a network allowlist, the command is run through the fence: gmux
// re-execs itself as a fenced child, which installs the seccomp filter and
// execs the command, while the daemon serves the supervisor.
package runtime

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Spec is everything needed to start a job's process.
type Spec struct {
	ID       string
	Command  []string
	Env      map[string]string // GPU bindings from the backend, merged over the daemon's env
	Dir      string
	Allow    []string // network allowlist; empty means no fence
	DenyNet  bool     // no network at all
	Stdout   io.Writer
	Stderr   io.Writer
	SelfPath string // path to the gmux binary, for the fenced re-exec
}

// Proc is a running job.
type Proc struct {
	Spec    Spec
	Cmd     *exec.Cmd
	Started time.Time

	mu     sync.Mutex
	exited bool
	code   int
	waitCh chan struct{}
}

// Launcher starts processes. Tests replace start.
type Launcher struct {
	// StartFenced serves a fence supervisor for a job and returns once the
	// child has installed its filter. The daemon provides it; without a fence
	// it is nil.
	StartFenced func(sockFD int, allow []string) error
}

func env(base []string, extra map[string]string) []string {
	m := map[string]string{}
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range extra {
		m[k] = v
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Start runs the job. Without an allowlist or deny it starts the command
// directly; otherwise it goes through the gmux fence.
func (l *Launcher) Start(s Spec) (*Proc, error) {
	if len(s.Command) == 0 {
		return nil, fmt.Errorf("job %s has no command", s.ID)
	}
	full := env(os.Environ(), s.Env)
	var cmd *exec.Cmd
	if s.DenyNet {
		cmd = exec.Command(s.SelfPath, append([]string{"__fence", "--deny", "--"}, s.Command...)...)
		cmd.Env = full
	} else if len(s.Allow) > 0 {
		return l.startFenced(s, full)
	} else {
		cmd = exec.Command(s.Command[0], s.Command[1:]...)
		cmd.Env = full
	}
	cmd.Dir = s.Dir
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return newProc(s, cmd), nil
}

func (l *Launcher) startFenced(s Spec, full []string) (*Proc, error) {
	if l.StartFenced == nil {
		return nil, fmt.Errorf("this build cannot enforce a network allowlist")
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	parent, child := fds[0], fds[1]
	cmd := exec.Command(s.SelfPath, append([]string{"__fence", "--allowlist", "--"}, s.Command...)...)
	cmd.Env = full
	cmd.Dir = s.Dir
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	cmd.ExtraFiles = []*os.File{os.NewFile(uintptr(child), "fence-sock")}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		syscall.Close(parent)
		syscall.Close(child)
		return nil, err
	}
	syscall.Close(child)
	// The daemon serves the supervisor on the parent socket. It reads the
	// listener fd the child sends and answers until the job exits.
	if err := l.StartFenced(parent, s.Allow); err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("start fence for %s: %w", s.ID, err)
	}
	return newProc(s, cmd), nil
}

func newProc(s Spec, cmd *exec.Cmd) *Proc {
	p := &Proc{Spec: s, Cmd: cmd, Started: time.Now(), waitCh: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exited = true
		p.code = exitCode(err)
		p.mu.Unlock()
		close(p.waitCh)
	}()
	return p
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
	}
	return 1
}

// Signal sends sig to the whole process group.
func (p *Proc) Signal(sig syscall.Signal) error {
	if p.Cmd.Process == nil {
		return fmt.Errorf("job not started")
	}
	return syscall.Kill(-p.Cmd.Process.Pid, sig)
}

// PID is the job's process id.
func (p *Proc) PID() int {
	if p.Cmd.Process == nil {
		return 0
	}
	return p.Cmd.Process.Pid
}

// Done is closed when the job exits.
func (p *Proc) Done() <-chan struct{} { return p.waitCh }

// Exited reports whether the job has exited and its code.
func (p *Proc) Exited() (bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited, p.code
}

// Stop signals the group, then kills it if it does not exit within grace.
func (p *Proc) Stop(grace time.Duration) {
	p.Signal(syscall.SIGTERM)
	select {
	case <-p.waitCh:
	case <-time.After(grace):
		p.Signal(syscall.SIGKILL)
		<-p.waitCh
	}
}
