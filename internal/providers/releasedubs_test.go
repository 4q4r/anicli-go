package providers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// PR44 owner rework: the release's dub-PROVIDER list is fetched ONCE
// (the first episode's detail) and applied to every episode of the
// release as keys with EMPTY link lists; heavy per-episode hydration
// happens only when a stream is actually resolved (ResolveStream
// self-hydrates the opened episode).

// TestAnilibGetEpisodesAppliesReleaseDubList: one extra request (the
// first episode's players) gives the whole release's dub list; episode
// one keeps its real links, the rest carry the keys with empty lists.
func TestAnilibGetEpisodesAppliesReleaseDubList(t *testing.T) {
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/episodes/"):
			_, _ = w.Write(fixture(t, "anilib_episode_players.json"))
		default:
			_, _ = w.Write(fixture(t, "anilib_episodes.json"))
		}
	})
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	episodes, err := p.GetEpisodes(context.Background(), "16488--bleach-sennen-kessen-hen")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 4 {
		t.Fatalf("episodes = %d, want 4", len(episodes))
	}

	// The first (post-sort) episode keeps its REAL embeds.
	first := episodes[0]
	if len(first.RawEmbeds["AniLib (AnimeLib)"]) == 0 {
		t.Fatalf("first episode embeds = %v, want real links", first.RawEmbeds)
	}
	// Every other episode carries the release's dub keys with EMPTY
	// link lists (the providers are known; the streams are not).
	wantKeys := []string{"AniLib (AnimeLib)", "Studio Band (Kodik)"}
	for _, ep := range episodes[1:] {
		if len(ep.RawEmbeds) != len(wantKeys) {
			t.Fatalf("episode %s embeds = %v, want the %v key set", ep.Num, ep.RawEmbeds, wantKeys)
		}
		for _, k := range wantKeys {
			if links := ep.RawEmbeds[k]; links == nil || len(links) != 0 {
				t.Fatalf("episode %s dub %q = %v, want an EMPTY list (streams resolve on demand)", ep.Num, k, links)
			}
		}
	}
}

// TestAnilibResolveStreamSelfHydrates: resolving an episode whose dub
// links were never fetched hydrates that ONE episode internally, then
// resolves — no bulk, and the caller needs no pre-pass.
func TestAnilibResolveStreamSelfHydrates(t *testing.T) {
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "anilib_episode_players.json"))
	})
	p := newAnilib(srv.URL, testClient(t, "anilib"))

	episode := contracts.Episode{
		Num:   "1",
		RawID: "11",
		RawEmbeds: map[string][]string{
			"AniLib (AnimeLib)":   {},
			"Studio Band (Kodik)": {},
		},
	}
	stream, err := p.ResolveStream(context.Background(), episode, "AniLib (AnimeLib)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if rec.Path != "/episodes/11" {
		t.Errorf("request = %q, want the on-demand /episodes/11 hydration", rec.Path)
	}
	if len(stream.Links) == 0 {
		t.Fatal("self-hydration must produce playable links")
	}
	if sd, ok := stream.Links["720"]; !ok || !strings.Contains(sd.URL, "video1.cdnlibs.org") {
		t.Fatalf("720 link = %+v, want the internal CDN URL", sd)
	}
}

// TestAnimegoGetEpisodesAppliesReleaseDubList: the same tier over the
// /player/{id} fragment — the provider buttons ride the SAME response
// as the episode carousel, so the release's dub list costs zero extra
// requests.
func TestAnimegoGetEpisodesAppliesReleaseDubList(t *testing.T) {
	srv, _ := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/player/"):
			_, _ = w.Write(fixture(t, "animego_player_series.json"))
		default:
			_, _ = w.Write(fixture(t, "animego_anime.html"))
		}
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/anime/piraty-chernoy-laguny-2115")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 2 {
		t.Fatalf("episodes = %d, want 2", len(episodes))
	}
	// Episode 1 keeps its real player links; episode 2 carries the
	// same dub keys with empty lists.
	first := episodes[0]
	if len(first.RawEmbeds["MC Entertainment"]) == 0 {
		t.Fatalf("first episode embeds = %v, want real links", first.RawEmbeds)
	}
	second := episodes[1]
	if links := second.RawEmbeds["MC Entertainment"]; links == nil || len(links) != 0 {
		t.Fatalf("episode 2 dub %q = %v, want an EMPTY list", "MC Entertainment", links)
	}
	if len(second.RawEmbeds) != 1 {
		t.Fatalf("episode 2 embeds = %v, want exactly the release dub keys", second.RawEmbeds)
	}
}

// TestAnimegoResolveStreamSelfHydrates: resolving episode 2's style
// empty-key dub hydrates it via /player/videos/{id} on demand, then the
// direct-URL embed resolves through the factory fallback (no extractor
// network hop).
func TestAnimegoResolveStreamSelfHydrates(t *testing.T) {
	srv, rec := fixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A minimal /player/videos fragment: one provider button with a
		// direct .m3u8 player URL tagged with the release dub key.
		_, _ = w.Write([]byte(`{"status":"success","message":null,"data":{"content":"<button data-anime-player-target=\"provider\" data-player=//cdn.example.com/static/ep.m3u8 data-translation-title=\"MC Entertainment\"></button>"}}`))
	})
	p := newAnimego(srv.URL, testClient(t, "animego"))

	episode := contracts.Episode{
		Num:   "2",
		RawID: "27784",
		RawEmbeds: map[string][]string{
			"MC Entertainment": {},
		},
	}
	stream, err := p.ResolveStream(context.Background(), episode, "MC Entertainment")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if rec.Path != "/player/videos/27784" {
		t.Errorf("request = %q, want the on-demand /player/videos/27784 hydration", rec.Path)
	}
	if len(stream.Links) == 0 {
		t.Fatal("self-hydration must produce playable links")
	}
}
