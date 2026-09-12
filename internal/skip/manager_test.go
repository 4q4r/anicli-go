package skip

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// TestMergeByType pins the smart-merge table (FEATURE_INVENTORY D
// ruling with ML removed): same-type conflicts resolve to the
// higher-priority provider, non-overlapping types merge.
func TestMergeByType(t *testing.T) {
	t.Parallel()

	t.Run("same type overlap within threshold keeps priority", func(t *testing.T) {
		t.Parallel()
		high := []Interval{{SkipType: "op", StartTime: 0, EndTime: 90, EpisodeLength: 1440}}
		low := []Interval{{SkipType: "op", StartTime: 2, EndTime: 88}}
		got, contributors := MergeByType([][]Interval{high, low})
		want := []Interval{{SkipType: "op", StartTime: 0, EndTime: 90, EpisodeLength: 1440}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("merged = %+v, want priority entry %+v", got, want)
		}
		if !reflect.DeepEqual(contributors, []string{0: "p0"}) {
			t.Errorf("contributors = %v, want [p0]", contributors)
		}
	})

	t.Run("same type disjoint still keeps priority", func(t *testing.T) {
		t.Parallel()
		high := []Interval{{SkipType: "op", StartTime: 0, EndTime: 90}}
		low := []Interval{{SkipType: "op", StartTime: 300, EndTime: 390}}
		got, _ := MergeByType([][]Interval{high, low})
		if len(got) != 1 || got[0].StartTime != 0 {
			t.Errorf("merged = %+v, want only the priority op (mirrors API>ML diff>10s authority)", got)
		}
	})

	t.Run("disjoint types merge", func(t *testing.T) {
		t.Parallel()
		api1 := []Interval{{SkipType: "op", StartTime: 0, EndTime: 90, EpisodeLength: 1440}}
		api2 := []Interval{
			{SkipType: "ed", StartTime: 1300, EndTime: 1400},
			{SkipType: "recap", StartTime: 20, EndTime: 60},
		}
		got, contributors := MergeByType([][]Interval{api1, api2})
		if len(got) != 3 {
			t.Fatalf("merged = %+v, want op+ed+recap", got)
		}
		wantTypes := []string{"op", "recap", "ed"} // start-sorted
		for i, want := range wantTypes {
			if got[i].SkipType != want {
				t.Errorf("merged[%d].type = %s, want %s (start order)", i, got[i].SkipType, want)
			}
		}
		if !reflect.DeepEqual(contributors, []string{"p0", "p1"}) {
			t.Errorf("contributors = %v, want [p0 p1]", contributors)
		}
	})

	t.Run("empty lower sets do not contribute", func(t *testing.T) {
		t.Parallel()
		high := []Interval{{SkipType: "op", StartTime: 0, EndTime: 90}}
		got, contributors := MergeByType([][]Interval{high, nil, {}})
		if len(got) != 1 {
			t.Errorf("merged = %+v, want single op", got)
		}
		if !reflect.DeepEqual(contributors, []string{"p0"}) {
			t.Errorf("contributors = %v, want [p0]", contributors)
		}
	})

	t.Run("all empty stays empty", func(t *testing.T) {
		t.Parallel()
		got, contributors := MergeByType([][]Interval{nil, {}})
		if len(got) != 0 {
			t.Errorf("merged = %+v, want empty", got)
		}
		if len(contributors) != 0 {
			t.Errorf("contributors = %v, want empty", contributors)
		}
	})
}

// managerFixture builds a Manager with both API providers pointed at
// one httptest server; intro_skipper is stubbed with controllable
// chapter output.
type managerFixture struct {
	srv *httptest.Server
	m   *Manager
	// introCalls counts intro_skipper subprocess invocations.
	introCalls int
	// introChapters seeds the stubbed ffprobe -show_chapters answer.
	introChapters string
}

