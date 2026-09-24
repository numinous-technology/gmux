package scheduler

import (
	"errors"
	"testing"
)

func h100s(n int) []Card {
	var cs []Card
	for i := 0; i < n; i++ {
		cs = append(cs, Card{Index: i, Name: "H100", MemMiB: 81920, Seats: 8})
	}
	return cs
}

func admit(t *testing.T, s *Scheduler, r Request) Decision {
	t.Helper()
	d, err := s.Admit(r)
	if err != nil {
		t.Fatalf("admit %s: %v", r.ID, err)
	}
	return d
}

func TestFourQuarterJobsFillOneCard(t *testing.T) {
	s := New(h100s(1))
	for _, id := range []string{"a", "b", "c", "d"} {
		d := admit(t, s, Request{ID: id, Share: 0.25})
		sl := d.Placement.Slots[0]
		if sl.Seats != 2 || sl.GrantedShare != 0.25 || sl.MemMiB != 20480 || sl.ComputePercent != 25 {
			t.Fatalf("%s got %+v", id, sl)
		}
	}
	if _, err := s.Admit(Request{ID: "e", Share: 0.125}); !errors.Is(err, ErrNoFit) {
		t.Fatalf("fifth job should not fit, got %v", err)
	}
}

func TestSharesRoundUpAndSayWhatWasGranted(t *testing.T) {
	s := New(h100s(1))
	d := admit(t, s, Request{ID: "a", Share: 0.3})
	if sl := d.Placement.Slots[0]; sl.Seats != 3 || sl.GrantedShare != 0.375 {
		t.Fatalf("got %+v", sl)
	}
}

func TestMemoryIsAdmittedToo(t *testing.T) {
	s := New(h100s(1))
	admit(t, s, Request{ID: "big", Share: 0.125, MemMiB: 70000})
	if _, err := s.Admit(Request{ID: "b", Share: 0.125, MemMiB: 20000}); !errors.Is(err, ErrNoFit) {
		t.Fatalf("memory should be full, got %v", err)
	}
	if _, err := s.Admit(Request{ID: "huge", Share: 0.1, MemMiB: 100000}); err == nil || errors.Is(err, ErrNoFit) {
		t.Fatalf("a request larger than the card is an error, not a wait: %v", err)
	}
}

func TestBestFitKeepsLargeHolesFree(t *testing.T) {
	s := New(h100s(2))
	admit(t, s, Request{ID: "half", Share: 0.5}) // card 0: 4 left
	d := admit(t, s, Request{ID: "small", Share: 0.25})
	if d.Placement.Slots[0].Card != 0 {
		t.Fatalf("small job should pack onto the fuller card, got %+v", d.Placement.Slots)
	}
	big := admit(t, s, Request{ID: "whole", Share: 1})
	if big.Placement.Slots[0].Card != 1 {
		t.Fatal("whole card job should still have a card")
	}
}

func TestGangPlacementIsAllOrNothing(t *testing.T) {
	s := New(h100s(2))
	admit(t, s, Request{ID: "occupant", Share: 0.75})
	if _, err := s.Admit(Request{ID: "pair", Share: 0.5, GPUs: 2}); !errors.Is(err, ErrNoFit) {
		t.Fatalf("only one card has room; the pair must not start on one, got %v", err)
	}
	if len(s.Available()) != 2 || s.Available()[1].Seats != 8 {
		t.Fatal("a failed gang must reserve nothing")
	}
	d := admit(t, s, Request{ID: "pair2", Share: 0.25, GPUs: 2})
	if len(d.Placement.Slots) != 2 || d.Placement.Slots[0].Card == d.Placement.Slots[1].Card {
		t.Fatalf("pair should span two cards: %+v", d.Placement.Slots)
	}
}

