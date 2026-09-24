// Package gpu is everything gmux needs to know about a particular vendor.
//
// The scheduler only knows cards, seats and memory. A Backend turns that into
// something a GPU enforces: it finds the cards, starts whatever sharing daemon
// the vendor provides, builds the environment that binds a job to its share,
// and pauses or resumes a job. Vendors differ a lot in what they can enforce
// without root, so every backend states its capabilities and gmux reports
// them rather than pretending.
package gpu

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Vendor names.
const (
	NVIDIA = "nvidia"
	AMD    = "amd"
	Intel  = "intel"
	Fake   = "fake"
)

// Device is one physical GPU.
type Device struct {
	Index  int    `json:"index"` // index within its vendor, as that vendor's tools number it
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
	MemMiB int    `json:"mem_mib"`
	Units  int    `json:"compute_units,omitempty"` // SMs, CUs or Xe cores, when known
	// Chiplets and Engines describe how compute units are grouped, when the
	// vendor publishes it (AMD: XCDs and shader engines per XCD). Backends that
	// partition by compute unit use it to give every job a balanced shape.
	Chiplets int `json:"chiplets,omitempty"`
	Engines  int `json:"engines_per_chiplet,omitempty"`
}

// Caps is what a backend can enforce on a shared card without host access.
type Caps struct {
	Concurrent bool `json:"concurrent"`  // kernels from different jobs run at the same time
	ComputeCap bool `json:"compute_cap"` // a job's share of compute is enforced
	MemoryCap  bool `json:"memory_cap"`  // a job cannot allocate past its memory
	Checkpoint bool `json:"checkpoint"`  // a suspended job's GPU memory is released
}

// Slot is the part of a placement a backend needs to bind a job.
type Slot struct {
	Seats          int
	SeatsPerCard   int
	MemMiB         int
	ComputePercent int
	SeatIDs        []int // the specific seats held; backends that partition hardware use them
}

// Backend is one vendor.
type Backend interface {
	Vendor() string
	Caps() Caps
	Discover(ctx context.Context) ([]Device, error)
	// Start prepares sharing on these devices (for NVIDIA, one MPS daemon each).
	Start(ctx context.Context, devs []Device, stateDir string) error
	Stop(ctx context.Context) error
	// JobEnv binds a process to its share of one device.
	JobEnv(dev Device, slot Slot, stateDir string) map[string]string
	// Suspend and Resume pause a job's GPU work. With Caps().Checkpoint the
	// job's GPU memory is freed while suspended; without it the memory stays
	// held and only compute is given back.
	Suspend(ctx context.Context, pid int) error
	Resume(ctx context.Context, pid int) error
}

// Runner executes a command. Tests replace it with recorded output.
type Runner interface {
	Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
	// RunIn is Run with stdin, for tools driven by commands on stdin.
	RunIn(ctx context.Context, env []string, stdin, name string, args ...string) ([]byte, error)
	Look(name string) bool
}

// Exec is the real runner.
type Exec struct{}

func (e Exec) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	return e.RunIn(ctx, env, "", name, args...)
}

func (Exec) RunIn(ctx context.Context, env []string, stdin, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (Exec) Look(name string) bool { _, err := exec.LookPath(name); return err == nil }

// Detect returns a backend for every vendor whose tools are present and
// answer. A machine with two vendors gets two backends.
func Detect(ctx context.Context, r Runner) []Backend {
	var out []Backend
	for _, b := range []Backend{NewNVIDIA(r), NewAMD(r), NewIntel(r)} {
		if devs, err := b.Discover(ctx); err == nil && len(devs) > 0 {
			out = append(out, b)
		}
	}
	return out
}

func mib(bytesStr string) int {
	var b float64
	fmt.Sscan(strings.TrimSpace(bytesStr), &b)
	return int(b / (1024 * 1024))
}
