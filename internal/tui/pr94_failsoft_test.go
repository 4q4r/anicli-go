package tui

// PR94 fail-soft resolve: per-provider error isolation of the merged
// stream resolve with honest attribution. A failed provider's entries
// are absent from the merged list, its error is collected; a blocking
// typed error fires ONLY when zero entries resolved across ALL
// providers — naming every failed provider and its reason.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// failsoftEpisode overrides fakeEpisode.ResolveStream with per-dub-key
// resolve errors — the PR94 harness for provider-level failures (the
// embedded fake keeps serving episodes, languages and hydration).
type failsoftEpisode struct {
	*fakeEpisode
	resolveErrs map[string]error // dub key → resolve failure
}

func (f *failsoftEpisode) ResolveStream(ctx context.Context, prov string, ep contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if err, ok := f.resolveErrs[dubID]; ok {
		return contracts.MediaStream{}, err
	}
	return f.fakeEpisode.ResolveStream(ctx, prov, ep, dubID)
}

var _ EpisodeService = (*failsoftEpisode)(nil)

// pr94Embeds builds a two-provider episode: both dubs carry embeds.
func pr94Embeds() contracts.Episode {
	return contracts.Episode{Num: "1", RawEmbeds: map[string][]string{
		"[animego] Дубль 1": {"e1"},
		"[anilib] AniLib":   {"e2"},
	}}
}

// TestPR94FailSoftOneProviderBroken: one provider's resolve failure
// must not abort the merged list — the healthy provider's entries
// surface, the failure is collected (PR94 defect A/C).
func TestPR94FailSoftOneProviderBroken(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {Links: map[string]contracts.VideoSource{"720": {URL: "u-720"}}},
			},
		},
		resolveErrs: map[string]error{
			"[animego] Дубль 1": contracts.WrapProvider("animego", contracts.OpResolveStream, 400, nil),
		},
	}
	entries, skipped, err := resolveAllStreams(context.Background(), eps, pr94Embeds(), "", nil)
	if err != nil {
		t.Fatalf("one broken provider must not fail the merged resolve, got %v", err)
	}
	if len(entries) != 1 || entries[0].DubKey != "[anilib] AniLib" || entries[0].Quality != "720" {
		t.Fatalf("healthy provider entries must surface, got %+v", entries)
	}
	if len(skipped) != 1 || skipped[0].Provider != "animego" || skipped[0].Reason != "HTTP 400" {
		t.Fatalf("skipped = %+v, want [{animego HTTP 400}]", skipped)
	}
}

