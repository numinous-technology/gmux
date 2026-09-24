// Package accounting records who used which share of which card, and when.
//
// Every stretch of a job holding a share is one interval: it opens when the
// job starts, resumes or is resized, and closes when it stops, is suspended or
// is resized again. Intervals are appended to a JSON lines file, one per
// line, so the ledger survives a daemon restart and can be read by anything.
package accounting

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Interval is one stretch of a job holding a share of one card.
type Interval struct {
	Job      string    `json:"job"`
	Name     string    `json:"name,omitempty"`
	Owner    string    `json:"owner,omitempty"`
	Vendor   string    `json:"vendor"`
	Card     int       `json:"card"`
	CardName string    `json:"card_name"`
	Share    float64   `json:"share"` // granted fraction of the card
	MemMiB   int       `json:"mem_mib"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
}

// Seconds is the interval's length.
func (i Interval) Seconds() float64 { return i.End.Sub(i.Start).Seconds() }

// Ledger appends closed intervals and keeps open ones in memory.
type Ledger struct {
	mu   sync.Mutex
	path string
	open map[string][]Interval // job -> open intervals, one per card
}

// Open returns a ledger backed by path, creating it if needed.
func Open(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &Ledger{path: path, open: map[string][]Interval{}}, nil
}

// Begin opens intervals for a job's slots.
func (l *Ledger) Begin(ivs []Interval) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, iv := range ivs {
		l.open[iv.Job] = append(l.open[iv.Job], iv)
	}
}

// End closes every open interval of a job at t and writes them out.
func (l *Ledger) End(job string, t time.Time) error {
	l.mu.Lock()
	ivs := l.open[job]
	delete(l.open, job)
	l.mu.Unlock()
	if len(ivs) == 0 {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, iv := range ivs {
		iv.End = t
		if iv.End.Before(iv.Start) {
			iv.End = iv.Start
		}
		b, _ := json.Marshal(iv)
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

// Row is one line of a usage report.
type Row struct {
	Key          string  `json:"key"`
	Seconds      float64 `json:"seconds"`       // wall time holding any share
	ShareSeconds float64 `json:"share_seconds"` // share times seconds, summed
	GPUHours     float64 `json:"gpu_hours"`     // share seconds as whole-card hours
	MemGiBHours  float64 `json:"mem_gib_hours"`
}

// Usage sums intervals overlapping [since, until) grouped by "job", "owner",
// "card" or "vendor". Open intervals count up to until. Intervals are clipped
// to the window so a report never counts time outside it.
func (l *Ledger) Usage(since, until time.Time, by string) ([]Row, error) {
	if _, err := keyOf(Interval{}, by); err != nil {
		return nil, err
	}
	ivs, err := l.read()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	for _, open := range l.open {
		for _, iv := range open {
			iv.End = until
			ivs = append(ivs, iv)
		}
	}
	l.mu.Unlock()
	rows := map[string]*Row{}
	for _, iv := range ivs {
		s, e := iv.Start, iv.End
		if s.Before(since) {
			s = since
		}
		if e.After(until) {
			e = until
		}
		if !e.After(s) {
			continue
		}
		key, err := keyOf(iv, by)
		if err != nil {
			return nil, err
		}
		r := rows[key]
		if r == nil {
			r = &Row{Key: key}
			rows[key] = r
		}
		sec := e.Sub(s).Seconds()
		r.Seconds += sec
		r.ShareSeconds += sec * iv.Share
		r.GPUHours += sec * iv.Share / 3600
		r.MemGiBHours += sec * float64(iv.MemMiB) / 1024 / 3600
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ShareSeconds > out[j].ShareSeconds })
	return out, nil
}

func keyOf(iv Interval, by string) (string, error) {
	switch by {
	case "", "job":
		if iv.Name != "" {
			return iv.Name, nil
		}
		return iv.Job, nil
	case "owner":
		if iv.Owner == "" {
			return "(none)", nil
		}
		return iv.Owner, nil
	case "card":
		return fmt.Sprintf("%s %d %s", iv.Vendor, iv.Card, iv.CardName), nil
	case "vendor":
		return iv.Vendor, nil
	}
	return "", fmt.Errorf("cannot group usage by %q (use job, owner, card or vendor)", by)
}

func (l *Ledger) read() ([]Interval, error) {
	f, err := os.Open(l.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Interval
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var iv Interval
		if json.Unmarshal(sc.Bytes(), &iv) == nil {
			out = append(out, iv)
		}
	}
	return out, sc.Err()
}