func newManagerFixture(t *testing.T, cfg config.Skip, aniskipPayload, animeskipPayload string) *managerFixture {
	t.Helper()

	f := &managerFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql" {
			if animeskipPayload == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(animeskipPayload))
			return
		}
		if aniskipPayload == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(aniskipPayload))
	}))
	t.Cleanup(f.srv.Close)

	cfgNet := config.Default().Network
	cfgNet.ProxyURL = ""
	cfgNet.RequestTimeout = 5 * time.Second
	net, err := netclient.New(cfgNet, netclient.WithProvider("skip-manager-test"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}

	f.m = NewManager(cfg, net,
		WithAniSkip(AniSkipOptions{BaseURL: f.srv.URL}),
		WithAnimeSkip(AnimeSkipOptions{Endpoint: f.srv.URL + "/graphql"}),
	)
	intro := NewIntroSkipperClient(DefaultIntroSkipperOptions())
	f.m.intro = intro
	intro.run = func(_ context.Context, _ string, args []string) (string, string, error) {
		f.introCalls++
		joined := argsDump(args)
		switch {
		case containsStr(joined, "-show_entries"):
			return "1440.0\n", "", nil
		case containsStr(joined, "-show_chapters"):
			if f.introChapters == "" {
				return `{"chapters":[]}`, "", nil
			}
			return f.introChapters, "", nil
		default:
			return "", "", nil
		}
	}
	return f
}

func argsDump(args []string) string {
	return strings.Join(args, " ")
}

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

// introChaptersJSON renders intervals as latin-titled ffprobe chapters.
func introChaptersJSON(intervals ...Interval) string {
	var b strings.Builder
	b.WriteString(`{"chapters":[`)
	for i, iv := range intervals {
		if i > 0 {
			b.WriteString(",")
		}
		title := "Opening"
		if iv.SkipType == "ed" {
			title = "Ending"
		}
		fmt.Fprintf(&b, `{"start_time":%g,"end_time":%g,"tags":{"title":%q}}`, iv.StartTime, iv.EndTime, title)
	}
	b.WriteString(`]}`)
	return b.String()
}

func foundPayload() string {
	return `{"found":true,"results":[
		{"interval":{"start_time":0,"end_time":90},"skip_type":"op","episode_length":1440},
		{"interval":{"start_time":1300,"end_time":1400},"skip_type":"ed","episode_length":1440}
	]}`
}

func emptyFoundPayload() string { return `{"found":false,"results":[]}` }

func graphqlPayload() string {
	return `{"data":{"episodeByMalId":{"timestamps":[
		{"skipType":"RECAP","startTime":20,"endTime":60}
	]}}}`
}

// TestManagerResolveMergesAPIProviders pins: both API providers are
// consulted, complementary types merge (aniskip op + anime_skip ED),
// chapter types are sorted-unique and the provider id marks the merge.
func TestManagerResolveMergesAPIProviders(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip", "intro_skipper"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: true}
	// The anime_skip collector only emits op/ed (neutral inference), so
	// a complementary ED exercises the real cross-provider merge.
	f := newManagerFixture(t, cfg,
		`{"found":true,"results":[{"interval":{"start_time":0,"end_time":90},"skip_type":"op","episode_length":1440}]}`,
		`{"data":{"episodeByMalId":{"timestamps":[{"skipType":"ED","startTime":1300,"endTime":1400}]}}}`)

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.FFMetadata == "" {
		t.Fatal("FFMetadata empty, want merged chapters")
	}
	if !reflect.DeepEqual(bundle.ChapterTypes, []string{"ed", "op"}) {
		t.Errorf("ChapterTypes = %v, want [ed op]", bundle.ChapterTypes)
	}
	if bundle.ProviderID != "merged" {
		t.Errorf("ProviderID = %q, want merged", bundle.ProviderID)
	}
	if !containsAll(bundle.FFMetadata, "Опенинг", "Эндинг") {
		t.Errorf("FFMetadata lacks merged labels:\n%s", bundle.FFMetadata)
	}
	if f.introCalls != 0 {
		t.Errorf("intro_skipper ran %d times, want 0 (API providers answered)", f.introCalls)
	}
}

