// Package daemon is gmux running: it owns the cards, the scheduler, the ledger
// and the jobs, and answers the CLI and SDK over a local socket. One daemon
// per container.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/numinous-technology/gmux/internal/accounting"
	"github.com/numinous-technology/gmux/internal/gpu"
	"github.com/numinous-technology/gmux/internal/runtime"
	"github.com/numinous-technology/gmux/internal/scheduler"
	"github.com/numinous-technology/gmux/internal/share"
)

// Job is a running or queued job as the daemon tracks it.
type Job struct {
	ID        string               `json:"id"`
	Name      string               `json:"name,omitempty"`
	Owner     string               `json:"owner,omitempty"`
	Command   []string             `json:"command"`
	Share     float64              `json:"share"`
	Priority  int                  `json:"priority"`
	State     string               `json:"state"` // queued, running, paused, exited
	ExitCode  *int                 `json:"exit_code,omitempty"`
	Placement *scheduler.Placement `json:"placement,omitempty"`
	Started   time.Time            `json:"started,omitempty"`

	req    scheduler.Request
	submit SubmitRequest
	proc   *runtime.Proc
}

// Daemon holds everything.
type Daemon struct {
	mu       sync.Mutex
	sched    *scheduler.Scheduler
	backends map[string]gpu.Backend // vendor -> backend
	byCard   map[int]cardRef        // scheduler card index -> vendor + device
	launch   *runtime.Launcher
	ledger   *accounting.Ledger
	jobs     map[string]*Job
	stateDir string
	selfPath string
	seats    int
}

type cardRef struct {
	vendor string
	dev    gpu.Device
}

// Config configures a daemon.
type Config struct {
	StateDir string
	SelfPath string // path to the gmux binary
	Seats    int    // seats per card; 0 uses the default
	Fake     string // fake GPU spec, for running without hardware
}