// TestPR161SkipSummaryCompactOneLine: the owner report (#161) — the
// picker's skip attribution printed the FULL provider error (embed
// URLs, Lua chunk paths, line numbers, multi-line stack tracebacks)
// below the stream picker. The summary must render the owner's exact
// compact format — one line, per-provider class reason, provider id
// always, nothing path- or traceback-shaped in it:
//
//	пропущены: animiku (provider timeout)
//
// The full error chain must still exist — it rides the log sink at
// the resolve seam (TestPR161FullErrorReachesLogSink).
func TestPR161SkipSummaryCompactOneLine(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	// The owner's verbatim timeout chain shape: the Lua classify
	// wraps the sentinel with the raw VM message — script path, line
	// number and the GopherLua stack traceback included.
	animikuTimeout := fmt.Errorf(`provider %q %s: %w: %s`,
		"animiku", contracts.OpResolveStream, contracts.ErrProviderTimeout,
		`providers/animiku/main.lua:293: extract: extractor:kodik: context deadline exceeded: Post "https://kodikplayer.com/fto…": 
stack traceback:
    [G]: in function 'extract'
    providers/animiku/main.lua:293 in main chunk
    [G]: ?`)
	// The typed extract wall of the same report: a zero-status
	// ProviderError wrapping the sentinel plus the verbose script
	// message.
	yummyExtract := contracts.WrapProvider("yummy", contracts.OpResolveStream, 0,
		fmt.Errorf("%w: %s", contracts.ErrExtractFailed,
			"providers/yummy/main.lua:354: context deadline exceeded\nstack traceback:\n    [G]: in function 'extract'\n    providers/yummy/main.lua:354 in main chunk\n    [G]: ?"))

	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {Links: map[string]contracts.VideoSource{"720": {URL: "u-720"}}},
			},
		},
		resolveErrs: map[string]error{
			"[animiku] Дубль 1": animikuTimeout,
			"[yummy] AniLib":    yummyExtract,
		},
	}
	ep := contracts.Episode{Num: "1", RawEmbeds: map[string][]string{
		"[animiku] Дубль 1": {"e1"},
		"[anilib] AniLib":   {"e2"},
		"[yummy] AniLib":    {"e3"},
	}}

	entries, skipped, err := resolveAllStreams(context.Background(), eps, ep, "", nil)
	if err != nil {
		t.Fatalf("fail-soft must keep the healthy provider's resolve alive, got %v", err)
	}
	if len(entries) != 1 || entries[0].DubKey != "[anilib] AniLib" {
		t.Fatalf("entries = %+v, want the healthy anilib stream only", entries)
	}
	if len(skipped) != 2 || skipped[0].Provider != "animiku" || skipped[1].Provider != "yummy" {
		t.Fatalf("skipped = %+v, want [animiku yummy] in consulted order", skipped)
	}

	// The owner's authoritative ru screen line, one line, compact.
	wantRU := "пропущены: animiku (provider timeout), yummy (extract error)"
	if got := skippedSummary(skipped); got != wantRU {
		t.Fatalf("ru summary = %q, want %q", got, wantRU)
	}
	for _, banned := range []string{"\n", ".lua", "stack traceback", "providers/", "kodikplayer", "deadline"} {
		if strings.Contains(skippedSummary(skipped), banned) {
			t.Fatalf("ru summary leaks %q: %q", banned, skippedSummary(skipped))
		}
	}

	// The en table carries the same composition (skipped: {list}).
	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	wantEN := "skipped: animiku (provider timeout), yummy (extract error)"
	if got := skippedSummary(skipped); got != wantEN {
		t.Fatalf("en summary = %q, want %q", got, wantEN)
	}
}

// TestPR161FullErrorReachesLogSink: debugging fidelity (#161) — the
// compact screen line must not cost the full chain. The complete
// provider error (chunk paths, line numbers, stack traceback) lands
// in the wired slog sink at the same seam: fail loud in logs,
// compact in the UI.
func TestPR161FullErrorReachesLogSink(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	animikuTimeout := fmt.Errorf(`provider %q %s: %w: %s`,
		"animiku", contracts.OpResolveStream, contracts.ErrProviderTimeout,
		`providers/animiku/main.lua:293: extract: extractor:kodik: context deadline exceeded: Post "https://kodikplayer.com/fto…": 
stack traceback:
    [G]: in function 'extract'
    providers/animiku/main.lua:293 in main chunk
    [G]: ?`)
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{episodes: testEpisodeSet()},
		resolveErrs: map[string]error{
			"[animiku] Дубль 1": animikuTimeout,
		},
	}
	ep := contracts.Episode{Num: "1", RawEmbeds: map[string][]string{
		"[animiku] Дубль 1": {"e1"},
	}}

	_, skipped, err := resolveAllStreams(context.Background(), eps, ep, "", log)
	if err == nil {
		t.Fatal("all providers broken must still fail the merged resolve")
	}
	if len(skipped) != 1 || skipped[0].Provider != "animiku" || skipped[0].Reason != "provider timeout" {
		t.Fatalf("skipped = %+v, want [{animiku provider timeout}]", skipped)
	}
	logged := buf.String()
	if logged == "" {
		t.Fatal("the full provider error never reached the log sink")
	}
	for _, want := range []string{"animiku", "provider timeout", "resolve_stream", "stack traceback", "kodikplayer"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log sink missing %q; logged:\n%s", want, logged)
		}
	}
}

