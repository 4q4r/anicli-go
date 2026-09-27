//go:build live

package tui

// LIVE probes for PR61 (merged multi-provider playback + aniskip
// fetch logging). Excluded from the hermetic default suite by the
// `live` build tag (the offline-suite ruling: `go test -race ./...`
// never egresses). Run manually:
//
//	go test -tags live -run TestLivePR61 -count=1 -v ./internal/tui/
//
// Optional proxy for foreign networks:
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 go test -tags live ...
//
// Probe 1 exercises the REAL merge model end to end: a fan-out search
// across the whole roster, a 2+ provider episode merge, the PR61
// resolveAllStreams into ONE quality-sorted list, and the exact mpv
// argv for both track modes (muxed single URL / separate audio).
// Probe 2 exercises the REAL aniskip fetch through realPlayback and
// prints the file-logger line plus the status note. Probe 3 proves
// the unauthenticated typed skip with a live wiring.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/player"
	"github.com/an0nx/anicli-go/internal/storage"
)

func liveSettings(t *testing.T) config.Settings {
	t.Helper()
	settings := config.Default()
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		settings.Network.ProxyURL = proxy
	}
	return settings
}

func liveDeps(t *testing.T) (*RealDeps, *Deps) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, t.TempDir()+"/live.db")
	if err != nil {
		t.Fatalf("open live store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	real, err := NewRealDeps(liveSettings(t), store, WithLogger(log))
	if err != nil {
		t.Fatalf("build live deps: %v", err)
	}
	t.Cleanup(real.Close)
	return real, real.Deps
}

// liveSearch fans one query out to every provider, returning the
// results grouped by source id (the session's group shape).
func liveSearch(t *testing.T, deps *Deps, query string) map[string]contracts.SearchResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bySource := map[string]contracts.SearchResult{}
	for _, p := range deps.Search.Providers() {
		results, err := deps.Search.Search(ctx, p.ID, query)
		if err != nil {
			t.Logf("  provider %-14s → err: %v", p.ID, err)
			continue
		}
		if len(results) == 0 {
			t.Logf("  provider %-14s → 0 results", p.ID)
			continue
		}
		t.Logf("  provider %-14s → %d results (first: %q)", p.ID, len(results), results[0].Title)
		if _, ok := bySource[p.ID]; !ok {
			bySource[p.ID] = results[0]
		}
	}
	return bySource
}

