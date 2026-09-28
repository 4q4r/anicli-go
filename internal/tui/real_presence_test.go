package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/discord"
	"github.com/an0nx/anicli-go/internal/player"
)

// fakePresence records the PR115 presence lifecycle in order.
type fakePresence struct {
	mu     sync.Mutex
	events []string
	sets   [][2]string // (title, episode)
}

func (f *fakePresence) SetActivity(title, episode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "set")
	f.sets = append(f.sets, [2]string{title, episode})
}

func (f *fakePresence) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "clear")
}

func (f *fakePresence) log(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "mpv:"+line)
}

func (f *fakePresence) snapshot() ([]string, [][2]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	copy(out, f.events)
	sets := make([][2]string, len(f.sets))
	copy(sets, f.sets)
	return out, sets
}

// stubPlayerBin writes a stub mpv: it survives the warmup window,
// prints one line and exits cleanly.
func stubPlayerBin(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeplayer")
	script := "#!/bin/sh\necho stub player running\nsleep 0.2\n"
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write stub player: %v", err)
	}
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // the stub player must be executable; lives in t.TempDir()
		t.Fatalf("chmod stub player executable: %v", err)
	}
	return path
}

// TestRealPlaybackAnnouncesPresence: the mpv launch announces the
// watched title to the presence sink BEFORE the player runs and
// clears it after the exit (PR115 wiring contract).
func TestRealPlaybackAnnouncesPresence(t *testing.T) {
	pres := &fakePresence{}
	p := player.New(player.Options{
		Bin: stubPlayerBin(t), Warmup: 50 * time.Millisecond,
		MaxRetries: 1, RetryDelay: 10 * time.Millisecond,
	})
	p.SetLog(func(line string) { pres.log(line) })
	pb := &realPlayback{player: p, presence: pres}

	err := pb.Play(context.Background(), PlayRequest{
		URL:        "http://example.invalid/v.mp4",
		Title:      "[Дубль 1 - Дубль 1] Наруто - 5",
		AnimeTitle: "Наруто",
		EpisodeNum: "5",
	})
	if err != nil {
		t.Fatalf("play: %v", err)
	}

	events, sets := pres.snapshot()
	if len(sets) != 1 || sets[0] != [2]string{"Наруто", "5"} {
		t.Fatalf("presence sets = %v, want one (Наруто, 5)", sets)
	}
	if len(events) < 3 {
		t.Fatalf("lifecycle events = %v, want set → mpv… → clear", events)
	}
	if events[0] != "set" {
		t.Fatalf("first presence event = %q, want the announce before mpv runs", events[0])
	}
	if events[len(events)-1] != "clear" {
		t.Fatalf("last presence event = %q, want clear after mpv exits", events[len(events)-1])
	}
	if !strings.Contains(strings.Join(events, "|"), "mpv:") {
		t.Fatalf("no mpv log line captured — ordering proof missing: %v", events)
	}
}

// TestRealPlaybackClearsPresenceOnPlayError: a failed launch must
// still clear the announced presence (deferred clear).
func TestRealPlaybackClearsPresenceOnPlayError(t *testing.T) {
	pres := &fakePresence{}
	p := player.New(player.Options{
		Bin:    filepath.Join(t.TempDir(), "definitely-missing-mpv"),
		Warmup: 10 * time.Millisecond, MaxRetries: 1, RetryDelay: time.Millisecond,
	})
	pb := &realPlayback{player: p, presence: pres}

	if err := pb.Play(context.Background(), PlayRequest{URL: "u", AnimeTitle: "Тайтл", EpisodeNum: "1"}); err == nil {
		t.Fatal("play with a missing binary must fail")
	}
	events, _ := pres.snapshot()
	if len(events) != 2 || events[0] != "set" || events[1] != "clear" {
		t.Fatalf("presence events on failed launch = %v, want [set clear]", events)
	}
}

// TestRealPlaybackNilPresence: a hand-built playback without a
// presence sink (legacy tests) keeps working.
func TestRealPlaybackNilPresence(t *testing.T) {
	p := player.New(player.Options{
		Bin:    filepath.Join(t.TempDir(), "missing"),
		Warmup: 10 * time.Millisecond, MaxRetries: 1, RetryDelay: time.Millisecond,
	})
	pb := &realPlayback{player: p}
	if err := pb.Play(context.Background(), PlayRequest{URL: "u"}); err == nil {
		t.Fatal("expected failure with missing binary")
	}
}

// TestNoopPresenceWiring: the disabled discord.New client is a valid
// sink — playback runs without panics (the opt-in contract).
func TestNoopPresenceWiring(t *testing.T) {
	p := player.New(player.Options{
		Bin: stubPlayerBin(t), Warmup: 50 * time.Millisecond,
		MaxRetries: 1, RetryDelay: 10 * time.Millisecond,
	})
	pb := &realPlayback{player: p, presence: discord.New(discord.Options{Enabled: false}, nil)}
	if err := pb.Play(context.Background(), PlayRequest{URL: "u", AnimeTitle: "Т", EpisodeNum: "1"}); err != nil {
		t.Fatalf("play with noop presence: %v", err)
	}
}

// TestSessionDoPlayCarriesPresenceParts: the streaming launch hands
// the clean anime title and episode to the playback service (the
// presence payload source, PR115).
func TestSessionDoPlayCarriesPresenceParts(t *testing.T) {
	deps := &Deps{Episode: &fakeEpisode{episodes: testEpisodeSet()}, Playback: &fakePlayback{}}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub = "Дубль 1"
	s.pickedVideo = contracts.VideoSource{URL: "http://example.invalid/v.mp4"}

	if msg := s.doPlay(); msg == nil {
		t.Fatal("doPlay returned nil")
	}
	pb := deps.Playback.(*fakePlayback)
	if len(pb.played) != 1 {
		t.Fatalf("played %d requests, want 1", len(pb.played))
	}
	req := pb.played[0]
	if req.AnimeTitle != "Тайтл" {
		t.Fatalf("AnimeTitle = %q, want the group display title", req.AnimeTitle)
	}
	if req.EpisodeNum != "1" {
		t.Fatalf("EpisodeNum = %q, want the current episode", req.EpisodeNum)
	}
}

// TestOfflinePlayLocalCarriesPresenceParts: the offline launch hands
// the directory title and picked episode to the playback service.
func TestOfflinePlayLocalCarriesPresenceParts(t *testing.T) {
	pb := &fakePlayback{}
	s := &offlineSession{
		deps:    &Deps{Playback: pb},
		title:   OfflineTitle{Name: "Наруто"},
		current: "2",
	}
	_, cmd := s.playLocal(offlinePlayMsg{path: "/tmp/n.mkv", title: "[OFFLINE] Наруто - 2"})
	if cmd != nil {
		_ = cmd()
	}
	if len(pb.played) != 1 {
		t.Fatalf("played %d requests, want 1", len(pb.played))
	}
	req := pb.played[0]
	if req.AnimeTitle != "Наруто" {
		t.Fatalf("AnimeTitle = %q, want the offline title name", req.AnimeTitle)
	}
	if req.EpisodeNum != "2" {
		t.Fatalf("EpisodeNum = %q, want the picked episode", req.EpisodeNum)
	}
}
