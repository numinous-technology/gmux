package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// AMDBackend shares ROCm cards. There is no MPS equivalent, but kernels from
// separate processes already run concurrently, a compute unit mask (HSA_CU_MASK)
// limits which CUs a process may use, and the gmux shim caps HIP allocations.
// Nothing here needs root. What it cannot do is free a suspended job's GPU
// memory: suspending only stops the job's compute.
type AMDBackend struct {
	r Runner
	// Shim is the path to libgmux.so. Without it HIP allocations are not capped.
	Shim string
	devs []Device
}

func NewAMD(r Runner) *AMDBackend { return &AMDBackend{r: r, Shim: os.Getenv("GMUX_SHIM")} }

func (b *AMDBackend) Vendor() string { return AMD }

// Caps: the compute cap needs every card's compute unit count (from rocminfo);
// the memory cap needs the shim.
func (b *AMDBackend) Caps() Caps {
	units := len(b.devs) > 0
	for _, d := range b.devs {
		if d.Units == 0 {
			units = false
		}
	}
	return Caps{Concurrent: true, ComputeCap: units, MemoryCap: b.Shim != "", Checkpoint: false}
}

// Discover reads rocm-smi's JSON. Memory is reported in bytes.
func (b *AMDBackend) Discover(ctx context.Context) ([]Device, error) {
	if !b.r.Look("rocm-smi") {
		return nil, fmt.Errorf("rocm-smi not found")
	}
	out, err := b.r.Run(ctx, nil, "rocm-smi", "--showproductname", "--showmeminfo", "vram", "--showuniqueid", "--json")
	if err != nil {
		return nil, err
	}
	devs, err := parseRocmSMI(out)
	if err != nil {
		return nil, err
	}
	if b.r.Look("rocminfo") {
		if info, err := b.r.Run(ctx, nil, "rocminfo"); err == nil {
			units := parseRocminfoUnits(string(info))
			for i := range devs {
				if i < len(units) {
					devs[i].Units = units[i]
				}
			}
		}
	}
	b.devs = devs
	return devs, nil
}

// parseRocminfoUnits returns the compute unit count of each GPU agent, in the
// order rocminfo lists them, which is the order rocm-smi numbers cards.
func parseRocminfoUnits(out string) []int {
	var units []int
	isGPU := false
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "Device Type:"):
			isGPU = strings.Contains(l, "GPU")
		case isGPU && strings.HasPrefix(l, "Compute Unit:"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, "Compute Unit:")))
			if err == nil {
				units = append(units, n)
			}
			isGPU = false
		}
	}
	return units
}

func parseRocmSMI(out []byte) ([]Device, error) {
	var raw map[string]map[string]string
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("unexpected rocm-smi output: %w", err)
	}
	var devs []Device
	for key, f := range raw {
		if !strings.HasPrefix(key, "card") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(key, "card"))
		if err != nil {
			continue
		}
		name := first(f, "Card Series", "Card series", "Card SKU", "Card model")
		devs = append(devs, Device{Index: idx, UUID: first(f, "Unique ID"), Name: name, Vendor: AMD,
			MemMiB: mib(first(f, "VRAM Total Memory (B)"))})
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Index < devs[j].Index })
	if len(devs) == 0 {
		return nil, fmt.Errorf("rocm-smi reported no cards")
	}
	return devs, nil
}

func first(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

func (b *AMDBackend) Start(ctx context.Context, devs []Device, stateDir string) error { return nil }
func (b *AMDBackend) Stop(ctx context.Context) error                                  { return nil }

// cuMask is a hex mask with the lowest n of total compute units set, or ""
// when no mask is needed (the whole card).
func cuMask(n, total int) string {
	if total <= 0 || n >= total {
		return ""
	}
	if n < 1 {
		n = 1
	}
	m := new(big.Int).Lsh(big.NewInt(1), uint(n))
	m.Sub(m, big.NewInt(1))
	return "0x" + m.Text(16)
}

// JobEnv selects the card, masks its compute units when their count is known,
// and caps HIP allocations through the shim.
func (b *AMDBackend) JobEnv(dev Device, s Slot, stateDir string) map[string]string {
	env := map[string]string{
		"HIP_VISIBLE_DEVICES":  strconv.Itoa(dev.Index),
		"ROCR_VISIBLE_DEVICES": strconv.Itoa(dev.Index),
		"GMUX_MEM_LIMIT_MIB":   strconv.Itoa(s.MemMiB),
		"GMUX_SHARE":           fmt.Sprintf("%d/%d", s.Seats, s.SeatsPerCard),
	}
	if dev.Units > 0 && s.ComputePercent < 100 {
		n := int(math.Ceil(float64(dev.Units) * float64(s.ComputePercent) / 100))
		if m := cuMask(n, dev.Units); m != "" {
			env["HSA_CU_MASK"] = "0:" + m
		}
	}
	if b.Shim != "" {
		env["LD_PRELOAD"] = b.Shim
	}
	return env
}

func (b *AMDBackend) Suspend(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGSTOP)
}
func (b *AMDBackend) Resume(ctx context.Context, pid int) error {
	return syscall.Kill(-pid, syscall.SIGCONT)
}
