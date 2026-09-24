package gpu

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// recorded answers each command with a file from testdata, as if the vendor's
// tool had printed it.
type recorded struct {
	files map[string]string // command name -> testdata file
	calls []string
}

func (r *recorded) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	return r.RunIn(ctx, env, "", name, args...)
}

func (r *recorded) RunIn(ctx context.Context, env []string, stdin, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")+" "+stdin))
	f, ok := r.files[name]
	if !ok || f == "" {
		return nil, nil
	}
	return os.ReadFile("testdata/" + f)
}

func (r *recorded) Look(name string) bool { _, ok := r.files[name]; return ok }

func TestNVIDIADiscoversEveryModel(t *testing.T) {
	b := NewNVIDIA(&recorded{files: map[string]string{"nvidia-smi": "nvidia-smi.csv"}})
	devs, err := b.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		mem  int
	}{{"NVIDIA H100 80GB HBM3", 81559}, {"NVIDIA A100-SXM4-40GB", 40960}, {"NVIDIA L4", 23034},
		{"NVIDIA GeForce RTX 4090", 24564}, {"Tesla T4", 15360}}
	if len(devs) != len(want) {
		t.Fatalf("found %d cards", len(devs))
	}
	for i, w := range want {
		if devs[i].Name != w.name || devs[i].MemMiB != w.mem || devs[i].Index != i || devs[i].Vendor != NVIDIA {
			t.Fatalf("card %d = %+v", i, devs[i])
		}
	}
}