func TestPreemptionTakesTheFewestLowestNewestJobs(t *testing.T) {
	s := New(h100s(1))
	admit(t, s, Request{ID: "old-low", Share: 0.25, Priority: 1, Preemptible: true})
	admit(t, s, Request{ID: "new-low", Share: 0.25, Priority: 1, Preemptible: true})
	admit(t, s, Request{ID: "guarded", Share: 0.25, Priority: 0})
	admit(t, s, Request{ID: "mid", Share: 0.25, Priority: 2, Preemptible: true})
	d := admit(t, s, Request{ID: "urgent", Share: 0.25, Priority: 5})
	if len(d.Preempt) != 1 || d.Preempt[0] != "new-low" {
		t.Fatalf("should push off exactly the newest lowest priority job, got %v", d.Preempt)
	}
	if _, ok := s.Placement("new-low"); ok {
		t.Fatal("preempted job must lose its seats")
	}
	// jobs that are not preemptible are never pushed off, whatever their priority
	if _, err := s.Admit(Request{ID: "urgent2", Share: 0.75, Priority: 9}); !errors.Is(err, ErrNoFit) {
		t.Fatalf("guarded job and higher priority jobs must stay: %v", err)
	}
}

func TestEqualPriorityNeverPreempts(t *testing.T) {
	s := New(h100s(1))
	admit(t, s, Request{ID: "a", Share: 1, Priority: 3, Preemptible: true})
	if _, err := s.Admit(Request{ID: "b", Share: 0.5, Priority: 3}); !errors.Is(err, ErrNoFit) {
		t.Fatalf("equal priority must not preempt: %v", err)
	}
}

func TestQueueStartsJobsInPriorityOrderOnRelease(t *testing.T) {
	s := New(h100s(1))
	admit(t, s, Request{ID: "running", Share: 1})
	if d := admit(t, s, Request{ID: "low", Share: 0.5, Wait: true}); !d.Queued {
		t.Fatal("should queue")
	}
	admit(t, s, Request{ID: "high", Share: 0.75, Priority: 3, Wait: true})
	started := s.Release("running")
	if len(started) != 1 || started[0].Placement.JobID != "high" {
		t.Fatalf("high priority should start first and low should still wait, got %+v", started)
	}
	if q := s.Queue(); len(q) != 1 || q[0].ID != "low" {
		t.Fatalf("queue = %+v", q)
	}
}

func TestBurstJobsCapAtTheWholeCardButReserveOnlyTheirSeats(t *testing.T) {
	s := New(h100s(1))
	d := admit(t, s, Request{ID: "eval", Share: 0.25, Burst: true})
	if sl := d.Placement.Slots[0]; sl.ComputePercent != 100 || sl.Seats != 2 {
		t.Fatalf("got %+v", sl)
	}
	admit(t, s, Request{ID: "other", Share: 0.75})
}

func TestResizeGrowsOnlyIntoFreeSeats(t *testing.T) {
	s := New(h100s(1))
	admit(t, s, Request{ID: "a", Share: 0.25})
	admit(t, s, Request{ID: "b", Share: 0.5})
	p, err := s.Resize("a", 0.5, 0)
	if err != nil || p.Slots[0].Seats != 4 {
		t.Fatalf("grow into free seats: %+v %v", p, err)
	}
	if _, err := s.Resize("a", 0.75, 0); err == nil {
		t.Fatal("cannot grow past the card")
	}
	if p, _ := s.Resize("a", 0.125, 0); p.Slots[0].Seats != 1 {
		t.Fatal("shrink")
	}
}

func TestDrainingCardTakesNothingNew(t *testing.T) {
	s := New(h100s(2))
	if err := s.SetDown(0, true); err != nil {
		t.Fatal(err)
	}
	d := admit(t, s, Request{ID: "a", Share: 0.25})
	if d.Placement.Slots[0].Card != 1 {
		t.Fatal("drained card must be skipped")
	}
}

func TestRequeuedJobResumesFirst(t *testing.T) {
	s := New(h100s(1))
	admit(t, s, Request{ID: "low", Share: 1, Priority: 1, Preemptible: true})
	admit(t, s, Request{ID: "waiting", Share: 1, Priority: 1, Wait: true})
	d := admit(t, s, Request{ID: "urgent", Share: 1, Priority: 5})
	s.Requeue(Request{ID: d.Preempt[0], Share: 1, Priority: 1, Preemptible: true})
	started := s.Release("urgent")
	if len(started) != 1 || started[0].Placement.JobID != "low" {
		t.Fatalf("the preempted job resumes before a job that merely waited: %+v", started)
	}
}
