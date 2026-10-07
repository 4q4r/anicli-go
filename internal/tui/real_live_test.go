//go:build live

package tui

// The OWNER-PATH live proof (fix-round 2 of #158): the remembered-dub
// scoped resolve — realEpisode.ResolveStream with the merged-session
// conventions EXACTLY as the session composes them (the "prov:{json}"
// raw id, the "[prov] dub" track key) — against the real animevib.
// The parity smoke and the providers live walk resolve through the
// provider directly with bare names; only this seam reproduces the
// merged-session call the owner's playback rides. Run manually:
//
//	go test ./internal/tui/ -tags live -run TestLiveRealEpisodeResolveStream -v

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/lua"
	"github.com/an0nx/anicli-go/internal/luaproviders"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
)

func liveAnimeVibProvider(t testing.TB) contracts.Provider {
	t.Helper()

	var src string
	for _, s := range luaproviders.Sources() {
		if s.ID == "animevib" {
			src = s.Src
			break
		}
	}
	if src == "" {
		t.Fatalf("no bundled animevib script")
	}
	network := config.Default().Network
	if proxy := os.Getenv("ANICLI_LUA_LIVE_PROXY"); proxy != "" {
		network.ProxyURL = proxy
	}
	cfg := lua.DefaultConfig()
	cfg.Timeout = 120 * time.Second // the fallback batch rides the budget
	client, err := netclient.New(network, netclient.WithProvider("animevib"))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	cfg.HTTP = client
	p, err := lua.LoadProviderBytes(cfg, nil, "animevib", []byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return p.Adapt()
}

// TestLiveRealEpisodeResolveStreamMergedTrackKey walks the owner's
// exact case live: Dandadan (search «дандадан», the Dandadan-1 post),
// episode 3, the remembered dub "[animevib] Amazing Dubbing" (bare
// name live-verified in the translations select and carrying ep3 on
// 2026-10-07). The merged raw id and the tagged dub key must both
// decompose at the realEpisode boundary; the resolve lands playable
// links attributed to the requested dub.
func TestLiveRealEpisodeResolveStreamMergedTrackKey(t *testing.T) {
	p := liveAnimeVibProvider(t)
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &realEpisode{registry: reg}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "дандадан")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var postURL string
	for _, res := range results {
		if strings.Contains(res.URL, "2937-n84b-dandadan-1") {
			postURL = res.URL
			break
		}
	}
	if postURL == "" {
		t.Fatalf("Dandadan-1 post not surfaced (search returned %d results)", len(results))
	}

	eps, err := p.GetEpisodes(ctx, postURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	var ep3 contracts.Episode
	for _, ep := range eps {
		if ep.Num == "3" {
			ep3 = ep
			break
		}
	}
	if ep3.RawID == "" {
		t.Fatalf("episode 3 not listed (%d episodes)", len(eps))
	}
	// The session merge composes BOTH conventions (MergeEpisodeLists):
	// "prov:" + bare raw id, and "[prov] " + bare dub names.
	merged := contracts.Episode{
		Num:       ep3.Num,
		RawID:     "animevib:" + ep3.RawID,
		RawEmbeds: map[string][]string{},
	}
	if _, ok := ep3.RawEmbeds["Amazing Dubbing"]; !ok {
		t.Fatalf("Amazing Dubbing not in the live ep3 dub table (%d dubs)", len(ep3.RawEmbeds))
	}
	merged.RawEmbeds["[animevib] Amazing Dubbing"] = ep3.RawEmbeds["Amazing Dubbing"]

	stream, err := svc.ResolveStream(ctx, "animevib", merged, "[animevib] Amazing Dubbing")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if stream.DubName != "Amazing Dubbing" {
		t.Errorf("DubName = %q, want %q", stream.DubName, "Amazing Dubbing")
	}
	if len(stream.Links) == 0 {
		t.Fatal("zero links — the resolve did not land playable entries")
	}
	for quality, src := range stream.Links {
		t.Logf("resolved %q via [animevib] Amazing Dubbing: %s → %s (%s)", stream.DubName, quality, src.URL, src.Type)
		break
	}
}

// TestLiveRealEpisodeResolveStreamDubMissTypesCarriers proves the
// corrected miss semantics live (fix-round 3 — the round-2 silent
// first-dub swap was rejected): a dub absent from the episode's table
// walls typed ErrNotFound whose message LISTS the dubs the episode
// actually carries (verified per translation page) — the actionable
// ask. Never a silent substitution, never an unattributed wall. The
// TUI menu over that list is pinned offline
// (TestScopedResolveDubMissWarnsAndOpensDubMenu).
func TestLiveRealEpisodeResolveStreamDubMissTypesCarriers(t *testing.T) {
	p := liveAnimeVibProvider(t)
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &realEpisode{registry: reg}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "дандадан")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var postURL string
	for _, res := range results {
		if strings.Contains(res.URL, "2937-n84b-dandadan-1") {
			postURL = res.URL
			break
		}
	}
	if postURL == "" {
		t.Fatalf("Dandadan-1 post not surfaced")
	}
	eps, err := p.GetEpisodes(ctx, postURL)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	var ep3 contracts.Episode
	for _, ep := range eps {
		if ep.Num == "3" {
			ep3 = ep
			break
		}
	}
	if ep3.RawID == "" {
		t.Fatalf("episode 3 not listed")
	}
	merged := contracts.Episode{
		Num:       ep3.Num,
		RawID:     "animevib:" + ep3.RawID,
		RawEmbeds: map[string][]string{},
	}

	_, err = svc.ResolveStream(ctx, "animevib", merged, "[animevib] NoSuchDub Team")
	if !errors.Is(err, contracts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (never a silent substitution)", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `carries no dub "NoSuchDub Team"`) {
		t.Errorf("message = %q, want the requested dub named", msg)
	}
	if !strings.Contains(msg, "episode dubs:") {
		t.Errorf("message = %q, want the actionable carrier list", msg)
	}
	// The list is the episode's TRUTH: the live-verified carrier of
	// ep3 (the embed dub JAM) must be among the listed dubs.
	if !strings.Contains(msg, "JAM") {
		t.Errorf("message = %q, want the live-verified carrier JAM listed", msg)
	}
	t.Logf("typed carrier list: %s", msg[strings.Index(msg, "(episode dubs:"):])
}