// TestCompactReasonTotality: characterization pins for the two
// guarantees the #161 classifier leans on — an UNCLASSIFIED error
// (raw VM failure: no sentinel, no status) summarizes to the
// path-free fallback label, never the raw chain, and an untyped
// context deadline summarizes to the timeout class like its typed
// sibling. Both branches were built test-first via the #161 pins;
// these lock the residual behavior.
func TestCompactReasonTotality(t *testing.T) {
	rawVM := fmt.Errorf(`provider "animiku" resolve_stream: providers/animiku/main.lua:293: attempt to index a nil value
stack traceback:
    [G]: ?`)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"raw vm fallback is path-free", rawVM, "resolve failed"},
		{"untyped deadline is a timeout", context.DeadlineExceeded, "provider timeout"},
		{"timeout wraps the class", fmt.Errorf("provider %q %s: %w", "kodik", "request", contracts.ErrProviderTimeout), "provider timeout"},
	}
	for _, tc := range cases {
		if got := compactResolveReason(tc.err); got != tc.want {
			t.Errorf("%s: compactResolveReason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPR94CompactReasonPrefersDeepStatus: providers wrap the HTTP
// failure INSIDE a textual extract chain («extract failed: … HTTP 400
// …») — the summary reason compacts to the status code itself.
func TestPR94CompactReasonPrefersDeepStatus(t *testing.T) {
	deep := contracts.WrapProvider("allanime", "request", 400, errors.New("unexpected http status 400 400 Bad Request"))
	err := contracts.WrapProvider("allanime", contracts.OpResolveStream, 0, fmt.Errorf("extract failed: allanime: episode query transport: %w", deep))
	if got := compactResolveReason(err); got != "HTTP 400" {
		t.Fatalf("compactResolveReason = %q, want %q", got, "HTTP 400")
	}
}

// TestPR94FailSoftAllBrokenNamesEveryProvider: zero entries across all
// providers → the typed error names EVERY failed provider with its
// reason, in deterministic sorted-key order — never one arbitrary
// completion-order culprit (PR94 defect A, honest attribution).
func TestPR94FailSoftAllBrokenNamesEveryProvider(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{episodes: testEpisodeSet()},
		resolveErrs: map[string]error{
			"[animego] Дубль 1": contracts.WrapProvider("animego", contracts.OpResolveStream, 503, nil),
			"[anilib] AniLib":   contracts.WrapProvider("anilib", contracts.OpResolveStream, 0, contracts.ErrExtractFailed),
		},
	}
	entries, skipped, err := resolveAllStreams(context.Background(), eps, pr94Embeds(), "", nil)
	if err == nil {
		t.Fatal("all providers broken must fail the merged resolve")
	}
	if len(entries) != 0 {
		t.Fatalf("no entries may surface, got %+v", entries)
	}
	var typed *errResolveFailed
	if !errors.As(err, &typed) {
		t.Fatalf("err = %T (%v), want *errResolveFailed", err, err)
	}
	// sortedEmbedKeys puts [anilib] AniLib before [animego] Дубль 1 —
	// attribution follows the consulted order, not completion order.
	// #161: the typed extract wall summarizes to its class label
	// («extract error») — the raw chain rides the log sink.
	want := "streams not fetched: anilib (extract error), animego (HTTP 503)"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
	if len(skipped) != 2 || skipped[0].Provider != "anilib" || skipped[1].Provider != "animego" {
		t.Fatalf("skipped = %+v, want [anilib animego] in sorted order", skipped)
	}
	// The typed error stays errors.Is-routable to each cause.
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("the typed error must unwrap to the per-provider causes")
	}
}

// TestPR94FailSoftSuccessButEmptyLinksPlusFailure: a provider that
// answered without links is not a failure, but the broken sibling
// still names itself when the merge comes up empty.
func TestPR94FailSoftSuccessButEmptyLinksPlusFailure(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {}, // success, zero links
			},
		},
		resolveErrs: map[string]error{
			"[animego] Дубль 1": contracts.WrapProvider("animego", contracts.OpResolveStream, 400, nil),
		},
	}
	_, skipped, err := resolveAllStreams(context.Background(), eps, pr94Embeds(), "", nil)
	var typed *errResolveFailed
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want the typed all-failed error", err)
	}
	if len(skipped) != 1 || skipped[0].Provider != "animego" {
		t.Fatalf("skipped = %+v, want the broken provider only", skipped)
	}
	if !strings.Contains(err.Error(), "animego (HTTP 400)") {
		t.Fatalf("err = %q, want the animego attribution", err.Error())
	}
}

