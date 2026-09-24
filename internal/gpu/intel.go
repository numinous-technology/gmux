package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"syscall"
)

// IntelBackend shares Intel data center and Arc GPUs through Level Zero.
// Jobs are pinned to a card with ZE_AFFINITY_MASK and run concurrently, but
// there is no per-process compute or memory cap available without host
// access, so a share is enforced only by admission. Suspending stops compute
// and keeps memory.
type IntelBackend struct{ r Runner }

func NewIntel(r Runner) *IntelBackend { return &IntelBackend{r: r} }

func (b *IntelBackend) Vendor() string { return Intel }

func (b *IntelBackend) Caps() Caps { return Caps{Concurrent: true} }

// Discover reads xpu-smi's JSON. Memory is reported in bytes.
func (b *IntelBackend) Discover(ctx context.Context) ([]Device, error) {
	if !b.r.Look("xpu-smi") {
		return nil, fmt.Errorf("xpu-smi not found")
	}
	out, err := b.r.Run(ctx, nil, "xpu-smi", "discovery", "-j")
	if err != nil {
		return nil, err
	}
	return parseXPUSMI(out)
}

func parseXPUSMI(out []byte) ([]Device, error) {
	var raw struct {
		DeviceList []struct {
			DeviceID   int    `json:"device_id"`
			DeviceName string `json:"device_name"`
			UUID       string `json:"uuid"`
			MemBytes   string `json:"memory_physical_size_byte"`
		} `json:"device_list"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("unexpected xpu-smi output: %w", err)
	}
	var devs []Device
	for _, d := range raw.DeviceList {
		devs = append(devs, Device{Index: d.DeviceID, UUID: d.UUID, Name: d.DeviceName, Vendor: Intel, MemMiB: mib(d.MemBytes)})
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("xpu-smi reported no cards")
	}
	return devs, nil
}

func (b *IntelBackend) Start(ctx context.Context, devs []Device, stateDir string) error { return nil }
func (b *IntelBackend) Stop(ctx context.Context) error                                  { return nil }

func (b *IntelBackend) JobEnv(dev Device, s Slot, stateDir string) map[string]string {
	return map[string]string{
		"ZE_AFFINITY_MASK": strconv.Itoa(dev.Index),
		"GMUX_SHARE":       fmt.Sprintf("%d/%d", s.Seats, s.SeatsPerCard),
	}
}

func (b *IntelBackend) Suspend(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGSTOP)
}
func (b *IntelBackend) Resume(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGCONT)
}