// TestManagerResolveSingleContributor pins the provider id when only
// one provider contributes.
func TestManagerResolveSingleContributor(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip", "intro_skipper"},
		AnimeSkipEnabled: true}
	f := newManagerFixture(t, cfg, foundPayload(), emptyGraphQLPayload())

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.ProviderID != ProviderAniSkip {
		t.Errorf("ProviderID = %q, want %q", bundle.ProviderID, ProviderAniSkip)
	}
	if !reflect.DeepEqual(bundle.ChapterTypes, []string{"ed", "op"}) {
		t.Errorf("ChapterTypes = %v, want [ed op]", bundle.ChapterTypes)
	}
}

func emptyGraphQLPayload() string { return `{"data":{"episodeByMalId":{"timestamps":[]}}}` }

// TestManagerResolveFallsBackToIntroSkipper pins: empty API answers
// hand off to intro_skipper when a local media file is given.
func TestManagerResolveFallsBackToIntroSkipper(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip", "intro_skipper"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: true}
	f := newManagerFixture(t, cfg, emptyFoundPayload(), emptyGraphQLPayload())
	f.introChapters = introChaptersJSON(Interval{SkipType: "op", StartTime: 0, EndTime: 90})

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{
		ShikimoriID: 21, EpisodeNum: 2, MediaInput: "ep.mp4",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.ProviderID != ProviderIntroSkipper {
		t.Errorf("ProviderID = %q, want %q", bundle.ProviderID, ProviderIntroSkipper)
	}
	if f.introCalls == 0 {
		t.Error("intro_skipper never ran, want fallback invocation")
	}
}

// TestManagerResolveIntroNeedsLocalFile pins: intro_skipper never runs
// for URL media inputs (remote guard) and a missing MediaInput skips
// it entirely.
func TestManagerResolveIntroNeedsLocalFile(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"intro_skipper"},
		AnimeSkipEnabled: false, IntroSkipperEnabled: true}
	f := newManagerFixture(t, cfg, emptyFoundPayload(), "")

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{
		ShikimoriID: 21, EpisodeNum: 2, MediaInput: "https://cdn/ep.m3u8",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.FFMetadata != "" || bundle.ProviderID != "" {
		t.Errorf("bundle = %+v, want empty", bundle)
	}
	if f.introCalls != 0 {
		t.Errorf("intro ran on URL media %d times, want 0", f.introCalls)
	}
}

// TestManagerResolveDisabledProviders pins per-provider toggles: a
// disabled anime_skip is never queried even though it is first in the
// order.
func TestManagerResolveDisabledProviders(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"anime_skip", "aniskip"},
		AnimeSkipEnabled: false}
	f := newManagerFixture(t, cfg, foundPayload(), "")
	// GraphQL endpoint would 500 (empty payload) if queried; aniskip
	// must answer alone.

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.ProviderID != ProviderAniSkip {
		t.Errorf("ProviderID = %q, want aniskip (anime_skip disabled)", bundle.ProviderID)
	}
}

// TestManagerResolveAllProvidersFail pins the loud aggregate: every
// enabled provider erroring surfaces as an error, not a silent empty
// bundle.
func TestManagerResolveAllProvidersFail(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: false}
	f := newManagerFixture(t, cfg, "", "")

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err == nil {
		t.Fatal("Resolve with all providers failing returned nil error, want aggregate error")
	}
	if !containsStr(bundle.Details, "aniskip") || !containsStr(bundle.Details, "anime_skip") {
		t.Errorf("Details = %q, want joined per-provider errors", bundle.Details)
	}
}

