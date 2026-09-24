package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/numinous-technology/gmux/internal/api"
	"github.com/numinous-technology/gmux/internal/daemon"
	"github.com/numinous-technology/gmux/internal/remote"
	"github.com/numinous-technology/gmux/internal/share"
)

// A machine without a GPU uses GPU hosts it has been told about. They live in
// ~/.config/gmux/remotes.json (or $GMUX_CONFIG) as name -> target, where a
// target is TOKEN@HOST:PORT#FINGERPRINT, exactly what `gmux serve --addr`
// prints for the host.
type hostConfig struct {
	Default string            `json:"default,omitempty"`
	Remotes map[string]string `json:"remotes"`
	path    string
}

func configPath() string {
	if p := os.Getenv("GMUX_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "gmux", "remotes.json")
}

func loadHosts() (*hostConfig, error) {
	h := &hostConfig{Remotes: map[string]string{}, path: configPath()}
	b, err := os.ReadFile(h.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, h); err != nil {
			return nil, fmt.Errorf("%s: %w", h.path, err)
		}
	}
	if h.Remotes == nil {
		h.Remotes = map[string]string{}
	}
	return h, nil
}

func (h *hostConfig) save() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(h, "", "  ")
	return os.WriteFile(h.path, append(b, '\n'), 0o600) // targets hold tokens
}

// names returns the configured hosts, the default first.
func (h *hostConfig) names() []string {
	var out []string
	for n := range h.Remotes {
		if n != h.Default {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if _, ok := h.Remotes[h.Default]; ok {
		out = append([]string{h.Default}, out...)
	}
	return out
}

// target resolves a host name or a literal target.
func (h *hostConfig) target(nameOrTarget string) string {
	if t, ok := h.Remotes[nameOrTarget]; ok {
		return t
	}
	return nameOrTarget
}

func cmdRemote(args []string) error {
	h, err := loadHosts()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		args = []string{"ls"}
	}
	switch args[0] {
	case "add":
		fs := flags(args[1:])
		if len(fs.rest) < 2 && len(args) < 3 {
			return fmt.Errorf("usage: gmux remote add NAME TOKEN@HOST:PORT#FINGERPRINT [--default]")
		}
		name, target := args[1], args[2]
		if !strings.Contains(target, "@") || !strings.Contains(target, "#") {
			return fmt.Errorf("a target is TOKEN@HOST:PORT#FINGERPRINT, as `gmux serve --addr` prints it")
		}
		c, err := remote.Dial(target)
		if err != nil {
			return err
		}
		c.SetTimeout(10 * time.Second)
		cv, err := c.Cards()
		if err != nil {
			return fmt.Errorf("could not reach %s: %w", name, err)
		}
		h.Remotes[name] = target
		if h.Default == "" || fs.bool("default") {
			h.Default = name
		}
		if err := h.save(); err != nil {
			return err
		}
		fmt.Printf("added %s: %s\n", name, summarize(cv))
		return nil
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: gmux remote rm NAME")
		}
		delete(h.Remotes, args[1])
		if h.Default == args[1] {
			h.Default = ""
		}
		return h.save()
	case "default":
		if len(args) < 2 {
			return fmt.Errorf("usage: gmux remote default NAME")
		}
		if _, ok := h.Remotes[args[1]]; !ok {
			return fmt.Errorf("no remote %q", args[1])
		}
		h.Default = args[1]
		return h.save()
	case "ls":
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "\tNAME\tHOST\tGPUS")
		for _, n := range h.names() {
			mark := " "
			if n == h.Default {
				mark = "*"
			}
			host := hostOf(h.Remotes[n])
			state := "unreachable"
			if c, err := remote.Dial(h.Remotes[n]); err == nil {
				c.SetTimeout(5 * time.Second)
				if cv, err := c.Cards(); err == nil {
					state = summarize(cv)
				} else {
					state = "unreachable: " + err.Error()
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, n, host, state)
		}
		return tw.Flush()
	}
	return fmt.Errorf("usage: gmux remote [ls|add|rm|default]")
}

func hostOf(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		target = target[i+1:]
	}
	if i := strings.Index(target, "#"); i >= 0 {
		target = target[:i]
	}
	return target
}

