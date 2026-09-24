// Package scheduler decides where jobs run and who gives way.
//
// It is pure: no processes, no clocks it does not receive, no I/O. The daemon
// asks it for decisions and carries them out, so every rule here can be tested
// directly.
//
// The rules, in order:
//
//  1. A job asks for a share of a card, optionally a memory amount, a number
//     of cards, and a priority. Its share becomes whole seats.
//  2. Placement is best fit: the card that would have the fewest free seats
//     left, so large holes stay available for large jobs. A job that needs
//     several cards gets all of them at once or none (gang placement).
//  3. If nothing fits, jobs with a lower priority that are marked preemptible
//     can be pushed off, lowest priority and newest first, until it fits.
//  4. If it still does not fit, the job waits in the queue or is refused,
//     whichever it asked for.
//  5. A burst job may use idle compute above its guarantee (its compute cap
//     is the whole card). Its guarantee is still enforced by admission and
//     preemption, so bursting never takes a seat from anyone.
package scheduler

import (
	"errors"
	"fmt"
	"sort"

	"github.com/numinous-technology/gmux/internal/share"
)

// Card is one GPU as the scheduler sees it.
type Card struct {
	Index  int
	Name   string
	MemMiB int
	Seats  int
	Down   bool // draining or unhealthy: holds its jobs, takes no new ones
}

// Request is what a job asks for.
type Request struct {
	ID          string
	Share       float64 // fraction of one card, in (0, 1]
	MemMiB      int     // 0 means proportional to the share
	GPUs        int     // cards needed, all at once; 0 means 1
	Priority    int     // higher wins
	Preemptible bool    // may be pushed off for a higher priority job
	Burst       bool    // may use idle compute above its guarantee
	Wait        bool    // queue instead of failing when nothing fits
}

// Slot is one card's part of a placement.
type Slot struct {
	Card           int     `json:"card"`
	Seats          int     `json:"seats"`
	GrantedShare   float64 `json:"granted_share"`
	MemMiB         int     `json:"mem_mib"`
	ComputePercent int     `json:"compute_percent"`
	// SeatIDs are the specific seats this slot holds on the card, so backends
	// that partition hardware (AMD compute units) give each job its own part
	// of the card instead of every job the same part.
	SeatIDs []int `json:"seat_ids"`
}

// Placement is where a job runs.
type Placement struct {
	JobID string `json:"job_id"`
	Slots []Slot `json:"slots"`
}

type placed struct {
	req   Request
	slots []Slot
	order uint64 // admission order, for "newest first"
}

// Decision is what Admit concluded.
type Decision struct {
	Placement *Placement
	Preempt   []string // jobs the caller must suspend before starting this one
	Queued    bool
}

// ErrNoFit means the job cannot run now and did not ask to wait.
var ErrNoFit = errors.New("does not fit")

// Scheduler holds the cards and who is on them.
type Scheduler struct {
	cards   []Card
	running map[string]*placed
	queue   []Request
	next    uint64
}

// New makes a scheduler over some cards.
func New(cards []Card) *Scheduler {
	cs := make([]Card, len(cards))
	copy(cs, cards)
	for i := range cs {
		if cs[i].Seats < 1 {
			cs[i].Seats = share.DefaultSeats
		}
	}
	return &Scheduler{cards: cs, running: map[string]*placed{}}
}

// Cards returns a copy of the cards.
func (s *Scheduler) Cards() []Card {
	out := make([]Card, len(s.cards))
	copy(out, s.cards)
	return out
}

// SetDown marks a card as draining (true) or back in service (false).
func (s *Scheduler) SetDown(card int, down bool) error {
	for i := range s.cards {
		if s.cards[i].Index == card {
			s.cards[i].Down = down
			return nil
		}
	}
	return fmt.Errorf("no card %d", card)
}

func (s *Scheduler) card(idx int) *Card {
	for i := range s.cards {
		if s.cards[i].Index == idx {
			return &s.cards[i]
		}
	}
	return nil
}

// usage is seats and memory in use on a card, optionally ignoring some jobs.
func (s *Scheduler) usage(card int, ignore map[string]bool) (seats, mem int) {
	for id, p := range s.running {
		if ignore[id] {
			continue
		}
		for _, sl := range p.slots {
			if sl.Card == card {
				seats += sl.Seats
				mem += sl.MemMiB
			}
		}
	}
	return
}

// need is the seats and memory a request takes on one card.
func need(r Request, c *Card) (seats, mem int, err error) {
	seats, err = share.Seats(r.Share, c.Seats)
	if err != nil {
		return 0, 0, err
	}
	mem = r.MemMiB
	if mem == 0 {
		mem = share.DefaultMemMiB(seats, c.Seats, c.MemMiB)
	}
	if mem > c.MemMiB {
		return 0, 0, fmt.Errorf("asks for %s but card %d has %s", share.FormatMem(mem), c.Index, share.FormatMem(c.MemMiB))
	}
	return seats, mem, nil
}