// TestManagerResolveMixedCleanEmptyAndFailure pins the all-failed
// semantics: a transport error from one provider plus a clean-empty
// answer from another degrades to an empty no_provider_result bundle,
// not the loud aggregate error (clean-empty counts as completion).
func TestManagerResolveMixedCleanEmptyAndFailure(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: false}
	// aniskip 403s (empty payload); anime_skip answers cleanly empty.
	f := newManagerFixture(t, cfg, "", emptyGraphQLPayload())

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v, want nil (anime_skip completed cleanly)", err)
	}
	if !bundle.Empty() || bundle.ProviderID != "" {
		t.Errorf("bundle = %+v, want empty", bundle)
	}
	if !strings.HasPrefix(bundle.Details, "no_provider_result") {
		t.Errorf("Details = %q, want no_provider_result prefix", bundle.Details)
	}
	if !containsStr(bundle.Details, "aniskip") {
		t.Errorf("Details = %q, want degraded aniskip note", bundle.Details)
	}
}

// TestManagerResolveLateContributorAttributesProvider pins the
// contributor-index mapping: when the first provider answers
// clean-empty and only the second contributes, ProviderID must name
// the contributing (second) provider, not the first set slot.
func TestManagerResolveLateContributorAttributesProvider(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: false}
	f := newManagerFixture(t, cfg, emptyFoundPayload(),
		`{"data":{"episodeByMalId":{"timestamps":[{"skipType":"ED","startTime":1300,"endTime":1400}]}}}`)

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.FFMetadata == "" {
		t.Fatal("FFMetadata empty, want anime_skip ED chapter")
	}
	if bundle.ProviderID != ProviderAnimeSkip {
		t.Errorf("ProviderID = %q, want %q (sole contributor was the second provider)",
			bundle.ProviderID, ProviderAnimeSkip)
	}
	if !reflect.DeepEqual(bundle.ChapterTypes, []string{"ed"}) {
		t.Errorf("ChapterTypes = %v, want [ed]", bundle.ChapterTypes)
	}
}

// TestManagerResolveAllCleanEmpty pins: providers answering cleanly
// with no data yield an empty bundle without error.
func TestManagerResolveAllCleanEmpty(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: false}
	f := newManagerFixture(t, cfg, emptyFoundPayload(), emptyGraphQLPayload())

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.FFMetadata != "" || len(bundle.ChapterTypes) != 0 || bundle.ProviderID != "" {
		t.Errorf("bundle = %+v, want empty", bundle)
	}
	if bundle.Details != "no_provider_result" {
		t.Errorf("Details = %q, want no_provider_result", bundle.Details)
	}
}

// TestManagerActiveOrder pins order normalization: unknown ids drop,
// missing known ids append, casing folds.
func TestManagerActiveOrder(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"Anime_Skip", "bogus"}}
	f := newManagerFixture(t, cfg, emptyFoundPayload(), "")
	got := f.m.ActiveOrder()
	want := []string{"anime_skip", "aniskip", "intro_skipper"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ActiveOrder = %v, want %v", got, want)
	}
}

// TestManagerResolvePartialFailure pins: one provider erroring is
// tolerated when another answers with data.
func TestManagerResolvePartialFailure(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: false}
	f := newManagerFixture(t, cfg, foundPayload(), "")

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{ShikimoriID: 21, EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.ProviderID != ProviderAniSkip {
		t.Errorf("ProviderID = %q, want aniskip despite anime_skip failure", bundle.ProviderID)
	}
	if bundle.Details == "" {
		t.Error("Details empty, want degraded-provider note")
	}
}

// TestManagerResolveNoShikimoriID pins: without a binding the API
// providers are skipped (python `if not mal_id: continue`).
func TestManagerResolveNoShikimoriID(t *testing.T) {
	t.Parallel()

	cfg := config.Skip{ProvidersOrder: []string{"aniskip", "anime_skip", "intro_skipper"},
		AnimeSkipEnabled: true, IntroSkipperEnabled: false}
	f := newManagerFixture(t, cfg, foundPayload(), graphqlPayload())

	bundle, err := f.m.Resolve(context.Background(), ResolveRequest{EpisodeNum: 2})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.FFMetadata != "" {
		t.Errorf("bundle = %+v, want empty without shikimori binding", bundle)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
