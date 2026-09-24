package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
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
	// SysRoot is where the kernel's sysfs is mounted ("/sys"); tests point it
	// at recorded topology.
	SysRoot string
	devs    []Device
}

func NewAMD(r Runner) *AMDBackend {
	return &AMDBackend{r: r, Shim: os.Getenv("GMUX_SHIM"), SysRoot: "/sys"}
}

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
	b.readTopology(devs)
	b.devs = devs
	return devs, nil
}

// readTopology fills in each card's chiplet (XCD) and shader-engine counts from
// the KFD topology in sysfs, matching nodes to cards by unique id. It needs no
// root. Cards it cannot match keep zero, and get the plain contiguous mask.
func (b *AMDBackend) readTopology(devs []Device) {
	root := filepath.Join(b.SysRoot, "class/kfd/kfd/topology/nodes")
	nodes, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, n := range nodes {
		raw, err := os.ReadFile(filepath.Join(root, n.Name(), "properties"))
		if err != nil {
			continue
		}
		props := map[string]uint64{}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 {
				if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					props[f[0]] = v
				}
			}
		}
		xcc, arrays, perEngine := props["num_xcc"], props["array_count"], props["simd_arrays_per_engine"]
		if props["simd_count"] == 0 || xcc == 0 || arrays == 0 || perEngine == 0 {
			continue
		}
		id := fmt.Sprintf("%x", props["unique_id"])
		for i := range devs {
			if strings.TrimPrefix(strings.ToLower(devs[i].UUID), "0x") == id {
				devs[i].Chiplets = int(xcc)
				devs[i].Engines = int(arrays / perEngine / xcc)
			}
		}
	}
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

// seatCUMask is the HSA_CU_MASK for a job holding the given seats.
//
// ROCm deals mask bits round robin: bit i goes to chiplet (XCD) i mod X, and
// within a chiplet the bits walk its shader engines round robin, one compute
// unit per engine per round. The hardware splits a kernel's workgroups evenly
// across every engine the job has units on, so a job runs at the speed of its
// thinnest engine. Measured on an MI325X: a contiguous quarter that left one
// engine with a single unit ran at half the speed of its neighbours.
//
// So seats map to whole engines. With S seats and E engines per chiplet, the
// S/E seats of engine e share it on every chiplet: a job holding all of them
// gets every reliable round of that engine, and a job holding some of them
// gets an even share of rounds. Only rounds that exist on every engine are
// used (units/X/E of them), so no job ever lands one stray unit on an engine.
//
// Without chiplet and engine counts, or when the seat count does not divide
// evenly into engines, it falls back to a contiguous block per seat. It
// returns "" when the job has the whole card.
func seatCUMask(seatIDs []int, seats int, dev Device, n int) string {
	total := dev.Units
	if total <= 0 || n >= total {
		return ""
	}
	if len(seatIDs) >= seats && seats > 0 {
		return ""
	}
	m := new(big.Int)
	X, E := dev.Chiplets, dev.Engines
	switch {
	case X > 0 && E > 0 && total%X == 0 && len(seatIDs) > 0 && seats >= E && seats%E == 0:
		rounds := total / X / E // rounds present on every engine of every chiplet
		g := seats / E          // seats per engine
		held := map[int][]int{} // engine -> seat offsets held within it
		for _, s := range seatIDs {
			held[s/g] = append(held[s/g], s%g)
		}
		for e, offs := range held {
			var use []int
			if len(offs) == g {
				for r := 0; r < rounds; r++ {
					use = append(use, r)
				}
			} else {
				per := rounds / g
				for _, o := range offs {
					for r := o; r < per*g; r += g {
						use = append(use, r)
					}
				}
			}
			for _, r := range use {
				local := r*E + e
				for x := 0; x < X; x++ {
					m.SetBit(m, local*X+x, 1)
				}
			}
		}
	case len(seatIDs) > 0 && seats > 0:
		for _, i := range seatIDs {
			lo, hi := i*total/seats, (i+1)*total/seats
			for u := lo; u < hi; u++ {
				m.SetBit(m, u, 1)
			}
		}
	default:
		if n < 1 {
			n = 1
		}
		m.Lsh(big.NewInt(1), uint(n))
		m.Sub(m, big.NewInt(1))
	}
	if m.Sign() == 0 {
		return ""
	}
	return "0x" + m.Text(16)
}

// JobEnv selects the card, masks its compute units to the job's own seats when
// the unit count is known, and caps HIP allocations through the shim.
//
// The card is selected once, through ROCR_VISIBLE_DEVICES, by its UUID when
// known (the rocm-smi card number is a DRM number and need not match the
// runtime's device order). After that filter the card is device 0 to HIP and
// to HSA_CU_MASK. Setting HIP_VISIBLE_DEVICES to the card number as well would
// filter twice and hide every card but the first.
func (b *AMDBackend) JobEnv(dev Device, s Slot, stateDir string) map[string]string {
	sel := strconv.Itoa(dev.Index)
	if u := strings.TrimPrefix(strings.ToLower(dev.UUID), "0x"); u != "" {
		sel = "GPU-" + u
	}
	env := map[string]string{
		"ROCR_VISIBLE_DEVICES": sel,
		"HIP_VISIBLE_DEVICES":  "0",
		"GMUX_MEM_LIMIT_MIB":   strconv.Itoa(s.MemMiB),
		"GMUX_SHARE":           fmt.Sprintf("%d/%d", s.Seats, s.SeatsPerCard),
	}
	if dev.Units > 0 && s.ComputePercent < 100 {
		n := int(math.Ceil(float64(dev.Units) * float64(s.ComputePercent) / 100))
		if m := seatCUMask(s.SeatIDs, s.SeatsPerCard, dev, n); m != "" {
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