func (s *Scheduler) slot(r Request, c *Card, seats, mem int, ignore map[string]bool, keep []int) Slot {
	pct := share.ComputePercent(seats, c.Seats)
	if r.Burst {
		pct = 100
	}
	return Slot{Card: c.Index, Seats: seats, GrantedShare: share.Granted(seats, c.Seats), MemMiB: mem,
		ComputePercent: pct, SeatIDs: s.pickSeats(c, seats, ignore, keep)}
}

// occupied marks the seats in use on a card, ignoring some jobs.
func (s *Scheduler) occupied(c *Card, ignore map[string]bool) []bool {
	used := make([]bool, c.Seats)
	for id, p := range s.running {
		if ignore[id] {
			continue
		}
		for _, sl := range p.slots {
			if sl.Card != c.Index {
				continue
			}
			for _, i := range sl.SeatIDs {
				if i >= 0 && i < len(used) {
					used[i] = true
				}
			}
		}
	}
	return used
}

// pickSeats chooses n free seats on a card. Seats in keep that are still free
// come first (a resize keeps its place); then the first contiguous run long
// enough; then the lowest free seats. Callers have already checked n fit.
func (s *Scheduler) pickSeats(c *Card, n int, ignore map[string]bool, keep []int) []int {
	used := s.occupied(c, ignore)
	var out []int
	for _, i := range keep {
		if len(out) == n {
			break
		}
		if i >= 0 && i < len(used) && !used[i] {
			out = append(out, i)
			used[i] = true
		}
	}
	need := n - len(out)
	if need == 0 {
		return sortedInts(out)
	}
	if len(out) == 0 {
		for start := 0; start+need <= len(used); start++ {
			run := true
			for i := start; i < start+need; i++ {
				if used[i] {
					run = false
					break
				}
			}
			if run {
				for i := start; i < start+need; i++ {
					out = append(out, i)
				}
				return out
			}
		}
	}
	for i := 0; i < len(used) && len(out) < n; i++ {
		if !used[i] {
			out = append(out, i)
			used[i] = true
		}
	}
	return sortedInts(out)
}

func sortedInts(v []int) []int { sort.Ints(v); return v }

// fit finds cards for r, ignoring the given jobs as if they were gone.
func (s *Scheduler) fit(r Request, ignore map[string]bool) ([]Slot, error) {
	want := r.GPUs
	if want < 1 {
		want = 1
	}
	type cand struct {
		c         *Card
		seats     int
		mem       int
		leftSeats int
	}
	var cands []cand
	var lastErr error
	for i := range s.cards {
		c := &s.cards[i]
		if c.Down {
			continue
		}
		seats, mem, err := need(r, c)
		if err != nil {
			lastErr = err
			continue
		}
		usedS, usedM := s.usage(c.Index, ignore)
		if usedS+seats > c.Seats || usedM+mem > c.MemMiB {
			continue
		}
		cands = append(cands, cand{c, seats, mem, c.Seats - usedS - seats})
	}
	if len(cands) < want {
		if lastErr != nil && len(cands) == 0 {
			return nil, lastErr
		}
		return nil, ErrNoFit
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].leftSeats != cands[j].leftSeats {
			return cands[i].leftSeats < cands[j].leftSeats
		}
		return cands[i].c.Index < cands[j].c.Index
	})
	out := make([]Slot, 0, want)
	for _, cd := range cands[:want] {
		out = append(out, s.slot(r, cd.c, cd.seats, cd.mem, ignore, nil))
	}
	return out, nil
}

// victims picks the smallest set of lower priority preemptible jobs, lowest
// priority and newest first, whose removal lets r fit.
func (s *Scheduler) victims(r Request) ([]string, []Slot) {
	var pool []*placed
	for _, p := range s.running {
		if p.req.Preemptible && p.req.Priority < r.Priority {
			pool = append(pool, p)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].req.Priority != pool[j].req.Priority {
			return pool[i].req.Priority < pool[j].req.Priority
		}
		return pool[i].order > pool[j].order
	})
	ignore := map[string]bool{}
	var chosen []string
	for _, p := range pool {
		ignore[p.req.ID] = true
		chosen = append(chosen, p.req.ID)
		if slots, err := s.fit(r, ignore); err == nil {
			// drop victims that turned out not to be needed
			needed := []string{}
			for _, id := range chosen {
				delete(ignore, id)
				if _, err := s.fit(r, ignore); err != nil {
					ignore[id] = true
					needed = append(needed, id)
				}
			}
			slots, _ = s.fit(r, ignore)
			return needed, slots
		}
	}
	return nil, nil
}

func validate(r Request) error {
	if r.ID == "" {
		return errors.New("job has no id")
	}
	if !(r.Share > 0 && r.Share <= 1) {
		return fmt.Errorf("share must be greater than 0 and at most 1, got %g", r.Share)
	}
	if r.GPUs < 0 {
		return errors.New("gpus cannot be negative")
	}
	return nil
}