// TestPR94FailSoftNoFailuresNoEntries: every provider answered without
// links and without errors — the plain «streams not found» verdict.
func TestPR94FailSoftNoFailuresNoEntries(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[animego] Дубль 1": {},
				"[anilib] AniLib":   {},
			},
		},
	}
	entries, skipped, err := resolveAllStreams(context.Background(), eps, pr94Embeds(), "", nil)
	if err == nil || err.Error() != "streams not found" {
		t.Fatalf("err = %v, want %q", err, "streams not found")
	}
	if len(entries) != 0 || len(skipped) != 0 {
		t.Fatalf("entries/skipped = %+v / %+v, want empty", entries, skipped)
	}
}

// TestPR94FailSoftScopedSuccessNoSkipped: the scoped fast path
// resolves only its own dub — no sibling failure can leak into its
// skipped list.
func TestPR94FailSoftScopedSuccessNoSkipped(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {Links: map[string]contracts.VideoSource{"720": {URL: "u-720"}}},
			},
		},
		resolveErrs: map[string]error{
			"[animego] Дубль 1": contracts.WrapProvider("animego", contracts.OpResolveStream, 400, nil),
		},
	}
	entries, skipped, err := resolveAllStreams(context.Background(), eps, pr94Embeds(), "[anilib] AniLib", nil)
	if err != nil || len(entries) != 1 {
		t.Fatalf("scoped healthy resolve must succeed, got %v / %+v", err, entries)
	}
	if len(skipped) != 0 {
		t.Fatalf("scoped success must carry no skipped providers, got %+v", skipped)
	}
}

// TestPR94ScopedFailureFallsThroughToMerged: the remembered dub's
// provider fails on the scoped fast path → the flow must NOT abort to
// the menu; it falls through to the full merged resolve of the REST
// and opens the picker with the honest skip summary (PR94 defect B).
func TestPR94ScopedFailureFallsThroughToMerged(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}}},
				"anilib":  {{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"u1b"}}}},
			},
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {Links: map[string]contracts.VideoSource{"720": {URL: "u-720"}}},
			},
		},
		resolveErrs: map[string]error{
			"[animego] Дубль 1": contracts.WrapProvider("animego", contracts.OpResolveStream, 400, nil),
		},
	}
	deps := &Deps{Episode: eps, Playback: &fakePlayback{}}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatal("missing scoped resolve command")
	}
	if ss.state != sessionStateResolveLoading {
		t.Fatalf("state = %v, want the loading surface", ss.state)
	}

	// The scoped round settles with the remembered provider's failure.
	msg := cmd().(streamResolvedMsg)
	if msg.scope == "" || msg.err == nil {
		t.Fatalf("scoped round must fail, got scope=%q err=%v", msg.scope, msg.err)
	}
	next2, cmd2 := ss.Update(msg)
	ss2 := next2.(*sessionScreen)
	if ss2.state != sessionStateQuality {
		t.Fatalf("state = %v, want the merged picker after the fall-through", ss2.state)
	}
	if cmd2 == nil {
		t.Fatal("the fall-through must launch the full merged resolve")
	}

	// The merged round settles: anilib's entries surface, animego's
	// failure is the honest summary in the picker status.
	msg2 := cmd2().(streamResolvedMsg)
	if msg2.scope != "" {
		t.Fatalf("fall-through round must be unscoped, got scope %q", msg2.scope)
	}
	next3, _ := ss2.Update(msg2)
	ss3 := next3.(*sessionScreen)
	if ss3.state != sessionStateQuality {
		t.Fatalf("state = %v, want the picker still open", ss3.state)
	}
	if len(ss3.streamEntries) != 1 || ss3.streamEntries[0].DubKey != "[anilib] AniLib" {
		t.Fatalf("merged entries = %+v, want the healthy provider's stream", ss3.streamEntries)
	}
	if !strings.Contains(ss3.status, "skipped: animego (HTTP 400)") {
		t.Fatalf("status = %q, want the skip summary", ss3.status)
	}
}

