package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// NVIDIABackend shares cards with MPS. MPS runs as the container's own user,
// so no root is needed, and it enforces both a compute cap (active thread
// percentage) and a driver-level memory cap (pinned device memory limit).
// cuda-checkpoint, on recent drivers, moves a suspended job's GPU state to
// host memory so its GPU memory is actually freed.
type NVIDIABackend struct {
	r        Runner
	started  []Device
	stateDir string
}

func NewNVIDIA(r Runner) *NVIDIABackend { return &NVIDIABackend{r: r} }

func (b *NVIDIABackend) Vendor() string { return NVIDIA }

func (b *NVIDIABackend) Caps() Caps {
	return Caps{Concurrent: true, ComputeCap: true, MemoryCap: true, Checkpoint: b.r.Look("cuda-checkpoint")}
}

// Discover reads nvidia-smi. Memory is reported in MiB.
func (b *NVIDIABackend) Discover(ctx context.Context) ([]Device, error) {
	if !b.r.Look("nvidia-smi") {
		return nil, fmt.Errorf("nvidia-smi not found")
	}
	out, err := b.r.Run(ctx, nil, "nvidia-smi", "--query-gpu=index,uuid,name,memory.total", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseNvidiaSMI(string(out))
}

func parseNvidiaSMI(out string) ([]Device, error) {
	var devs []Device
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 4 {
			return nil, fmt.Errorf("unexpected nvidia-smi line %q", line)
		}
		idx, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		mem, err2 := strconv.Atoi(strings.TrimSpace(f[3]))
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("unexpected nvidia-smi line %q", line)
		}
		devs = append(devs, Device{Index: idx, UUID: strings.TrimSpace(f[1]), Name: strings.TrimSpace(f[2]), Vendor: NVIDIA, MemMiB: mem})
	}
	return devs, nil
}

func mpsDirs(stateDir string, idx int) (pipe, log string) {
	base := filepath.Join(stateDir, "mps", strconv.Itoa(idx))
	return filepath.Join(base, "pipe"), filepath.Join(base, "log")
}

// Start runs one MPS control daemon per card, each seeing only its card.
func (b *NVIDIABackend) Start(ctx context.Context, devs []Device, stateDir string) error {
	if !b.r.Look("nvidia-cuda-mps-control") {
		return fmt.Errorf("nvidia-cuda-mps-control not found: shares would not be enforced")
	}
	for _, d := range devs {
		pipe, log := mpsDirs(stateDir, d.Index)
		for _, dir := range []string{pipe, log} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		env := append(os.Environ(), "CUDA_MPS_PIPE_DIRECTORY="+pipe, "CUDA_MPS_LOG_DIRECTORY="+log,
			"CUDA_VISIBLE_DEVICES="+strconv.Itoa(d.Index))
		if _, err := b.r.Run(ctx, env, "nvidia-cuda-mps-control", "-d"); err != nil {
			return fmt.Errorf("start MPS on card %d: %w", d.Index, err)
		}
		b.started = append(b.started, d)
	}
	b.stateDir = stateDir
	return nil
}

// Stop asks each MPS daemon to quit. Every daemon is asked even if one fails.
func (b *NVIDIABackend) Stop(ctx context.Context) error {
	var first error
	for _, d := range b.started {
		pipe, log := mpsDirs(b.stateDir, d.Index)
		env := append(os.Environ(), "CUDA_MPS_PIPE_DIRECTORY="+pipe, "CUDA_MPS_LOG_DIRECTORY="+log)
		if _, err := b.r.RunIn(ctx, env, "quit\n", "nvidia-cuda-mps-control"); err != nil && first == nil {
			first = fmt.Errorf("stop MPS on card %d: %w", d.Index, err)
		}
	}
	b.started = nil
	return first
}

// JobEnv points the job at its card's MPS daemon with its caps. Inside the
// job the card is always device 0, which is what the memory limit names.
func (b *NVIDIABackend) JobEnv(dev Device, s Slot, stateDir string) map[string]string {
	pipe, log := mpsDirs(stateDir, dev.Index)
	return map[string]string{
		"CUDA_VISIBLE_DEVICES":              strconv.Itoa(dev.Index),
		"CUDA_MPS_PIPE_DIRECTORY":           pipe,
		"CUDA_MPS_LOG_DIRECTORY":            log,
		"CUDA_MPS_ACTIVE_THREAD_PERCENTAGE": strconv.Itoa(s.ComputePercent),
		"CUDA_MPS_PINNED_DEVICE_MEM_LIMIT":  fmt.Sprintf("0=%dM", s.MemMiB),
		"GMUX_SHARE":                        fmt.Sprintf("%d/%d", s.Seats, s.SeatsPerCard),
	}
}

// Suspend stops the job, then (if available) moves its GPU state to host
// memory so the card's memory is freed for whoever preempted it.
func (b *NVIDIABackend) Suspend(ctx context.Context, pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGSTOP); err != nil {
		return err
	}
	if b.r.Look("cuda-checkpoint") {
		if _, err := b.r.Run(ctx, nil, "cuda-checkpoint", "--toggle", "--pid", strconv.Itoa(pid)); err != nil {
			_ = syscall.Kill(-pid, syscall.SIGCONT)
			return err
		}
	}
	return nil
}

// Resume restores GPU state (if it was moved) and continues the job.
func (b *NVIDIABackend) Resume(ctx context.Context, pid int) error {
	if b.r.Look("cuda-checkpoint") {
		if _, err := b.r.Run(ctx, nil, "cuda-checkpoint", "--toggle", "--pid", strconv.Itoa(pid)); err != nil {
			return err
		}
	}
	return syscall.Kill(-pid, syscall.SIGCONT)
}