// Admit decides what happens to a new job. On a placement with preemptions,
// the caller must suspend those jobs (and call Evict for each) before
// starting the new one; Admit has already reserved its seats.
func (s *Scheduler) Admit(r Request) (Decision, error) {
	if err := validate(r); err != nil {
		return Decision{}, err
	}
	if _, dup := s.running[r.ID]; dup {
		return Decision{}, fmt.Errorf("job %s is already running", r.ID)
	}
	if want := max(r.GPUs, 1); want > len(s.cards) {
		return Decision{}, fmt.Errorf("asks for %d cards but there are %d", want, len(s.cards))
	}
	slots, err := s.fit(r, nil)
	var preempt []string
	if errors.Is(err, ErrNoFit) {
		preempt, slots = s.victims(r)
		if slots != nil {
			err = nil
		}
	}
	if err != nil {
		if errors.Is(err, ErrNoFit) && r.Wait {
			s.queue = append(s.queue, r)
			return Decision{Queued: true}, nil
		}
		return Decision{}, err
	}
	for _, id := range preempt {
		delete(s.running, id)
	}
	s.next++
	s.running[r.ID] = &placed{req: r, slots: slots, order: s.next}
	return Decision{Placement: &Placement{JobID: r.ID, Slots: slots}, Preempt: preempt}, nil
}

// Evict removes a job without starting anything from the queue (used for
// preempted jobs, whose seats were already handed to someone else).
func (s *Scheduler) Evict(id string) { delete(s.running, id) }

// Release frees a job's seats and returns the queued jobs that now fit, in
// priority then arrival order. The caller starts them.
func (s *Scheduler) Release(id string) []Decision {
	delete(s.running, id)
	return s.drain()
}

func (s *Scheduler) drain() []Decision {
	sort.SliceStable(s.queue, func(i, j int) bool { return s.queue[i].Priority > s.queue[j].Priority })
	var started []Decision
	var rest []Request
	for _, r := range s.queue {
		slots, err := s.fit(r, nil)
		if err != nil {
			rest = append(rest, r)
			continue
		}
		s.next++
		s.running[r.ID] = &placed{req: r, slots: slots, order: s.next}
		started = append(started, Decision{Placement: &Placement{JobID: r.ID, Slots: slots}})
	}
	s.queue = rest
	return started
}

// Requeue puts a preempted job back at the front of its priority band, so it
// resumes as soon as there is room.
func (s *Scheduler) Requeue(r Request) {
	r.Wait = true
	s.queue = append([]Request{r}, s.queue...)
}

// Cancel removes a queued job.
func (s *Scheduler) Cancel(id string) bool {
	for i, r := range s.queue {
		if r.ID == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return true
		}
	}
	return false
}

// Queue returns the waiting jobs.
func (s *Scheduler) Queue() []Request {
	out := make([]Request, len(s.queue))
	copy(out, s.queue)
	return out
}

// Placement returns where a running job is.
func (s *Scheduler) Placement(id string) (*Placement, bool) {
	p, ok := s.running[id]
	if !ok {
		return nil, false
	}
	return &Placement{JobID: id, Slots: append([]Slot(nil), p.slots...)}, true
}

// Resize changes a running job's share on the cards it already holds.
// Growing needs free seats on every one of them; it never preempts.
func (s *Scheduler) Resize(id string, fraction float64, memMiB int) (*Placement, error) {
	p, ok := s.running[id]
	if !ok {
		return nil, fmt.Errorf("job %s is not running", id)
	}
	r := p.req
	r.Share = fraction
	if memMiB > 0 {
		r.MemMiB = memMiB
	}
	if err := validate(r); err != nil {
		return nil, err
	}
	ignore := map[string]bool{id: true}
	var slots []Slot
	for _, old := range p.slots {
		c := s.card(old.Card)
		seats, mem, err := need(r, c)
		if err != nil {
			return nil, err
		}
		usedS, usedM := s.usage(c.Index, ignore)
		if usedS+seats > c.Seats || usedM+mem > c.MemMiB {
			return nil, fmt.Errorf("card %d has %d free seats and %s free memory", c.Index,
				c.Seats-usedS, share.FormatMem(c.MemMiB-usedM))
		}
		slots = append(slots, s.slot(r, c, seats, mem, ignore, old.SeatIDs))
	}
	p.req, p.slots = r, slots
	return &Placement{JobID: id, Slots: slots}, nil
}

// Free is the seats and memory still available on each card.
type Free struct {
	Card   int `json:"card"`
	Seats  int `json:"free_seats"`
	MemMiB int `json:"free_mem_mib"`
}

// Available reports what is left on every card.
func (s *Scheduler) Available() []Free {
	var out []Free
	for _, c := range s.cards {
		usedS, usedM := s.usage(c.Index, nil)
		out = append(out, Free{Card: c.Index, Seats: c.Seats - usedS, MemMiB: c.MemMiB - usedM})
	}
	return out
}