// TestPR94ScopedFailureAllBrokenNamesAll: the fall-through's merged
// round also comes up empty → the typed error names EVERY provider —
// the remembered one included — and the flow explains at the menu.
func TestPR94ScopedFailureAllBrokenNamesAll(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}}},
				"anilib":  {{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"u1b"}}}},
			},
		},
		resolveErrs: map[string]error{
			"[animego] Дубль 1": contracts.WrapProvider("animego", contracts.OpResolveStream, 400, nil),
			"[anilib] AniLib":   contracts.WrapProvider("anilib", contracts.OpResolveStream, 0, contracts.ErrExtractFailed),
		},
	}
	deps := &Deps{Episode: eps, Playback: &fakePlayback{}}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"

	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	msg := cmd().(streamResolvedMsg)
	next2, cmd2 := ss.Update(msg)
	ss2 := next2.(*sessionScreen)
	if cmd2 == nil {
		t.Fatal("the fall-through must launch the full merged resolve")
	}
	msg2 := cmd2().(streamResolvedMsg)
	next3, _ := ss2.Update(msg2)
	ss3 := next3.(*sessionScreen)
	if ss3.state != sessionStateMenu {
		t.Fatalf("state = %v, want the menu after the all-broken verdict", ss3.state)
	}
	// #161: the typed extract wall summarizes to «extract error».
	for _, want := range []string{"animego (HTTP 400)", "anilib (extract error)"} {
		if !strings.Contains(ss3.status, want) {
			t.Fatalf("status = %q, want the %s attribution", ss3.status, want)
		}
	}
}

// TestPR94SummaryComposesWithVanishedDubNote: the merged picker's
// skip summary composes with the vanished-dub warning the «⏭ Next»
// flow already stamped — both honest notes stay visible (PR94
// defect C, python resolve_dubs_smart parity).
func TestPR94SummaryComposesWithVanishedDubNote(t *testing.T) {
	eps := &failsoftEpisode{
		fakeEpisode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{"Дубль 1": {"u1v"}}}},
				"anilib":  {{Num: "1", RawID: "b1", RawEmbeds: map[string][]string{"AniLib": {"u1b"}}}},
				"xprov":   {{Num: "1", RawID: "x1", RawEmbeds: map[string][]string{"XDub": {"u1x"}}}},
			},
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {Links: map[string]contracts.VideoSource{"720": {URL: "u-720"}}},
			},
		},
		resolveErrs: map[string]error{
			"[xprov] XDub": contracts.WrapProvider("xprov", contracts.OpResolveStream, 403, nil),
		},
	}
	deps := &Deps{Episode: eps, Playback: &fakePlayback{}}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
		{Title: "Тайтл", URL: "u3", SourceID: "xprov"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"

	// The vanished-dub path: episode 1 has no animego embeds for the
	// REMEMBERED dub (simulate by wiping them), so «⏭ Next» warns and
	// runs the unscoped merged resolve.
	s.currentEpisodeData().RawEmbeds["[animego] Дубль 1"] = nil
	scr, cmd := s.autoWatchNext()
	ss := scr.(*sessionScreen)
	if !strings.Contains(ss.status, "Previous settings unavailable") {
		t.Fatalf("status = %q, want the vanished-dub warning first", ss.status)
	}
	if cmd == nil {
		t.Fatal("missing merged resolve command")
	}
	msg := cmd().(streamResolvedMsg)
	next2, _ := ss.Update(msg)
	ss2 := next2.(*sessionScreen)
	if len(ss2.streamEntries) != 1 {
		t.Fatalf("entries = %+v, want the healthy anilib stream", ss2.streamEntries)
	}
	if !strings.Contains(ss2.status, "Previous settings unavailable") {
		t.Fatalf("status = %q, the vanished-dub note must survive", ss2.status)
	}
	if !strings.Contains(ss2.status, "skipped: xprov (HTTP 403)") {
		t.Fatalf("status = %q, want the skip summary", ss2.status)
	}
}

// TestPR94CleanResolveStampsNoSummary: a fully healthy merged resolve
// stamps nothing — no fake attribution (PR94 defect C, the honest
// absence of failures).
func TestPR94CleanResolveStampsNoSummary(t *testing.T) {
	deps := &Deps{Episode: &fakeEpisode{episodes: testEpisodeSet()}, Playback: &fakePlayback{}}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.list.Jump(sessionActionIndex(s, "watch"))
	next, _ := s.Update(enter())
	ss := next.(*sessionScreen)
	ss.formatList.Jump(indexOfDayFormatList(ss, "stream"))
	_, cmd := ss.Update(enter())
	msg := cmd().(streamResolvedMsg)
	next2, _ := ss.Update(msg)
	ss2 := next2.(*sessionScreen)
	if strings.Contains(ss2.status, "пропущены") {
		t.Fatalf("status = %q, a clean resolve must not stamp a summary", ss2.status)
	}
}