func TestNVIDIAJobEnvBindsTheShareThroughMPS(t *testing.T) {
	b := NewNVIDIA(&recorded{files: map[string]string{"nvidia-smi": "nvidia-smi.csv", "nvidia-cuda-mps-control": ""}})
	env := b.JobEnv(Device{Index: 2, Vendor: NVIDIA}, Slot{Seats: 2, SeatsPerCard: 8, MemMiB: 5758, ComputePercent: 25}, "/state")
	for k, v := range map[string]string{
		"CUDA_VISIBLE_DEVICES":              "2",
		"CUDA_MPS_PIPE_DIRECTORY":           "/state/mps/2/pipe",
		"CUDA_MPS_ACTIVE_THREAD_PERCENTAGE": "25",
		"CUDA_MPS_PINNED_DEVICE_MEM_LIMIT":  "0=5758M",
		"GMUX_SHARE":                        "2/8",
	} {
		if env[k] != v {
			t.Fatalf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestNVIDIAStartsAndStopsOneMPSDaemonPerCard(t *testing.T) {
	r := &recorded{files: map[string]string{"nvidia-smi": "nvidia-smi.csv", "nvidia-cuda-mps-control": ""}}
	b := NewNVIDIA(r)
	devs, _ := b.Discover(context.Background())
	if err := b.Start(context.Background(), devs[:2], t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var starts, quits int
	for _, c := range r.calls {
		if c == "nvidia-cuda-mps-control -d" {
			starts++
		}
		if strings.HasPrefix(c, "nvidia-cuda-mps-control") && strings.HasSuffix(c, "quit") {
			quits++
		}
	}
	if starts != 2 || quits != 2 {
		t.Fatalf("starts=%d quits=%d calls=%v", starts, quits, r.calls)
	}
}

func TestNVIDIARefusesToStartWithoutMPS(t *testing.T) {
	b := NewNVIDIA(&recorded{files: map[string]string{"nvidia-smi": "nvidia-smi.csv"}})
	if err := b.Start(context.Background(), []Device{{Index: 0}}, t.TempDir()); err == nil {
		t.Fatal("without MPS a share would not be enforced; start must say so")
	}
}

func TestNVIDIACheckpointIsACapabilityOnlyWhenTheToolExists(t *testing.T) {
	with := NewNVIDIA(&recorded{files: map[string]string{"cuda-checkpoint": ""}})
	without := NewNVIDIA(&recorded{files: map[string]string{}})
	if !with.Caps().Checkpoint || without.Caps().Checkpoint {
		t.Fatal("checkpoint capability must follow the tool")
	}
}

func TestAMDDiscoversEveryModelWithComputeUnits(t *testing.T) {
	b := NewAMD(&recorded{files: map[string]string{"rocm-smi": "rocm-smi.json", "rocminfo": "rocminfo.txt"}})
	devs, err := b.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name  string
		mem   int
		units int
	}{{"AMD Instinct MI300X", 196592, 304}, {"AMD Instinct MI250X", 65520, 110}, {"Radeon RX 7900 XTX", 24560, 96}}
	if len(devs) != 3 {
		t.Fatalf("found %d cards: %+v", len(devs), devs)
	}
	for i, w := range want {
		if devs[i].Name != w.name || devs[i].MemMiB != w.mem || devs[i].Units != w.units {
			t.Fatalf("card %d = %+v, want %+v", i, devs[i], w)
		}
	}
	if !b.Caps().ComputeCap {
		t.Fatal("with every card's compute unit count known, compute is capped")
	}
}

func TestAMDWithoutRocminfoDoesNotClaimAComputeCap(t *testing.T) {
	b := NewAMD(&recorded{files: map[string]string{"rocm-smi": "rocm-smi.json"}})
	if _, err := b.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.Caps().ComputeCap {
		t.Fatal("no compute unit counts, no compute cap")
	}
	env := b.JobEnv(Device{Index: 0}, Slot{Seats: 2, SeatsPerCard: 8, MemMiB: 1024, ComputePercent: 25}, "")
	if _, ok := env["HSA_CU_MASK"]; ok {
		t.Fatal("no mask without a unit count")
	}
}

func TestAMDJobEnvMasksComputeUnitsAndCapsMemory(t *testing.T) {
	b := NewAMD(&recorded{})
	b.Shim = "/opt/gmux/libgmux.so"
	env := b.JobEnv(Device{Index: 1, Units: 110}, Slot{Seats: 2, SeatsPerCard: 8, MemMiB: 16380, ComputePercent: 25}, "")
	if env["HIP_VISIBLE_DEVICES"] != "1" || env["ROCR_VISIBLE_DEVICES"] != "1" {
		t.Fatalf("device selection: %v", env)
	}
	if env["GMUX_MEM_LIMIT_MIB"] != "16380" || env["LD_PRELOAD"] != "/opt/gmux/libgmux.so" {
		t.Fatalf("memory cap: %v", env)
	}
	// 25% of 110 CUs rounds up to 28: the lowest 28 bits set
	if env["HSA_CU_MASK"] != "0:0xfffffff" {
		t.Fatalf("cu mask = %q", env["HSA_CU_MASK"])
	}
	if !b.Caps().MemoryCap {
		t.Fatal("shim present, memory is capped")
	}
	whole := b.JobEnv(Device{Index: 0, Units: 304}, Slot{Seats: 8, SeatsPerCard: 8, ComputePercent: 100}, "")
	if _, ok := whole["HSA_CU_MASK"]; ok {
		t.Fatal("a whole card needs no mask")
	}
}

func TestCUMask(t *testing.T) {
	for _, c := range []struct {
		n, total int
		want     string
	}{{1, 8, "0x1"}, {4, 8, "0xf"}, {5, 8, "0x1f"}, {8, 8, ""}, {76, 304, "0x" + strings.Repeat("f", 19)}} {
		if got := cuMask(c.n, c.total); got != c.want {
			t.Fatalf("cuMask(%d,%d) = %q want %q", c.n, c.total, got, c.want)
		}
	}
}

func TestIntelDiscoversEveryModel(t *testing.T) {
	b := NewIntel(&recorded{files: map[string]string{"xpu-smi": "xpu-smi.json"}})
	devs, err := b.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		mem  int
	}{{"Intel(R) Data Center GPU Max 1550", 131072}, {"Intel(R) Data Center GPU Flex 170", 16384}, {"Intel(R) Arc(TM) A770 Graphics", 16368}}
	if len(devs) != 3 {
		t.Fatalf("found %d", len(devs))
	}
	for i, w := range want {
		if devs[i].Name != w.name || devs[i].MemMiB != w.mem {
			t.Fatalf("card %d = %+v", i, devs[i])
		}
	}
}

func TestIntelSaysSharesAreAdmissionOnly(t *testing.T) {
	b := NewIntel(&recorded{})
	c := b.Caps()
	if !c.Concurrent || c.ComputeCap || c.MemoryCap || c.Checkpoint {
		t.Fatalf("intel caps = %+v", c)
	}
	env := b.JobEnv(Device{Index: 2}, Slot{Seats: 4, SeatsPerCard: 8}, "")
	if env["ZE_AFFINITY_MASK"] != "2" {
		t.Fatalf("env %v", env)
	}
}

func TestDetectFindsEveryVendorPresent(t *testing.T) {
	r := &recorded{files: map[string]string{"nvidia-smi": "nvidia-smi.csv", "rocm-smi": "rocm-smi.json", "xpu-smi": "xpu-smi.json"}}
	var vendors []string
	for _, b := range Detect(context.Background(), r) {
		vendors = append(vendors, b.Vendor())
	}
	if fmt.Sprint(vendors) != "[nvidia amd intel]" {
		t.Fatalf("detected %v", vendors)
	}
	if got := Detect(context.Background(), &recorded{}); len(got) != 0 {
		t.Fatal("no tools, no backends")
	}
}

func TestFakeSpec(t *testing.T) {
	b, err := NewFake("2xH100:80G, 1xL4:24G")
	if err != nil {
		t.Fatal(err)
	}
	devs, _ := b.Discover(context.Background())
	if len(devs) != 3 || devs[0].MemMiB != 81920 || devs[2].Name != "L4" || devs[2].Index != 2 {
		t.Fatalf("%+v", devs)
	}
	if _, err := NewFake("H100"); err == nil {
		t.Fatal("memory size is required")
	}
}
