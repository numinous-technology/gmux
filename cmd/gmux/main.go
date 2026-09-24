// Command gmux shares one GPU across many jobs, in space and time, and keeps
// the books. It runs anywhere Docker does: no root, no kernel modules.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/numinous-technology/gmux/internal/api"
	"github.com/numinous-technology/gmux/internal/daemon"
	"github.com/numinous-technology/gmux/internal/fence"
	"github.com/numinous-technology/gmux/internal/gpu"
	"github.com/numinous-technology/gmux/internal/remote"
	"github.com/numinous-technology/gmux/internal/session"
	"github.com/numinous-technology/gmux/internal/share"
)

func sockPath() string {
	if v := os.Getenv("GMUX_SOCKET"); v != "" {
		return v
	}
	return filepath.Join(stateDir(), "gmux.sock")
}

func stateDir() string {
	if v := os.Getenv("GMUX_STATE"); v != "" {
		return v
	}
	return "/tmp/gmux"
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	case "ps":
		err = ps()
	case "top":
		err = top()
	case "cards":
		err = cards()
	case "resize":
		err = resize(os.Args[2:])
	case "stop":
		err = stop(os.Args[2:])
	case "usage":
		err = usageCmd(os.Args[2:])
	case "__fence":
		err = fenceChild(os.Args[2:]) // internal: the seccomp child of a fenced run
	case "-h", "--help", "help":
		usage()
	case "version":
		fmt.Println("gmux", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmux: "+err.Error())
		os.Exit(1)
	}
}

var version = "0.1.0"

func usage() {
	fmt.Print(`gmux: share one GPU across many jobs, in space and time.

  gmux serve [--fake SPEC] [--seats N]     start the daemon
         [--addr :7070 --token SECRET]     also accept remote clients
  gmux run --share F [opts] -- CMD...      run a job on a share of a GPU
  gmux ps                                  list jobs
  gmux top                                 cards and what is on them
  gmux cards                               the GPUs and what each can enforce
  gmux resize ID --share F                 change a running job's share
  gmux stop ID                             stop a job
  gmux usage [--since 24h] [--by job]      who used what

run options:
  --share F        fraction of one card, 0..1 (required)
  --mem SIZE       memory cap, like 20G (default: proportional to the share)
  --gpus N         cards needed, all at once (default 1)
  --name NAME      a label for reports
  --priority N     higher wins; can preempt lower --preemptible jobs
  --preemptible    may be pushed off for a higher priority job
  --burst          may use idle compute above its guarantee
  --wait           queue instead of failing when nothing fits
  --allow HOST     network allowlist (repeatable); everything else is refused
  --deny-net       no network at all

run on a remote GPU host (no GPU needed locally):
  --remote T@host:port   sync this directory there, run on its GPU, stream back
  --pull GLOB            after the run, download result files matching GLOB
  --session ID          reuse a named remote workspace (default: from the path)
`)
}

func serve(args []string) error {
	fs := flags(args)
	cfg := daemon.Config{StateDir: stateDir(), SelfPath: self(), Fake: fs.str("fake", os.Getenv("GMUX_FAKE")), Seats: fs.intv("seats", 0)}
	ctx := context.Background()
	d, err := daemon.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	cv := d.Cards()
	fmt.Printf("gmux serving %d card(s) on %s\n", len(cv), sockPath())
	for _, c := range cv {
		fmt.Printf("  card %d  %s %s  %s  caps: %s\n", c.Index, c.Vendor, c.Name, share.FormatMem(c.MemMiB), capsLine(c.Caps))
	}

	// A remote listener lets GPU-less clients run commands on this host's
	// GPUs. It needs a token; sessions live under the state dir.
	addr := fs.str("addr", os.Getenv("GMUX_ADDR"))
	token := fs.str("token", os.Getenv("GMUX_TOKEN"))
	var sess *session.Store
	if addr != "" {
		if token == "" {
			return fmt.Errorf("--addr needs --token so remote clients can authenticate")
		}
		sess, err = session.Open(filepath.Join(stateDir(), "sessions-store"))
		if err != nil {
			return err
		}
	}
	srv := api.New(d, sess, token)
	if addr != "" {
		fmt.Printf("gmux accepting remote clients on %s (bearer token required)\n", addr)
		go func() {
			if e := srv.ServeTCP(addr); e != nil {
				fmt.Fprintln(os.Stderr, "gmux: remote listener stopped: "+e.Error())
			}
		}()
	}
	return srv.Serve(sockPath())
}

