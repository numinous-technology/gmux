package gpu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"github.com/numinous-technology/gmux/internal/share"
)

// FakeBackend pretends to have cards. It is for trying gmux on a laptop and
// for tests: jobs run for real, they just have no GPU to use.
type FakeBackend struct{ devs []Device }

// NewFake parses a spec like "2xH100:80G,1xL4:24G".
func NewFake(spec string) (*FakeBackend, error) {
	var devs []Device
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		count := 1
		if i := strings.Index(part, "x"); i > 0 {
			if n, err := strconv.Atoi(part[:i]); err == nil {
				count, part = n, part[i+1:]
			}
		}
		name, memStr, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("fake gpu %q needs a memory size, like H100:80G", part)
		}
		mem, err := share.ParseMem(memStr)
		if err != nil {
			return nil, err
		}
		for i := 0; i < count; i++ {
			devs = append(devs, Device{Index: len(devs), UUID: fmt.Sprintf("FAKE-%d", len(devs)), Name: name, Vendor: Fake, MemMiB: mem})
		}
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("no fake gpus in %q", spec)
	}
	return &FakeBackend{devs: devs}, nil
}

func (b *FakeBackend) Vendor() string { return Fake }
func (b *FakeBackend) Caps() Caps {
	return Caps{Concurrent: true, ComputeCap: true, MemoryCap: true, Checkpoint: true}
}
func (b *FakeBackend) Discover(ctx context.Context) ([]Device, error)                  { return b.devs, nil }
func (b *FakeBackend) Start(ctx context.Context, devs []Device, stateDir string) error { return nil }
func (b *FakeBackend) Stop(ctx context.Context) error                                  { return nil }
func (b *FakeBackend) JobEnv(dev Device, s Slot, stateDir string) map[string]string {
	return map[string]string{
		"GMUX_FAKE_GPU":        strconv.Itoa(dev.Index),
		"GMUX_COMPUTE_PERCENT": strconv.Itoa(s.ComputePercent),
		"GMUX_MEM_LIMIT_MIB":   strconv.Itoa(s.MemMiB),
		"GMUX_SHARE":           fmt.Sprintf("%d/%d", s.Seats, s.SeatsPerCard),
	}
}
func (b *FakeBackend) Suspend(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGSTOP)
}
func (b *FakeBackend) Resume(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGCONT)
}
