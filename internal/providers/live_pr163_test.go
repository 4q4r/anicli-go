//go:build live

package providers

// PR163 live reproduction: the owner-reported yummy playback failure
// («Реинкарнация безработного» S2 ep 7, Озвучка StudioBand → mpv
// "exit status 2" after 5 attempts). The probe resolves the owner's
// exact case through the real provider chain and prints everything
// needed to classify the failure at the mpv boundary: the resolved
// URLs, the playback headers, and the exact mpv argv production would
// build. The manual headless mpv run happens outside (shell), so the
// probe stays read-only.
//
//	go test ./internal/providers/ -tags live -run TestLivePR163YummyResolve -v

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/player"
)

func TestLivePR163YummyResolve(t *testing.T) {
	p := liveProvider(t, "yummy")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "Реинкарнация безработного")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("search: 0 results")
	}
	t.Logf("search: %d results, first=%q url=%q", len(results), results[0].Title, results[0].URL)

	eps, err := p.GetEpisodes(ctx, results[0].URL)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}
	t.Logf("episodes: %d", len(eps))

	idx := -1
	for i := range eps {
		if eps[i].Num == "7" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("episode 7 not found among %d", len(eps))
	}
	for dub, links := range eps[idx].RawEmbeds {
		t.Logf("  dub: %q (%d links)", dub, len(links))
	}

	stream, err := p.ResolveStream(ctx, eps[idx], "Озвучка StudioBand")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	t.Logf("resolved dub_name=%q links=%d", stream.DubName, len(stream.Links))

	keys := make([]string, 0, len(stream.Links))
	for q := range stream.Links {
		keys = append(keys, q)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a > b
	})
	for _, q := range keys {
		src := stream.Links[q]
		t.Logf("  [%s] type=%s url=%s headers=%v extra=%v", q, src.Type, src.URL, src.Headers, src.ExtraMPVOpts)
	}
	if len(keys) == 0 {
		t.Fatalf("no resolved URLs")
	}

	best := stream.Links[keys[0]]
	// The exact production argv (the TUI adds Title/Chapters, neither
	// can break the file-open; the network class is what this probe
	// isolates).
	req := player.Request{URL: best.URL, Headers: best.Headers}
	t.Logf("PRODUCTION ARGV: %v", player.BuildArgs(req, player.Options{Bin: "/usr/bin/mpv"}))
}

// TestLivePR163YummyPlayback plays the owner's exact case (fresh
// resolve → the StudioBand best-quality link) through the REAL
// production Player, headless (--vo=null --ao=null --frames=100 are
// the only extras; the argv family is production BuildArgs). Play
// must return nil.
func TestLivePR163YummyPlayback(t *testing.T) {
	p := liveProvider(t, "yummy")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "Реинкарнация безработного")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("search: 0 results")
	}
	eps, err := p.GetEpisodes(ctx, results[0].URL)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}
	idx := -1
	for i := range eps {
		if eps[i].Num == "7" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("episode 7 not found among %d", len(eps))
	}
	stream, err := p.ResolveStream(ctx, eps[idx], "Озвучка StudioBand")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(stream.Links) == 0 {
		t.Fatalf("no resolved links")
	}
	keys := make([]string, 0, len(stream.Links))
	for q := range stream.Links {
		keys = append(keys, q)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(keys[i])
		b, _ := strconv.Atoi(keys[j])
		return a > b
	})
	best := stream.Links[keys[0]]
	t.Logf("playing [%s] %s", keys[0], best.URL)

	lines := make(chan string, 64)
	pl := player.New(player.Options{Bin: "/usr/bin/mpv"})
	pl.SetLog(func(line string) {
		t.Log("mpv: " + line)
		select {
		case lines <- line:
		default:
		}
	})
	playCtx, playCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer playCancel()
	err = pl.Play(playCtx, player.Request{
		URL:          best.URL,
		Headers:      best.Headers,
		ExtraMPVOpts: []string{"--vo=null", "--ao=null", "--frames=100"},
	})
	if err != nil {
		t.Fatalf("production Play failed: %v", err)
	}
	t.Log("PRODUCTION PLAY: nil (mpv exited 0)")
}

// TestLivePR163ErrorTailDemo shows the PR163 error improvement on a
// REAL mpv failure: a dead URL must surface mpv's own "Failed to
// open" line instead of a bare "exit status 2".
func TestLivePR163ErrorTailDemo(t *testing.T) {
	pl := player.New(player.Options{Bin: "/usr/bin/mpv", MaxRetries: 2})
	pl.SetLog(func(line string) { t.Log("mpv: " + line) })
	err := pl.Play(context.Background(), player.Request{
		URL: "https://invalid.example.nonexistent/video.mp4",
	})
	if err == nil {
		t.Fatal("dead URL must fail")
	}
	t.Logf("IMPROVED ERROR: %v", err)
	if !strings.Contains(err.Error(), "Failed to open") {
		t.Errorf("error must carry mpv's stderr tail, got %q", err)
	}
}