func run(args []string) error {
	fs := flags(args)
	cmd := fs.rest
	if len(cmd) == 0 {
		return fmt.Errorf("a command is required after --")
	}
	shareF := fs.float("share", 0)
	if shareF <= 0 {
		return fmt.Errorf("--share is required, like --share 0.25")
	}
	mem := 0
	if v := fs.str("mem", ""); v != "" {
		m, err := share.ParseMem(v)
		if err != nil {
			return err
		}
		mem = m
	}
	if target := fs.str("remote", os.Getenv("GMUX_REMOTE")); target != "" {
		return runRemote(target, fs, cmd, shareF, mem)
	}
	req := daemon.SubmitRequest{
		Command: cmd, Share: shareF, MemMiB: mem, GPUs: fs.intv("gpus", 0),
		Name: fs.str("name", ""), Owner: os.Getenv("USER"), Priority: fs.intv("priority", 0),
		Preemptible: fs.bool("preemptible"), Burst: fs.bool("burst"), Wait: fs.bool("wait"),
		Allow: fs.multiVals("allow"), DenyNet: fs.bool("deny-net"),
	}
	if dir, err := os.Getwd(); err == nil {
		req.Dir = dir
	}
	job, err := api.Dial(sockPath()).Submit(req)
	if err != nil {
		return err
	}
	if job.State == "queued" {
		fmt.Printf("%s queued (nothing free right now; it starts when a share opens up)\n", job.ID)
		return nil
	}
	pl := ""
	if job.Placement != nil {
		var parts []string
		for _, s := range job.Placement.Slots {
			parts = append(parts, fmt.Sprintf("card %d @ %.0f%%", s.Card, s.GrantedShare*100))
		}
		pl = strings.Join(parts, ", ")
	}
	fmt.Printf("%s running on %s\n", job.ID, pl)
	return nil
}

// runRemote syncs the working directory to a gmux GPU host, runs the command
// there on a share, streams the output back, and pulls result files.
func runRemote(target string, fs *flagset, cmd []string, shareF float64, mem int) error {
	c, err := remote.Dial(target)
	if err != nil {
		return err
	}
	dir, _ := os.Getwd()
	sid, err := c.EnsureSession(fs.str("session", sessionFor(dir)))
	if err != nil {
		return err
	}
	nf, up, err := c.Sync(dir, sid)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gmux] synced %d files (%d new) to %s\n", nf, up, target)
	exit, err := c.Exec(sid, remote.ExecRequest{
		Command: cmd, Share: shareF, MemMiB: mem, Name: fs.str("name", ""),
		Allow: fs.multiVals("allow"), DenyNet: fs.bool("deny-net"), Wait: fs.bool("wait"),
	}, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if pull := fs.multiVals("pull"); len(pull) > 0 {
		n, err := c.Pull(sid, pull, dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[gmux] pulled %d file(s)\n", n)
	}
	if exit != 0 {
		os.Exit(exit)
	}
	return nil
}

// sessionFor derives a stable session id from a directory, so repeated runs
// from the same folder reuse the same remote workspace and only sync changes.
func sessionFor(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "ws" + hex.EncodeToString(sum[:])[:12]
}

func ps() error {
	jobs, err := api.Dial(sockPath()).Jobs()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE\tSHARE\tCARDS\tCOMMAND")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n", j.ID, dash(j.Name), state(j), j.Share*100, cardsOf(j), strings.Join(j.Command, " "))
	}
	tw.Flush()
	return nil
}