// New discovers the GPUs, starts their sharing daemons, and returns a daemon.
func New(ctx context.Context, cfg Config) (*Daemon, error) {
	if cfg.Seats <= 0 {
		cfg.Seats = share.DefaultSeats
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	d := &Daemon{
		backends: map[string]gpu.Backend{},
		byCard:   map[int]cardRef{},
		launch:   &runtime.Launcher{},
		jobs:     map[string]*Job{},
		stateDir: cfg.StateDir,
		selfPath: cfg.SelfPath,
		seats:    cfg.Seats,
	}
	var backends []gpu.Backend
	if cfg.Fake != "" {
		b, err := gpu.NewFake(cfg.Fake)
		if err != nil {
			return nil, err
		}
		backends = []gpu.Backend{b}
	} else {
		backends = gpu.Detect(ctx, gpu.Exec{})
		if len(backends) == 0 {
			return nil, fmt.Errorf("no GPUs found (no nvidia-smi, rocm-smi or xpu-smi reported a card); use --fake to try gmux without hardware")
		}
	}
	var cards []scheduler.Card
	idx := 0
	for _, b := range backends {
		d.backends[b.Vendor()] = b
		devs, err := b.Discover(ctx)
		if err != nil {
			return nil, err
		}
		if err := b.Start(ctx, devs, cfg.StateDir); err != nil {
			return nil, fmt.Errorf("%s: %w", b.Vendor(), err)
		}
		for _, dev := range devs {
			cards = append(cards, scheduler.Card{Index: idx, Name: dev.Name, MemMiB: dev.MemMiB, Seats: cfg.Seats})
			d.byCard[idx] = cardRef{vendor: b.Vendor(), dev: dev}
			idx++
		}
	}
	l, err := accounting.Open(filepath.Join(cfg.StateDir, "usage.jsonl"))
	if err != nil {
		return nil, err
	}
	d.ledger = l
	d.sched = scheduler.New(cards)
	d.launch.StartFenced = d.startFence
	return d, nil
}

// Close stops the sharing daemons.
func (d *Daemon) Close(ctx context.Context) {
	for _, b := range d.backends {
		b.Stop(ctx)
	}
}

// SubmitRequest is what the API accepts to start a job.
type SubmitRequest struct {
	Command     []string `json:"command"`
	Name        string   `json:"name,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Share       float64  `json:"share"`
	MemMiB      int      `json:"mem_mib,omitempty"`
	GPUs        int      `json:"gpus,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	Preemptible bool     `json:"preemptible,omitempty"`
	Burst       bool     `json:"burst,omitempty"`
	Wait        bool     `json:"wait,omitempty"`
	Allow       []string `json:"allow,omitempty"`
	DenyNet     bool     `json:"deny_net,omitempty"`
	Dir         string   `json:"dir,omitempty"`
}

func newID() string { return fmt.Sprintf("j%d", time.Now().UnixNano()%1_000_000_000) }

// Submit admits and starts (or queues) a job.
func (d *Daemon) Submit(r SubmitRequest) (*Job, error) {
	if len(r.Command) == 0 {
		return nil, fmt.Errorf("a command is required")
	}
	if r.Share <= 0 {
		r.Share = 1
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	id := newID()
	req := scheduler.Request{ID: id, Share: r.Share, MemMiB: r.MemMiB, GPUs: r.GPUs,
		Priority: r.Priority, Preemptible: r.Preemptible, Burst: r.Burst, Wait: r.Wait}
	dec, err := d.sched.Admit(req)
	if err != nil {
		return nil, err
	}
	job := &Job{ID: id, Name: r.Name, Owner: r.Owner, Command: r.Command, Share: r.Share,
		Priority: r.Priority, req: req, submit: r}
	d.jobs[id] = job
	for _, victim := range dec.Preempt {
		if v := d.jobs[victim]; v != nil && v.proc != nil {
			d.pauseLocked(v)
			d.sched.Requeue(v.req)
			v.State = "paused"
		}
	}
	if dec.Queued {
		job.State = "queued"
		return job, nil
	}
	if err := d.startLocked(job, dec.Placement); err != nil {
		d.sched.Release(id)
		delete(d.jobs, id)
		return nil, err
	}
	return job, nil
}

func (d *Daemon) startLocked(job *Job, pl *scheduler.Placement) error {
	r := job.submit
	env := map[string]string{}
	var ivs []accounting.Interval
	for _, slot := range pl.Slots {
		ref := d.byCard[slot.Card]
		b := d.backends[ref.vendor]
		for k, v := range b.JobEnv(ref.dev, gpu.Slot{Seats: slot.Seats, SeatsPerCard: d.seats, MemMiB: slot.MemMiB, ComputePercent: slot.ComputePercent}, d.stateDir) {
			env[k] = v
		}
		ivs = append(ivs, accounting.Interval{Job: job.ID, Name: job.Name, Owner: job.Owner,
			Vendor: ref.vendor, Card: slot.Card, CardName: ref.dev.Name, Share: slot.GrantedShare,
			MemMiB: slot.MemMiB, Start: time.Now()})
	}
	proc, err := d.launch.Start(runtime.Spec{
		ID: job.ID, Command: job.Command, Env: env, Dir: r.Dir,
		Allow: r.Allow, DenyNet: r.DenyNet,
		Stdout: os.Stdout, Stderr: os.Stderr, SelfPath: d.selfPath,
	})
	if err != nil {
		return err
	}
	job.proc = proc
	job.State = "running"
	job.Placement = pl
	job.Started = proc.Started
	d.ledger.Begin(ivs)
	go d.reap(job.ID, proc)
	return nil
}

func (d *Daemon) reap(id string, proc *runtime.Proc) {
	<-proc.Done()
	d.mu.Lock()
	defer d.mu.Unlock()
	job := d.jobs[id]
	if job == nil || job.proc != proc {
		return
	}
	_, code := proc.Exited()
	job.State, job.ExitCode = "exited", &code
	job.proc = nil
	d.ledger.End(id, time.Now())
	for _, dec := range d.sched.Release(id) {
		if q := d.jobs[dec.Placement.JobID]; q != nil && (q.State == "queued" || q.State == "paused") {
			d.startLocked(q, dec.Placement)
		}
	}
}

func (d *Daemon) pauseLocked(job *Job) {
	if job.proc == nil {
		return
	}
	for _, slot := range job.Placement.Slots {
		b := d.backends[d.byCard[slot.Card].vendor]
		b.Suspend(context.Background(), job.proc.PID())
		break
	}
	d.ledger.End(job.ID, time.Now())
}

// Stop ends a job.
func (d *Daemon) Stop(id string) error {
	d.mu.Lock()
	job := d.jobs[id]
	if job == nil {
		d.mu.Unlock()
		return fmt.Errorf("no job %s", id)
	}
	if job.State == "queued" {
		d.sched.Cancel(id)
		delete(d.jobs, id)
		d.mu.Unlock()
		return nil
	}
	proc := job.proc
	d.mu.Unlock()
	if proc != nil {
		proc.Stop(10 * time.Second)
	}
	return nil
}

// Resize changes a running job's share.
func (d *Daemon) Resize(id string, fraction float64, memMiB int) (*Job, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	job := d.jobs[id]
	if job == nil || job.State != "running" {
		return nil, fmt.Errorf("job %s is not running", id)
	}
	pl, err := d.sched.Resize(id, fraction, memMiB)
	if err != nil {
		return nil, err
	}
	// re-apply the memory cap live where the backend supports it; the compute
	// cap and any live reapply is best effort and documented per vendor.
	d.ledger.End(id, time.Now())
	var ivs []accounting.Interval
	for _, slot := range pl.Slots {
		ref := d.byCard[slot.Card]
		ivs = append(ivs, accounting.Interval{Job: id, Name: job.Name, Owner: job.Owner,
			Vendor: ref.vendor, Card: slot.Card, CardName: ref.dev.Name, Share: slot.GrantedShare,
			MemMiB: slot.MemMiB, Start: time.Now()})
	}
	d.ledger.Begin(ivs)
	job.Share = fraction
	job.Placement = pl
	return job, nil
}

// Jobs returns a snapshot of all jobs, newest first.
func (d *Daemon) Jobs() []*Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*Job, 0, len(d.jobs))
	for _, j := range d.jobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Job returns one job.
func (d *Daemon) Job(id string) (*Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[id]
	if !ok {
		return nil, false
	}
	cp := *j
	return &cp, true
}

// CardView is a card and what is free on it.
type CardView struct {
	Index      int      `json:"index"`
	Vendor     string   `json:"vendor"`
	Name       string   `json:"name"`
	MemMiB     int      `json:"mem_mib"`
	Seats      int      `json:"seats"`
	FreeSeats  int      `json:"free_seats"`
	FreeMemMiB int      `json:"free_mem_mib"`
	Caps       gpu.Caps `json:"caps"`
}

// Cards reports every card and its free space.
func (d *Daemon) Cards() []CardView {
	d.mu.Lock()
	defer d.mu.Unlock()
	free := map[int]scheduler.Free{}
	for _, f := range d.sched.Available() {
		free[f.Card] = f
	}
	var out []CardView
	for _, c := range d.sched.Cards() {
		ref := d.byCard[c.Index]
		out = append(out, CardView{Index: c.Index, Vendor: ref.vendor, Name: c.Name, MemMiB: c.MemMiB,
			Seats: c.Seats, FreeSeats: free[c.Index].Seats, FreeMemMiB: free[c.Index].MemMiB,
			Caps: d.backends[ref.vendor].Caps()})
	}
	return out
}

// Usage reports accounting.
func (d *Daemon) Usage(since, until time.Time, by string) ([]accounting.Row, error) {
	return d.ledger.Usage(since, until, by)
}
