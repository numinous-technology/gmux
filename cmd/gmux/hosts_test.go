package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/numinous-technology/gmux/internal/daemon"
)

func card(free, seats, freeMem, mem int) daemon.CardView {
	return daemon.CardView{Seats: seats, FreeSeats: free, MemMiB: mem, FreeMemMiB: freeMem}
}

func TestBestHostPicksTheMostRoomAndSkipsTheUnreachable(t *testing.T) {
	cands := []candidate{
		{name: "down", err: errors.New("unreachable")},
		{name: "busy", cards: []daemon.CardView{card(2, 8, 20000, 80000)}},
		{name: "roomy", cards: []daemon.CardView{card(8, 8, 80000, 80000)}},
	}
	if i := bestHost(cands, 0.25, 0); cands[i].name != "roomy" {
		t.Fatalf("picked %s, want roomy", cands[i].name)
	}
	// a half share fits only on roomy; nothing fits a job needing 2 cards' worth of memory
	if i := bestHost(cands, 0.5, 0); cands[i].name != "roomy" {
		t.Fatalf("half share went to %s", cands[i].name)
	}
	if i := bestHost(cands, 0.25, 90000); i != -1 {
		t.Fatal("a job that fits nowhere should get -1")
	}
	// a tie goes to the earlier (default) host
	tie := []candidate{{name: "first", cards: []daemon.CardView{card(8, 8, 80000, 80000)}}, {name: "second", cards: []daemon.CardView{card(8, 8, 80000, 80000)}}}
	if i := bestHost(tie, 0.25, 0); tie[i].name != "first" {
		t.Fatal("a tie should go to the default host")
	}
}

func TestHostConfigRoundTrip(t *testing.T) {
	t.Setenv("GMUX_CONFIG", filepath.Join(t.TempDir(), "remotes.json"))
	h, err := loadHosts()
	if err != nil {
		t.Fatal(err)
	}
	h.Remotes["b"] = "tok@b:7070#ff"
	h.Remotes["a"] = "tok@a:7070#ee"
	h.Default = "b"
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	h2, _ := loadHosts()
	if got := h2.names(); len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Fatalf("names = %v, want the default first", got)
	}
	if h2.target("a") != "tok@a:7070#ee" || h2.target("x@y:1#z") != "x@y:1#z" {
		t.Fatal("target resolves names and passes literal targets through")
	}
	if hostOf("tok@gpu.example:7070#ab") != "gpu.example:7070" {
		t.Fatal("hostOf")
	}
}