// TestLivePR61MergedStreams: the PR61 merge model against LIVE
// providers — episodes from 2+ providers merge into one aggregate and
// resolve into ONE quality-sorted stream list; the mpv argv proves
// both track modes.
func TestLivePR61MergedStreams(t *testing.T) {
	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "магическая битва"
	}
	t.Logf("fan-out search: %q", query)
	_, deps := liveDeps(t)
	bySource := liveSearch(t, deps, query)
	if len(bySource) < 2 {
		t.Fatalf("need results from 2+ providers for the merge proof, got %d: %v", len(bySource), bySource)
	}

	// The checked-group shape the session receives (sorted-first
	// primary, PR61).
	group := []contracts.SearchResult{}
	for _, r := range bySource {
		group = append(group, r)
	}
	group = stableGroup(group)
	t.Logf("checked group (%d providers): primary=%s sources=%v",
		len(group), group[0].SourceID, func() (out []string) {
			for _, r := range group {
				out = append(out, r.SourceID)
			}
			return out
		}())

	// Fetch every source's episodes and merge (python session_loop
	// parity — the merge, not a provider pick).
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	parts := []SourceEpisodes{}
	for _, res := range group {
		eps, err := deps.Episode.GetEpisodes(ctx, res.SourceID, res.URL)
		if err != nil {
			t.Logf("  episodes %-14s → err: %v", res.SourceID, err)
			continue
		}
		t.Logf("  episodes %-14s → %d episodes", res.SourceID, len(eps))
		parts = append(parts, SourceEpisodes{SourceID: res.SourceID, Episodes: eps})
	}
	if len(parts) < 2 {
		t.Fatalf("need episode lists from 2+ providers, got %d", len(parts))
	}
	merged, order := MergeEpisodeLists(parts)
	t.Logf("merged aggregate: %d episodes, first=%q", len(order), order[0])

	// Hydrate the first episode for every contributing provider
	// (the PR43 on-demand model) and print the merged dub keys.
	ep := merged[order[0]]
	for _, part := range strings.Split(ep.RawID, "|") {
		prov, id, found := strings.Cut(part, ":")
		if !found || hasProviderEmbeds(ep.RawEmbeds, prov) {
			continue
		}
		local := contracts.Episode{Num: ep.Num, RawID: id, RawEmbeds: map[string][]string{}}
		out, err := deps.Episode.HydrateDubs(ctx, prov, local)
		if err != nil {
			t.Logf("  hydrate %-12s → err: %v", prov, err)
			continue
		}
		for dub, links := range out.RawEmbeds {
			if len(links) == 0 {
				continue
			}
			if ep.RawEmbeds == nil {
				ep.RawEmbeds = map[string][]string{}
			}
			ep.RawEmbeds["["+prov+"] "+dub] = links
		}
	}
	if len(ep.RawEmbeds) == 0 {
		t.Fatalf("no hydrated embeds for episode %q — cannot prove the merge", ep.Num)
	}
	t.Logf("episode %q embed keys: %v", ep.Num, sortedEmbedKeys(ep.RawEmbeds))

	// THE PR61 PROOF: streams from ALL providers merge into ONE
	// quality-sorted list — no provider gate. PR94: broken providers
	// degrade fail-soft — their skip records print as the honest
	// summary instead of aborting the merge.
	entries, skipped, err := resolveAllStreams(ctx, deps.Episode, ep, "")
	if err != nil {
		t.Fatalf("merged resolve: %v", err)
	}
	if line := skippedSummary(skipped); line != "" {
		t.Logf("SKIP SUMMARY: %s", line)
	} else {
		t.Logf("SKIP SUMMARY: (none — every consulted provider resolved)")
	}
	dubStats := DubStats([]contracts.Episode{ep})
	s := &sessionScreen{deps: deps, dubStats: dubStats}
	t.Logf("MERGED STREAM LIST (%d entries, quality-sorted):", len(entries))
	for i, e := range entries {
		t.Logf("  %2d. %s | url=%s", i, s.streamEntryLabel(e), e.Source.URL)
	}

	// mpv argv, muxed mode (⭐ Same as video — one stream for both).
	best := entries[0]
	muxed := player.BuildArgs(player.Request{
		URL:   best.Source.URL,
		Title: fmt.Sprintf("[%s - %s] LIVE PR61 - %s", best.DubKey, best.DubKey, ep.Num),
	}, player.Options{})
	t.Logf("mpv argv (MUXED, single url): %v", muxed)

	// mpv argv, SEPARATE audio mode (video + --audio-file).
	if len(entries) > 1 {
		second := entries[len(entries)-1]
		separate := player.BuildArgs(player.Request{
			URL:      best.Source.URL,
			AudioURL: second.Source.URL,
			Title:    "LIVE PR61 separate audio",
		}, player.Options{})
		t.Logf("mpv argv (SEPARATE, video=%s audio=%s): %v",
			best.Source.URL, second.Source.URL, separate)
		for _, arg := range separate {
			if strings.HasPrefix(arg, "--audio-file=") {
				t.Logf("  --audio-file plumbing present: %s", arg)
			}
		}
	}
}

// TestLivePR61AniskipLog: every aniskip fetch outcome is logged with
// the status note (success / не найдены / недоступны).
func TestLivePR61AniskipLog(t *testing.T) {
	real, deps := liveDeps(t)
	pb, ok := deps.Playback.(*realPlayback)
	if !ok {
		t.Fatalf("deps.Playback is %T, want *realPlayback", deps.Playback)
	}
	_ = real
	// Shikimori id 21 — the fetch outcome (found, miss or failure)
	// proves the PR61 logging either way.
	path, cleanup, note, err := pb.ResolveSkips(context.Background(), 21, 1)
	t.Logf("verdict: path=%q note=%q err=%v", path, note, err)
	if note == "" {
		t.Fatalf("every fetch must produce a note, got empty")
	}
	cleanup()
}

// TestLivePR61UnauthSkip: without credentials the sync is a typed
// skip — the exact one-line note the owner asked for (a real-account
// E2E needs the owner's credentials).
func TestLivePR61UnauthSkip(t *testing.T) {
	_, deps := liveDeps(t)
	msg := syncWatchProgress(context.Background(), deps, 21, 1)
	t.Logf("verdict: note=%q err=%v", msg.note, msg.err)
	if msg.err != nil {
		t.Fatalf("unauthenticated must be a typed skip, not an error: %v", msg.err)
	}
	if !strings.Contains(msg.note, "нет авторизации") && !strings.Contains(msg.note, "отключён") {
		t.Fatalf("note = %q, want the typed unauth/disabled skip", msg.note)
	}
}