func summarize(cv []daemon.CardView) string {
	var parts []string
	for _, c := range cv {
		parts = append(parts, fmt.Sprintf("%s (%d/%d seats free)", c.Name, c.FreeSeats, c.Seats))
	}
	if len(parts) == 0 {
		return "no GPUs"
	}
	return strings.Join(parts, ", ")
}

// controller is what the job and card commands need, locally or remotely.
type controller interface {
	Jobs() ([]daemon.Job, error)
	Cards() ([]daemon.CardView, error)
	Stop(id string) error
	Resize(id string, share float64, memMiB int) (*daemon.Job, error)
	Usage(since, by string) (map[string]any, error)
}

// localUp reports whether a local gmux daemon is answering.
func localUp() bool {
	c, err := net.DialTimeout("unix", sockPath(), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// explicitRemote returns the remote a command was pointed at, if any.
func explicitRemote(fs *flagset, h *hostConfig) string {
	if r := fs.str("remote", os.Getenv("GMUX_REMOTE")); r != "" {
		return h.target(r)
	}
	return ""
}

// pick chooses where a job or card command goes: --local, then --remote (or
// GMUX_REMOTE), then a local daemon if one answers, then the default remote.
// So on a machine with no GPU every command reaches the GPU host by default.
func pick(fs *flagset) (controller, string, error) {
	if fs.bool("local") {
		return api.Dial(sockPath()), "local", nil
	}
	h, err := loadHosts()
	if err != nil {
		return nil, "", err
	}
	if t := explicitRemote(fs, h); t != "" {
		c, err := remote.Dial(t)
		if err != nil {
			return nil, "", err
		}
		c.SetTimeout(30 * time.Second)
		return c, hostOf(t), nil
	}
	if localUp() {
		return api.Dial(sockPath()), "local", nil
	}
	names := h.names()
	if len(names) == 0 {
		return nil, "", fmt.Errorf("no local gmux here and no GPU host configured: run `gmux serve` on a machine with a GPU, or `gmux remote add` one")
	}
	c, err := remote.Dial(h.Remotes[names[0]])
	if err != nil {
		return nil, "", err
	}
	c.SetTimeout(30 * time.Second)
	return c, names[0], nil
}

// candidate is one host's cards, for placement across hosts.
type candidate struct {
	name  string
	cards []daemon.CardView
	err   error
}

// bestHost picks the host that can take a job of the given share and memory
// with the most seats to spare, preferring earlier hosts on a tie. -1 means no
// reachable host has room right now.
func bestHost(cands []candidate, frac float64, memMiB int) int {
	best, bestFree := -1, -1
	for i, c := range cands {
		if c.err != nil {
			continue
		}
		for _, card := range c.cards {
			need := int(math.Ceil(frac*float64(card.Seats) - 1e-9))
			mem := memMiB
			if mem == 0 {
				mem = share.DefaultMemMiB(need, card.Seats, card.MemMiB)
			}
			if card.FreeSeats >= need && card.FreeMemMiB >= mem {
				if left := card.FreeSeats - need; left > bestFree {
					best, bestFree = i, left
				}
			}
		}
	}
	return best
}

// placeRemote asks every configured host what it has free, in parallel, and
// returns the target that should take the job.
func placeRemote(h *hostConfig, frac float64, memMiB int) (string, string, error) {
	names := h.names()
	cands := make([]candidate, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			cands[i].name = n
			c, err := remote.Dial(h.Remotes[n])
			if err != nil {
				cands[i].err = err
				return
			}
			c.SetTimeout(5 * time.Second)
			cands[i].cards, cands[i].err = c.Cards()
		}(i, n)
	}
	wg.Wait()
	if i := bestHost(cands, frac, memMiB); i >= 0 {
		return names[i], h.Remotes[names[i]], nil
	}
	for i, c := range cands { // nothing free: the first reachable host decides (queue or refuse)
		if c.err == nil {
			return names[i], h.Remotes[names[i]], nil
		}
	}
	return "", "", fmt.Errorf("no configured GPU host is reachable")
}
