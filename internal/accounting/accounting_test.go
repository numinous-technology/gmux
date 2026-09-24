package accounting

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestIntervalsAreSummedByJobCardAndVendor(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	l.Begin([]Interval{{Job: "j1", Name: "train", Owner: "ana", Vendor: "nvidia", Card: 0, CardName: "H100", Share: 0.25, MemMiB: 20480, Start: t0}})
	l.Begin([]Interval{
		{Job: "j2", Name: "pair", Owner: "ben", Vendor: "amd", Card: 0, CardName: "MI300X", Share: 0.5, MemMiB: 98304, Start: t0},
		{Job: "j2", Name: "pair", Owner: "ben", Vendor: "amd", Card: 1, CardName: "MI300X", Share: 0.5, MemMiB: 98304, Start: t0},
	})
	if err := l.End("j1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.End("j2", t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	byJob, _ := l.Usage(t0, t0.Add(2*time.Hour), "job")
	got := map[string]Row{}
	for _, r := range byJob {
		got[r.Key] = r
	}
	if !near(got["train"].GPUHours, 0.25) || !near(got["pair"].GPUHours, 0.5) {
		t.Fatalf("gpu hours: %+v", byJob)
	}
	if !near(got["train"].MemGiBHours, 20) {
		t.Fatalf("mem hours: %+v", got["train"])
	}
	byVendor, _ := l.Usage(t0, t0.Add(2*time.Hour), "vendor")
	if len(byVendor) != 2 {
		t.Fatalf("%+v", byVendor)
	}
	byCard, _ := l.Usage(t0, t0.Add(2*time.Hour), "card")
	if len(byCard) != 3 {
		t.Fatalf("each card is its own row: %+v", byCard)
	}
	if _, err := l.Usage(t0, t0, "colour"); err == nil {
		t.Fatal("unknown grouping must be refused")
	}
}

func TestUsageIsClippedToTheWindowAndCountsOpenJobs(t *testing.T) {
	l, _ := Open(filepath.Join(t.TempDir(), "usage.jsonl"))
	l.Begin([]Interval{{Job: "long", Vendor: "nvidia", Share: 1, Start: t0}})
	rows, _ := l.Usage(t0.Add(time.Hour), t0.Add(90*time.Minute), "job")
	if len(rows) != 1 || !near(rows[0].Seconds, 1800) {
		t.Fatalf("an open job counts only the window: %+v", rows)
	}
}

func TestLedgerSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	l, _ := Open(path)
	l.Begin([]Interval{{Job: "a", Vendor: "intel", Share: 0.5, Start: t0}})
	l.End("a", t0.Add(time.Hour))
	again, _ := Open(path)
	rows, _ := again.Usage(t0, t0.Add(time.Hour), "job")
	if len(rows) != 1 || !near(rows[0].GPUHours, 0.5) {
		t.Fatalf("%+v", rows)
	}
}