func top() error {
	c := api.Dial(sockPath())
	cv, err := c.Cards()
	if err != nil {
		return err
	}
	jobs, _ := c.Jobs()
	onCard := map[int][]string{}
	for _, j := range jobs {
		if j.State == "running" && j.Placement != nil {
			for _, s := range j.Placement.Slots {
				onCard[s.Card] = append(onCard[s.Card], fmt.Sprintf("%s(%.0f%%)", dash(j.Name, j.ID), s.GrantedShare*100))
			}
		}
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CARD\tGPU\tSEATS FREE\tMEM FREE\tJOBS")
	for _, c := range cv {
		fmt.Fprintf(tw, "%d\t%s %s\t%d/%d\t%s\t%s\n", c.Index, c.Vendor, c.Name, c.FreeSeats, c.Seats,
			share.FormatMem(c.FreeMemMiB), strings.Join(onCard[c.Index], " "))
	}
	tw.Flush()
	return nil
}

func cards() error {
	cv, err := api.Dial(sockPath()).Cards()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CARD\tVENDOR\tGPU\tMEMORY\tENFORCES")
	for _, c := range cv {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", c.Index, c.Vendor, c.Name, share.FormatMem(c.MemMiB), capsLine(c.Caps))
	}
	tw.Flush()
	return nil
}

func resize(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gmux resize ID --share F [--mem SIZE]")
	}
	id := args[0]
	fs := flags(args[1:])
	mem := 0
	if v := fs.str("mem", ""); v != "" {
		m, err := share.ParseMem(v)
		if err != nil {
			return err
		}
		mem = m
	}
	job, err := api.Dial(sockPath()).Resize(id, fs.float("share", 0), mem)
	if err != nil {
		return err
	}
	fmt.Printf("%s resized to %.0f%%\n", job.ID, job.Share*100)
	return nil
}

func stop(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gmux stop ID")
	}
	if err := api.Dial(sockPath()).Stop(args[0]); err != nil {
		return err
	}
	fmt.Printf("%s stopped\n", args[0])
	return nil
}

func usageCmd(args []string) error {
	fs := flags(args)
	out, err := api.Dial(sockPath()).Usage(fs.str("since", "24h"), fs.str("by", "job"))
	if err != nil {
		return err
	}
	rows, _ := out["rows"].([]any)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.ToUpper(fs.str("by", "job"))+"\tGPU-HOURS\tMEM GIB-HOURS\tHELD")
	for _, r := range rows {
		m, _ := r.(map[string]any)
		fmt.Fprintf(tw, "%v\t%.3f\t%.1f\t%s\n", m["key"], num(m["gpu_hours"]), num(m["mem_gib_hours"]), dur(num(m["seconds"])))
	}
	tw.Flush()
	return nil
}

func fenceChild(args []string) error {
	mode := args[0]
	i := 1
	for i < len(args) && args[i] != "--" {
		i++
	}
	cmd := args[i+1:]
	switch mode {
	case "--deny":
		return fence.RunDenied(cmd, os.Environ())
	case "--allowlist":
		return fence.Fenced(3, cmd, os.Environ()) // fd 3 is the socket to the supervisor
	}
	return fmt.Errorf("unknown fence mode %q", mode)
}

// small helpers ------------------------------------------------------------

func self() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

func capsLine(c gpu.Caps) string {
	var on []string
	if c.Concurrent {
		on = append(on, "concurrent")
	}
	if c.ComputeCap {
		on = append(on, "compute cap")
	}
	if c.MemoryCap {
		on = append(on, "memory cap")
	}
	if c.Checkpoint {
		on = append(on, "checkpoint")
	}
	if len(on) == 0 {
		return "admission only"
	}
	return strings.Join(on, ", ")
}

func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func dur(sec float64) string {
	d := time.Duration(sec) * time.Second
	return d.Truncate(time.Second).String()
}

func dash(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "-"
}

func state(j daemon.Job) string {
	if j.State == "exited" && j.ExitCode != nil {
		return "exited(" + strconv.Itoa(*j.ExitCode) + ")"
	}
	return j.State
}

func cardsOf(j daemon.Job) string {
	if j.Placement == nil {
		return "-"
	}
	var ids []string
	for _, s := range j.Placement.Slots {
		ids = append(ids, strconv.Itoa(s.Card))
	}
	return strings.Join(ids, ",")
}

var _ = runtime.GOOS
