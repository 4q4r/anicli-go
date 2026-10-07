package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// episodesProvider fakes a registry provider with canned episodes.
type episodesProvider struct {
	fakeProvider
	sourceType contracts.SourceType
	episodes   []contracts.Episode
	streams    map[string]contracts.MediaStream
	streamErr  map[string]error
}

func (p *episodesProvider) SourceType() contracts.SourceType { return p.sourceType }
func (p *episodesProvider) GetEpisodes(_ context.Context, animeURL string) ([]contracts.Episode, error) {
	if animeURL == "https://fake.example/broken" {
		return nil, contracts.WrapProvider(p.id, contracts.OpGetEpisodes, 0, contracts.ErrProvider403)
	}
	return p.episodes, nil
}
func (p *episodesProvider) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if err, ok := p.streamErr[dubID]; ok {
		return contracts.MediaStream{}, err
	}
	if s, ok := p.streams[dubID]; ok {
		return s, nil
	}
	return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, nil
}

func newEpisodesApp(t *testing.T, p *episodesProvider) *App {
	t.Helper()
	app := newTestApp(t)
	// Replace the default registry member with the episodes fake
	// (same "fake" id; a fresh registry avoids the duplicate-id error).
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("register episodes provider: %v", err)
	}
	app.registry = reg
	return app
}

func authHeader(t *testing.T, h http.Handler) map[string]string {
	t.Helper()
	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", loginBody(testPass, ""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %v", rec.Code, payload)
	}
	token, _ := payload["access_token"].(string)
	return bear(nil, token)
}

func TestEpisodesDTONoRawURLLeak(t *testing.T) {
	p := &episodesProvider{
		fakeProvider: fakeProvider{id: "fake"},
		sourceType:   contracts.SourceTypeBoth,
		episodes: []contracts.Episode{
			{
				Num: "1", Title: "First", RawID: "e1",
				RawEmbeds: map[string][]string{
					"Dub A": {"https://embed.example/a1", "https://embed.example/a2"},
					"Dub B": {"https://embed.example/b1"},
				},
			},
			{Num: "2", RawID: "e2"},
		},
	}
	app := newEpisodesApp(t, p)
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/episodes?source_id=fake&source_url=https%3A%2F%2Ffake.example%2Fanime", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("episodes = %d: %v", rec.Code, payload)
	}
	if cached, _ := payload["cached"].(bool); cached {
		t.Fatal("first fetch must not be cached")
	}
	items, _ := payload["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	first, _ := items[0].(map[string]any)
	// video/audio keys split by source type "both": both carry the keys.
	videoKeys, _ := first["video_keys"].([]any)
	audioKeys, _ := first["audio_keys"].([]any)
	if len(videoKeys) != 2 || len(audioKeys) != 2 {
		t.Fatalf("keys = %v / %v", videoKeys, audioKeys)
	}
	if videoKeys[0] != "Dub A" || videoKeys[1] != "Dub B" {
		t.Fatalf("keys must be sorted: %v", videoKeys)
	}
	if mixed, _ := first["mixed_possible"].(bool); !mixed {
		t.Fatal("two tracks on a both-source must set mixed_possible")
	}
	if first["num"] != "1" || first["raw_id"] != "e1" || first["title"] != "First" {
		t.Fatalf("episode fields wrong: %v", first)
	}

	// THE RULING: no raw embed URLs anywhere in the payload.
	body := fmt.Sprint(payload)
	if strings.Contains(body, "embed.example") {
		t.Fatal("raw embed URLs leaked into the episodes DTO")
	}

	// Empty second episode: no keys, no mixed.
	second, _ := items[1].(map[string]any)
	if vk, _ := second["video_keys"].([]any); len(vk) != 0 {
		t.Fatalf("empty episode must have no keys: %v", second)
	}
	if mixed, _ := second["mixed_possible"].(bool); mixed {
		t.Fatal("empty episode must not be mixed_possible")
	}
}

func TestEpisodesAudioOnlySourceType(t *testing.T) {
	p := &episodesProvider{
		fakeProvider: fakeProvider{id: "audiofake"},
		sourceType:   contracts.SourceTypeAudio,
		episodes: []contracts.Episode{{
			Num: "1", RawID: "e1",
			RawEmbeds: map[string][]string{"Dub": {"https://x"}},
		}},
	}
	app := newEpisodesApp(t, p)
	h := app.Router()
	auth := authHeader(t, h)

	_, payload := doJSON(t, h, http.MethodGet, "/api/v1/episodes?source_id=audiofake&source_url=u", "", auth)
	items, _ := payload["items"].([]any)
	first, _ := items[0].(map[string]any)
	if vk, _ := first["video_keys"].([]any); len(vk) != 0 {
		t.Fatalf("audio source must not emit video keys: %v", first)
	}
	if ak, _ := first["audio_keys"].([]any); len(ak) != 1 {
		t.Fatalf("audio source must emit audio keys: %v", first)
	}
}

// TestEpisodesVideoOnlyMultiDubNotMixed pins the python mixed_possible
// precedence: bool(video_keys and audio_keys and (sets differ or
// len(video_keys) > 1)) — a video-only source has no audio keys, so
// even multiple dubs never set mixed_possible.
func TestEpisodesVideoOnlyMultiDubNotMixed(t *testing.T) {
	p := &episodesProvider{
		fakeProvider: fakeProvider{id: "videofake"},
		sourceType:   contracts.SourceTypeVideo,
		episodes: []contracts.Episode{{
			Num: "1", RawID: "e1",
			RawEmbeds: map[string][]string{
				"Dub A": {"https://embed.example/a1"},
				"Dub B": {"https://embed.example/b1"},
			},
		}},
	}
	app := newEpisodesApp(t, p)
	h := app.Router()
	auth := authHeader(t, h)

	_, payload := doJSON(t, h, http.MethodGet, "/api/v1/episodes?source_id=videofake&source_url=u", "", auth)
	items, _ := payload["items"].([]any)
	first, _ := items[0].(map[string]any)
	if vk, _ := first["video_keys"].([]any); len(vk) != 2 {
		t.Fatalf("video source must emit both video keys: %v", first)
	}
	if ak, _ := first["audio_keys"].([]any); len(ak) != 0 {
		t.Fatalf("video source must emit no audio keys: %v", first)
	}
	if mixed, _ := first["mixed_possible"].(bool); mixed {
		t.Fatal("video-only multi-dub must not set mixed_possible (python requires non-empty audio_keys)")
	}
}

func TestEpisodesProviderErrorContract(t *testing.T) {
	p := &episodesProvider{fakeProvider: fakeProvider{id: "fake"}, sourceType: contracts.SourceTypeBoth}
	app := newEpisodesApp(t, p)
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/episodes?source_id=fake&source_url=https%3A%2F%2Ffake.example%2Fbroken", "", auth)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("provider_403 = %d, want 403", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "provider_403" {
		t.Fatalf("code = %v", errObj["code"])
	}
	if errObj["trace_id"] == "" {
		t.Fatal("trace_id must ride provider errors")
	}
}

func TestStreamsResolveHappyPath(t *testing.T) {
	p := &episodesProvider{
		fakeProvider: fakeProvider{id: "fake"},
		sourceType:   contracts.SourceTypeBoth,
		// Provider-native BARE dub names: the resolve handler strips
		// the merged "[prov] " track tag before the call (#158
		// fix-round 2, python extract_best_source parity) — the same
		// split a real provider lives with (its episodes listing
		// merges under tagged keys, its resolve consumes bare names).
		streams: map[string]contracts.MediaStream{
			"Dub V": {DubName: "Dub V", Links: map[string]contracts.VideoSource{
				"1080": {URL: "https://cdn.example/v1080.m3u8", Quality: "1080", Type: "m3u8"},
				"720":  {URL: "https://cdn.example/v720.m3u8", Quality: "720", Type: "m3u8", Headers: map[string]string{"Referer": "https://fake.example"}},
				"480":  {URL: "https://cdn.example/v480.mp4", Quality: "480", Type: "mp4"},
			}},
			"Dub A": {DubName: "Dub A", Links: map[string]contracts.VideoSource{
				"192": {URL: "https://cdn.example/a192.mp4?expires=4102444800", Quality: "192", Type: "mp4"},
			}},
		},
	}
	app := newEpisodesApp(t, p)
	// Register the secondary provider used via the "[other] ..." track key.
	h := app.Router()
	auth := authHeader(t, h)

	body := `{
		"source_id": "fake",
		"episode_num": "1",
		"episode_raw_id": "e1",
		"video_key": "Dub V",
		"audio_key": "[other] Dub A",
		"urls_video": ["https://embed.example/v1"],
		"urls_audio": ["https://embed.example/a1"]
	}`
	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/streams/resolve", body, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve = %d: %v", rec.Code, payload)
	}

	if payload["mux_mode"] != "dual_url" {
		t.Fatalf("mux_mode = %v (differing keys must be dual_url)", payload["mux_mode"])
	}
	videoStreams, _ := payload["video_streams"].([]any)
	if len(videoStreams) != 3 {
		t.Fatalf("video_streams = %d, want 3", len(videoStreams))
	}
	// Python sorted(items, key=lambda item: item[0], reverse=True) is
	// LEXICOGRAPHIC on the string keys: {"1080","720","480"} →
	// [720, 480, 1080] — not numeric order.
	wantOrder := []int{720, 480, 1080}
	for i, want := range wantOrder {
		item, _ := videoStreams[i].(map[string]any)
		if item["quality"] != float64(want) {
			t.Fatalf("video_streams[%d].quality = %v, want %d", i, item["quality"], want)
		}
	}
	top, _ := videoStreams[0].(map[string]any)
	if top["type"] != "hls" || top["is_hls"] != true {
		t.Fatalf("hls hint wrong: %v", top)
	}
	if top["url"] != "https://cdn.example/v720.m3u8" {
		t.Fatalf("url = %v", top["url"])
	}

	audioStreams, _ := payload["audio_streams"].([]any)
	audio, _ := audioStreams[0].(map[string]any)
	if audio["name"] != "[other] Dub A" {
		t.Fatalf("audio name = %v", audio["name"])
	}
	if audio["default"] != true {
		t.Fatal("first audio variant must be default")
	}
	if audio["quality"] != float64(192) {
		t.Fatalf("audio quality = %v", audio["quality"])
	}
	if audio["expires_at"] == nil {
		t.Fatal("tokenized url must expose expires_at")
	}

	meta, _ := payload["source_meta"].(map[string]any)
	if meta["provider"] != "fake" {
		t.Fatalf("source_meta.provider = %v", meta["provider"])
	}
	if meta["tokenized"] != true {
		t.Fatal("tokenized must be true when any url carries expiry")
	}
	if meta["requires_headers"] != true {
		t.Fatal("requires_headers must be true (720 variant carries Referer)")
	}
	if order, _ := meta["recommended_order"].([]any); len(order) != 3 ||
		order[0] != float64(720) || order[1] != float64(480) || order[2] != float64(1080) {
		t.Fatalf("recommended_order = %v", meta["recommended_order"])
	}

	// single_av when audio key equals video key.
	bodySingle := `{
		"source_id": "fake", "episode_num": "1", "episode_raw_id": "e1",
		"video_key": "Dub V", "audio_key": "Dub V",
		"urls_video": ["https://embed.example/v1"], "urls_audio": ["https://embed.example/a1"]
	}`
	_, payload = doJSON(t, h, http.MethodPost, "/api/v1/streams/resolve", bodySingle, auth)
	if payload["mux_mode"] != "single_av" {
		t.Fatalf("same-key audio must be single_av, got %v", payload["mux_mode"])
	}
}

// TestSortedQualities pins the python variant ordering: sorted(items,
// key=lambda item: item[0], reverse=True) is LEXICOGRAPHIC descending
// on the string keys (api_server.py streams/resolve), so 3-digit and
// 4-digit quality labels interleave in string order, not numeric.
func TestSortedQualities(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want []string
	}{
		{"three keys lexicographic not numeric", []string{"1080", "720", "480"}, []string{"720", "480", "1080"}},
		{"two keys", []string{"1080", "720"}, []string{"720", "1080"}},
		{"lexicographic beats numeric", []string{"1000", "720"}, []string{"720", "1000"}},
		{"single key", []string{"480"}, []string{"480"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			links := map[string]contracts.VideoSource{}
			for _, k := range tc.keys {
				links[k] = contracts.VideoSource{URL: "https://cdn.example/" + k + ".mp4"}
			}
			got := sortedQualities(links)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("sortedQualities(%v) = %v, want %v", tc.keys, got, tc.want)
			}
		})
	}
}

func TestStreamsResolveFailures(t *testing.T) {
	p := &episodesProvider{
		fakeProvider: fakeProvider{id: "fake"},
		sourceType:   contracts.SourceTypeBoth,
		streamErr: map[string]error{
			"Dub V": contracts.ErrAllCandidatesFailed,
		},
	}
	app := newEpisodesApp(t, p)
	h := app.Router()
	auth := authHeader(t, h)

	// Missing video urls -> 422 validation.
	rec, _ := doJSON(t, h, http.MethodPost, "/api/v1/streams/resolve",
		`{"source_id":"fake","episode_num":"1","episode_raw_id":"e","video_key":"k","urls_video":[]}`, auth)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty urls_video = %d, want 422", rec.Code)
	}

	// Nothing resolves -> 502 all_candidates_failed with track details.
	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/streams/resolve",
		`{"source_id":"fake","episode_num":"1","episode_raw_id":"e","video_key":"Dub V","urls_video":["https://x"]}`, auth)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unresolvable = %d, want 502", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "all_candidates_failed" {
		t.Fatalf("code = %v", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["track"] != "video" {
		t.Fatalf("details.track = %v", details)
	}
}

// rawIDRecorder records the RawID every ResolveStream call receives —
// the decomposition pin for the streams/resolve handler (#157). The
// handler composes the python-parity request episode
// ("source:rawid"), and the python resolve loop strips the called
// provider's own part before provider.resolve_stream
// (cli/stream_resolver.py extract_best_source) — the step the Go port
// skipped, handing "animevib:{...}" to the lua scripts whose
// json.decode crashed on the first byte 'a'.
type rawIDRecorder struct {
	fakeProvider
	links  bool
	gotRaw []string
	gotDub []string
}

func (p *rawIDRecorder) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	p.gotRaw = append(p.gotRaw, episode.RawID)
	p.gotDub = append(p.gotDub, dubID)
	if !p.links {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, nil
	}
	return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{
		"720": {URL: "https://cdn.example/720.m3u8", Quality: "720"},
	}}, nil
}

// TestStreamsResolveDecomposesPrefixedRawID pins both resolve legs:
// the video provider receives its own bare part of the composed raw
// id (the merged bytes' first byte 'a' is what crashed animevib
// live), and a genuinely foreign audio provider receives NO raw id —
// python's decomposition loop-miss ("", extract_best_source) — the
// video source's state must never leak across providers. The response
// status is irrelevant to the pin (the recorders' link maps decide
// it); the recorded raw ids are the contract.
func TestStreamsResolveDecomposesPrefixedRawID(t *testing.T) {
	t.Parallel()

	video := &rawIDRecorder{fakeProvider: fakeProvider{id: "fake"}, links: true}
	audio := &rawIDRecorder{fakeProvider: fakeProvider{id: "other"}}
	app := newTestApp(t)
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(video); err != nil {
		t.Fatalf("register video provider: %v", err)
	}
	if err := reg.Register(audio); err != nil {
		t.Fatalf("register audio provider: %v", err)
	}
	app.registry = reg
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/streams/resolve", `{
		"source_id": "fake",
		"episode_num": "1",
		"episode_raw_id": "{\"n\":\"1\",\"u\":\"https://www.animevib.ru/1.html\"}",
		"video_key": "JAM",
		"audio_key": "[other] AniLib",
		"urls_video": ["https://embed.example/v1"],
		"urls_audio": ["https://embed.example/a1"]
	}`, auth)
	// The linkless audio recorder lands the handler's honest 502 for
	// the audio track — the resolve DID reach both providers.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("resolve = %d: %v", rec.Code, payload)
	}

	want := `{"n":"1","u":"https://www.animevib.ru/1.html"}`
	if len(video.gotRaw) != 1 || video.gotRaw[0] != want {
		t.Fatalf("video provider got RawID %q, want the bare %q", video.gotRaw, want)
	}
	// The "[other]" audio provider names a different source than the
	// request's single episode_raw_id carries: python's loop-miss
	// yields "" — no state leak across providers.
	if len(audio.gotRaw) != 1 || audio.gotRaw[0] != "" {
		t.Fatalf("audio provider got RawID %q, want \"\" (the python loop-miss)", audio.gotRaw)
	}
}

// TestStreamsResolveStripsTrackTagFromKeys pins the dub half of the
// python extract_best_source decomposition on the API surface
// (fix-round 2 of #158): the api server routes through the same
// extract_best_source (api_server.py _api_resolve_streams →
// attempt_extraction), whose line 174 strips the merged track tag —
// re.sub(r"^\[.*?\]\s*", "", dub_key) — before every
// provider.resolve_stream, on BOTH legs. The Go port passed the keys
// verbatim: a client replaying the merged-session key
// "[animevib] Amazing Dubbing" handed the lua script a name its bare
// dub comparison could never match. A bare key passes through
// unchanged (the direct-callers contract).
func TestStreamsResolveStripsTrackTagFromKeys(t *testing.T) {
	t.Parallel()

	video := &rawIDRecorder{fakeProvider: fakeProvider{id: "fake"}, links: true}
	audio := &rawIDRecorder{fakeProvider: fakeProvider{id: "other"}}
	app := newTestApp(t)
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(video); err != nil {
		t.Fatalf("register video provider: %v", err)
	}
	if err := reg.Register(audio); err != nil {
		t.Fatalf("register audio provider: %v", err)
	}
	app.registry = reg
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/streams/resolve", `{
		"source_id": "fake",
		"episode_num": "3",
		"episode_raw_id": "{\"n\":\"3\",\"u\":\"https://www.animevib.ru/1.html\"}",
		"video_key": "[animevib] Amazing Dubbing",
		"audio_key": "[other] AniLib",
		"urls_video": ["https://embed.example/v1"],
		"urls_audio": ["https://embed.example/a1"]
	}`, auth)
	// The linkless audio recorder lands the handler's honest 502 —
	// the resolve reached both providers.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("resolve = %d: %v", rec.Code, payload)
	}

	if len(video.gotDub) != 1 || video.gotDub[0] != "Amazing Dubbing" {
		t.Fatalf("video provider got dubID %q, want the bare %q", video.gotDub, "Amazing Dubbing")
	}
	if len(audio.gotDub) != 1 || audio.gotDub[0] != "AniLib" {
		t.Fatalf("audio provider got dubID %q, want the bare %q", audio.gotDub, "AniLib")
	}
}

// fakeShiki is a test double for the ShikiClient interface.
type fakeShiki struct {
	rates        []shikimori.UserRate
	animes       []shikimori.Anime
	ongoing      []shikimori.OngoingCandidate
	autocomplete []shikimori.AutocompleteItem
	authed       bool
	ratesErr     error
}

func (f *fakeShiki) Autocomplete(_ context.Context, _ string, _ int) ([]shikimori.AutocompleteItem, error) {
	return f.autocomplete, nil
}
func (f *fakeShiki) GetUserRates(context.Context) ([]shikimori.UserRate, error) {
	return f.rates, f.ratesErr
}
func (f *fakeShiki) GetAnimesInfo(_ context.Context, _ []int64) ([]shikimori.Anime, error) {
	return f.animes, nil
}
func (f *fakeShiki) FetchOngoingCandidates(context.Context) ([]shikimori.OngoingCandidate, error) {
	return f.ongoing, nil
}
func (f *fakeShiki) GetAnimeDetails(_ context.Context, id int64) (*shikimori.AnimeDetails, error) {
	return &shikimori.AnimeDetails{Anime: shikimori.Anime{ID: id, Name: "Detail"}}, nil
}
func (f *fakeShiki) Authenticated() bool { return f.authed }

func TestSearchAutocompleteViaFakeShiki(t *testing.T) {
	app := newTestApp(t)
	titleRu, titleEn := "Стальной алхимик", "Fullmetal Alchemist"
	app.shiki = &fakeShiki{autocomplete: []shikimori.AutocompleteItem{{
		ShikimoriID: 5114, TitleRu: &titleRu, TitleEn: &titleEn, URL: "/anime/5114",
	}}}
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/search?q=alchemist", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("search = %d: %v", rec.Code, payload)
	}
	if payload["query"] != "alchemist" {
		t.Fatalf("query echo = %v", payload["query"])
	}
	if cached, _ := payload["cached"].(bool); cached {
		t.Fatal("first search must not be cached")
	}
	results, _ := payload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("results = %v", payload["results"])
	}
	item, _ := results[0].(map[string]any)
	if item["shikimori_id"] != float64(5114) || item["url"] != "/anime/5114" {
		t.Fatalf("item = %v", item)
	}

	// Second identical search hits the cache (cached=true) with the
	// same payload.
	_, payload = doJSON(t, h, http.MethodGet, "/api/v1/search?q=alchemist", "", auth)
	if cached, _ := payload["cached"].(bool); !cached {
		t.Fatal("second search must be served from cache")
	}
	if results, _ := payload["results"].([]any); len(results) != 1 {
		t.Fatalf("cached results = %v", payload["results"])
	}

	// Missing q -> 422.
	rec, _ = doJSON(t, h, http.MethodGet, "/api/v1/search", "", auth)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing q = %d, want 422", rec.Code)
	}
}

func TestSearchShikiErrorDegradesToEmpty(t *testing.T) {
	app := newTestApp(t)
	app.shiki = &fakeShiki{ratesErr: fmt.Errorf("nope")}
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/search?q=x", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded search = %d, want 200", rec.Code)
	}
	if results, _ := payload["results"].([]any); len(results) != 0 {
		t.Fatalf("results = %v", payload["results"])
	}
}

func seedLocalAnime(t *testing.T, app *App, title string, shikiID int64, episode string, updated time.Time) int64 {
	t.Helper()
	poster := "https://img.example/p.jpg"
	row := &storage.AnimeProgress{
		Title: title, Poster: &poster, SourceID: "fake", SourceURL: "https://fake.example/" + title,
		CurrentEpisode: episode, ShikimoriID: &shikiID, ShikimoriStatus: "watching",
		UpdatedAt: updated,
	}
	if err := app.store.Progress.Upsert(context.Background(), row); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := app.store.Sources.ReplaceForAnime(context.Background(), row.ID, []storage.AnimeSource{
		{AnimeProgressID: row.ID, SourceID: "fake", SourceURL: row.SourceURL},
	}); err != nil {
		t.Fatalf("seed sources: %v", err)
	}
	return row.ID
}

func TestHomeFeedComposition(t *testing.T) {
	app := newTestApp(t)

	now := time.Now().UTC()
	// Local row, watched 3 episodes, updated recently.
	seedLocalAnime(t, app, "Local Anime", 100, "3", now.Add(-time.Hour))
	// Older second row.
	seedLocalAnime(t, app, "Second Anime", 200, "5", now.Add(-48*time.Hour))

	app.shiki = &fakeShiki{
		authed: true,
		rates: []shikimori.UserRate{
			{ID: 1, TargetID: 100, TargetType: "Anime", Status: "watching", Episodes: 3, CreatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339), UpdatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
			{ID: 2, TargetID: 300, TargetType: "Anime", Status: "planned", CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		},
		animes: []shikimori.Anime{
			{ID: 100, Name: "Anime EN", Russian: "Аниме", Status: "ongoing", EpisodesAired: 6, NextEpisode: 7, Score: json.Number("9.1"),
				Image:         shikimori.Image{Original: "/system/animes/original/100.jpg"},
				NextEpisodeAt: now.Add(-5 * time.Hour).Format(time.RFC3339)},
			{ID: 300, Name: "Planned EN", Russian: "Запланировано", Status: "anounced", Score: json.Number("7.2"),
				Image: shikimori.Image{Original: "/system/animes/original/300.jpg"}},
		},
		ongoing: []shikimori.OngoingCandidate{
			{ShikimoriID: 100, PosterURL: "https://shikimori.io/system/animes/original/100.jpg", SourceURL: "https://shikimori.io/animes/100"},
			{ShikimoriID: 999, PosterURL: "https://shikimori.io/system/animes/original/999.jpg", SourceURL: "https://shikimori.io/animes/999"},
		},
	}
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/home/feed", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("feed = %d: %v", rec.Code, payload)
	}

	sections, _ := payload["sections"].(map[string]any)

	// continue_watching: rate 100 is watching, locally bound, watched 3
	// with 6 aired (more available).
	cw, _ := sections["continue_watching"].(map[string]any)
	cwItems, _ := cw["items"].([]any)
	if len(cwItems) != 1 {
		t.Fatalf("continue_watching items = %d, want 1: %v", len(cwItems), cw)
	}
	cwCard, _ := cwItems[0].(map[string]any)
	if cwCard["shikimori_id"] != float64(100) {
		t.Fatalf("continue card id = %v", cwCard["shikimori_id"])
	}
	if bl, _ := cwCard["bottom_text_left"].(string); bl != "Остановились на 3 серии" {
		t.Fatalf("bottom_text_left = %v", cwCard["bottom_text_left"])
	}

	// new_releases: anime 100 has next_episode_at 5h ago (inside 14d).
	nr, _ := sections["new_releases"].(map[string]any)
	nrItems, _ := nr["items"].([]any)
	if len(nrItems) != 1 {
		t.Fatalf("new_releases items = %d, want 1: %v", len(nrItems), nr)
	}
	nrCard, _ := nrItems[0].(map[string]any)
	if nrCard["shikimori_id"] != float64(100) {
		t.Fatalf("new_releases card = %v", nrCard)
	}

	// library_recent: both rates, newest created_at first (300 then 100).
	lr, _ := sections["library_recent"].(map[string]any)
	lrItems, _ := lr["items"].([]any)
	if len(lrItems) != 2 {
		t.Fatalf("library_recent items = %d, want 2: %v", len(lrItems), lr)
	}
	lrFirst, _ := lrItems[0].(map[string]any)
	if lrFirst["shikimori_id"] != float64(300) {
		t.Fatalf("library_recent order broken: first = %v", lrFirst["shikimori_id"])
	}
	if lrFirst["badge"] != "В планах" {
		t.Fatalf("planned badge = %v", lrFirst["badge"])
	}
	lrSecond, _ := lrItems[1].(map[string]any)
	if lrSecond["badge"] != "Смотрю" {
		t.Fatalf("watching badge = %v", lrSecond["badge"])
	}

	// hero: personalized first (watching 100, planned 300), then the
	// ongoing-only candidate 999 (100 deduped) -> [100, 300, 999].
	hero, _ := payload["hero"].(map[string]any)
	heroItems, _ := hero["items"].([]any)
	if len(heroItems) != 3 {
		t.Fatalf("hero items = %d, want 3: %v", len(heroItems), hero)
	}
	heroFirst, _ := heroItems[0].(map[string]any)
	if heroFirst["shikimori_id"] != float64(100) {
		t.Fatalf("hero order broken: %v", heroFirst["shikimori_id"])
	}
	if heroFirst["badge"] != heroLibraryBadge {
		t.Fatalf("personalized hero badge = %v", heroFirst["badge"])
	}
	if heroFirst["background_url"] == "" {
		t.Fatal("hero card must carry background_url")
	}
	heroThird, _ := heroItems[2].(map[string]any)
	if heroThird["shikimori_id"] != float64(999) {
		t.Fatalf("ongoing-only candidate must follow personalized: %v", heroThird)
	}
	if heroThird["badge"] != heroBadge {
		t.Fatalf("ongoing hero badge = %v", heroThird["badge"])
	}

	// Poster resolution must be absolute against the shikimori base.
	if pu, _ := cwCard["poster_url"].(string); !strings.HasPrefix(pu, "https://shikimori.io/system/animes/original/100.jpg") {
		t.Fatalf("poster_url = %v", cwCard["poster_url"])
	}
}

func TestHomeFeedHeroPagination(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	seedLocalAnime(t, app, "L", 100, "1", now)

	var rates []shikimori.UserRate
	var animes []shikimori.Anime
	var ongoing []shikimori.OngoingCandidate
	for id := int64(1); id <= 5; id++ {
		rates = append(rates, shikimori.UserRate{
			ID: id, TargetID: id, TargetType: "Anime", Status: "watching", Episodes: 1,
			CreatedAt: now.Format(time.RFC3339),
		})
		animes = append(animes, shikimori.Anime{ID: id, Name: fmt.Sprintf("A%d", id), Status: "ongoing", EpisodesAired: 2})
	}
	app.shiki = &fakeShiki{authed: true, rates: rates, animes: animes, ongoing: ongoing}
	h := app.Router()
	auth := authHeader(t, h)

	_, page1 := doJSON(t, h, http.MethodGet, "/api/v1/home/feed?hero_limit=2", "", auth)
	hero, _ := page1["hero"].(map[string]any)
	items1, _ := hero["items"].([]any)
	if len(items1) != 2 {
		t.Fatalf("page1 hero = %d items", len(items1))
	}
	next, _ := hero["next_cursor"].(string)
	if next == "" {
		t.Fatal("next_cursor must be set when more pages exist")
	}

	rec, page2 := doJSON(t, h, http.MethodGet, "/api/v1/home/feed?hero_limit=2&hero_cursor="+next, "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("page2 = %d", rec.Code)
	}
	hero2, _ := page2["hero"].(map[string]any)
	items2, _ := hero2["items"].([]any)
	if len(items2) != 2 {
		t.Fatalf("page2 hero = %d items", len(items2))
	}
	// No overlap between pages.
	seen := map[float64]bool{}
	for _, it := range append(append([]any{}, items1...), items2...) {
		m, _ := it.(map[string]any)
		id := m["shikimori_id"].(float64)
		if seen[id] {
			t.Fatalf("anime %v duplicated across pages", id)
		}
		seen[id] = true
	}
}

func TestLibraryListLocalFallback(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	seedLocalAnime(t, app, "Local Anime", 100, "3", now.Add(-time.Hour))
	seedLocalAnime(t, app, "Older", 200, "1", now.Add(-72*time.Hour))

	// No shiki -> empty rates -> local DB fallback.
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/library", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("library = %d: %v", rec.Code, payload)
	}
	items, _ := payload["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("library fallback items = %d, want 2: %v", len(items), payload)
	}
	if total, _ := payload["total"].(float64); total != 2 {
		t.Fatalf("total = %v", payload["total"])
	}
	first, _ := items[0].(map[string]any)
	if first["shikimori_id"] != float64(100) {
		t.Fatalf("fallback must sort by updated_at desc: %v", first["shikimori_id"])
	}
	if first["has_local_binding"] != true {
		t.Fatal("fallback cards must set has_local_binding")
	}
	filters, _ := payload["filters"].(map[string]any)
	if fl, _ := filters["status"].([]any); len(fl) != 0 {
		t.Fatalf("empty filter must render empty list: %v", filters)
	}

	// Pagination.
	_, page2 := doJSON(t, h, http.MethodGet, "/api/v1/library?limit=1", "", auth)
	if items, _ := page2["items"].([]any); len(items) != 1 {
		t.Fatalf("limit=1 items = %d", len(items))
	}
	if nc, _ := page2["next_cursor"].(string); nc == "" {
		t.Fatal("next_cursor must be set")
	}

	// Status filter with aliasing: planned == plan_to_watch.
	_, filtered := doJSON(t, h, http.MethodGet, "/api/v1/library?status=plan_to_watch", "", auth)
	if items, _ := filtered["items"].([]any); len(items) != 0 {
		t.Fatalf("watching rows must not match plan filter: %v", items)
	}
}

func TestLibraryBindAndHistoryFlow(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()
	auth := authHeader(t, h)

	bindBody := `{
		"title": "Bound Anime",
		"artwork": {"poster": "https://img.example/bound.jpg"},
		"sources": [
			{"source_id": "fake", "source_url": "https://fake.example/bound"},
			{"source_id": "other", "source_url": "https://other.example/bound"}
		],
		"shikimori_id": 4242,
		"shikimori_title": "Bound Shiki Title",
		"shikimori_status": "watching"
	}`
	rec, payload := doJSON(t, h, http.MethodPost, "/api/v1/library/bind", bindBody, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind = %d: %v", rec.Code, payload)
	}
	if payload["title"] != "Bound Anime" {
		t.Fatalf("bind echo = %v", payload["title"])
	}
	if payload["shikimori_id"] != float64(4242) {
		t.Fatalf("shikimori_id = %v", payload["shikimori_id"])
	}
	sources, _ := payload["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources = %v", sources)
	}
	artwork, _ := payload["artwork"].(map[string]any)
	if artwork["poster"] != "https://img.example/bound.jpg" {
		t.Fatalf("artwork poster = %v", artwork["poster"])
	}
	if artwork["artwork_version"] == "" {
		t.Fatal("artwork cache hints missing")
	}
	boundID := int64(payload["id"].(float64))

	// Empty sources -> 422.
	rec, payload = doJSON(t, h, http.MethodPost, "/api/v1/library/bind", `{"title":"x","sources":[]}`, auth)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty sources = %d, want 422", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["message"] != "Sources must not be empty" {
		t.Fatalf("message = %v", errObj["message"])
	}

	// GET /history finds the bound row; episodes route binds to the
	// fake provider.
	_, payload = doJSON(t, h, http.MethodGet, fmt.Sprintf("/api/v1/history/%d", boundID), "", auth)
	if payload["id"] != float64(boundID) {
		t.Fatalf("history one = %v", payload["id"])
	}

	rec, _ = doJSON(t, h, http.MethodGet, "/api/v1/history/999999", "", auth)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing history = %d, want 404", rec.Code)
	}

	// PATCH history status.
	rec, payload = doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/history/%d", boundID),
		`{"status":"completed","score":9}`, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d: %v", rec.Code, payload)
	}
	if payload["shikimori_status"] != "completed" || payload["score"] != float64(9) {
		t.Fatalf("patched fields = %v %v", payload["shikimori_status"], payload["score"])
	}

	// PATCH on a missing id must 404 like python (get_by_id → not_found),
	// not 500.
	rec, payload = doJSON(t, h, http.MethodPatch, "/api/v1/history/999999", `{"status":"watching"}`, auth)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing history = %d, want 404: %v", rec.Code, payload)
	}
	errObj, _ = payload["error"].(map[string]any)
	if errObj["code"] != "not_found" || errObj["message"] != "History item not found" {
		t.Fatalf("patch missing history error = %v", errObj)
	}

	// Progress PATCH then GET.
	rec, payload = doJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/v1/history/%d/progress", boundID),
		`{"episode":"2","position_sec":120,"duration_sec":1440,"video_key":"Dub","quality":1080}`, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("progress patch = %d: %v", rec.Code, payload)
	}
	if payload["position_sec"] != float64(120) {
		t.Fatalf("progress = %v", payload)
	}
	// python returns the flat progress dict incl. top-level anime_id.
	if payload["anime_id"] != float64(boundID) {
		t.Fatalf("progress patch anime_id = %v, want %d", payload["anime_id"], boundID)
	}
	_, payload = doJSON(t, h, http.MethodGet, fmt.Sprintf("/api/v1/history/%d/progress?episode=2", boundID), "", auth)
	progress, _ := payload["progress"].(map[string]any)
	if progress == nil || progress["episode"] != "2" {
		t.Fatalf("progress get = %v", payload)
	}
	// Missing episode -> progress null.
	_, payload = doJSON(t, h, http.MethodGet, fmt.Sprintf("/api/v1/history/%d/progress?episode=99", boundID), "", auth)
	if p, _ := payload["progress"].(map[string]any); p != nil {
		t.Fatalf("unknown episode progress = %v", p)
	}

	// source_not_bound: strip sources.
	if err := app.store.Sources.ReplaceForAnime(context.Background(), boundID, nil); err != nil {
		t.Fatalf("clear sources: %v", err)
	}
	rec, payload = doJSON(t, h, http.MethodGet, fmt.Sprintf("/api/v1/history/%d/episodes", boundID), "", auth)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no binding = %d, want 404", rec.Code)
	}
	errObj, _ = payload["error"].(map[string]any)
	if errObj["code"] != "source_not_bound" {
		t.Fatalf("code = %v, want source_not_bound", errObj["code"])
	}
}

func TestProvidersRoute(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/providers", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers = %d", rec.Code)
	}
	items, _ := payload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", payload)
	}
	item, _ := items[0].(map[string]any)
	if item["source_id"] != "fake" || item["source_type"] != "both" {
		t.Fatalf("item = %v", item)
	}

	_, payload = doJSON(t, h, http.MethodGet, "/api/v1/providers", "", auth)
	if cached, _ := payload["cached"].(bool); !cached {
		t.Fatal("second providers call must be cached")
	}
}

func TestReleasesCalendar(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	app.shiki = &fakeShiki{
		authed: true,
		rates: []shikimori.UserRate{
			{ID: 1, TargetID: 100, TargetType: "Anime", Status: "watching"},
			{ID: 2, TargetID: 300, TargetType: "Anime", Status: "planned"},
		},
		animes: []shikimori.Anime{
			{ID: 100, Name: "Soon", Russian: "Скоро", Status: "ongoing", NextEpisode: 7,
				NextEpisodeAt: now.Add(36 * time.Hour).Format(time.RFC3339),
				Image:         shikimori.Image{Original: "/system/animes/original/100.jpg"}},
			{ID: 200, Name: "Far", NextEpisodeAt: now.Add(30 * 24 * time.Hour).Format(time.RFC3339)},
			{ID: 300, Name: "Sooner", Status: "ongoing", NextEpisode: 3,
				NextEpisodeAt: now.Add(12 * time.Hour).Format(time.RFC3339)},
		},
	}
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/releases/calendar?days=7", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("calendar = %d: %v", rec.Code, payload)
	}
	if payload["days"] != float64(7) {
		t.Fatalf("days = %v", payload["days"])
	}
	items, _ := payload["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (36h and 12h in, 30d out): %v", len(items), payload)
	}
	// python sorts events by release_at ascending
	// (release_calendar_service.py events.sort(key=lambda item:
	// item.release_at)) — the 12h event must lead even though its rate
	// was listed after the 36h one.
	if items[0].(map[string]any)["shikimori_id"] != float64(300) ||
		items[1].(map[string]any)["shikimori_id"] != float64(100) {
		t.Fatalf("calendar must sort by release_at ascending: %v", items)
	}
	firstEvent, _ := items[0].(map[string]any)
	if firstEvent["user_status"] != "planned" || firstEvent["next_episode"] != float64(3) {
		t.Fatalf("first event = %v", firstEvent)
	}
	event, _ := items[1].(map[string]any)
	if event["shikimori_id"] != float64(100) || event["next_episode"] != float64(7) {
		t.Fatalf("event = %v", event)
	}
	if event["user_status"] != "watching" || event["anime_status"] != "ongoing" {
		t.Fatalf("statuses = %v", event)
	}
	if event["release_at"] == "" {
		t.Fatal("release_at must be set")
	}
	artwork, _ := event["artwork"].(map[string]any)
	if artwork["poster"] == "" {
		t.Fatal("event artwork poster missing")
	}

	// Unauthenticated client -> empty items.
	app2 := newTestApp(t)
	app2.shiki = &fakeShiki{authed: false}
	h2 := app2.Router()
	auth2 := authHeader(t, h2)
	_, payload = doJSON(t, h2, http.MethodGet, "/api/v1/releases/calendar", "", auth2)
	if items, _ := payload["items"].([]any); len(items) != 0 {
		t.Fatalf("unauth calendar = %v", items)
	}
}

func TestShikimoriAnimePage(t *testing.T) {
	app := newTestApp(t)
	app.shiki = &fakeShiki{}
	h := app.Router()
	auth := authHeader(t, h)

	rec, payload := doJSON(t, h, http.MethodGet, "/api/v1/shikimori/anime/123/page", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("page = %d: %v", rec.Code, payload)
	}
	anime, _ := payload["anime"].(map[string]any)
	if anime["id"] != float64(123) {
		t.Fatalf("anime = %v", anime)
	}
	if _, ok := payload["artwork"].(map[string]any); !ok {
		t.Fatal("artwork missing")
	}
	if cached, _ := payload["cached"].(bool); cached {
		t.Fatal("first fetch must not be cached")
	}

	// nil shiki -> 404 not_found.
	app2 := newTestApp(t)
	h2 := app2.Router()
	auth2 := authHeader(t, h2)
	rec, payload = doJSON(t, h2, http.MethodGet, "/api/v1/shikimori/anime/123/page", "", auth2)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no shiki = %d, want 404", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestUnknownRouteUnifiedError(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	rec, _ := doJSON(t, h, http.MethodGet, "/api/v1/nope", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown api route without token = %d, want 401 (guard first)", rec.Code)
	}
	rec, payload := doJSON(t, h, http.MethodGet, "/definitely/not/api", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-api 404 = %d", rec.Code)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Fatalf("code = %v", errObj["code"])
	}
}

func TestTraceIDEchoedFromHeader(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()

	rec, _ := doJSON(t, h, http.MethodGet, "/api/v1/health", "", map[string]string{"X-Trace-Id": "my-trace-42"})
	if got := rec.Header().Get("X-Trace-Id"); got != "my-trace-42" {
		t.Fatalf("X-Trace-Id echoed = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestPerUserCacheScoping(t *testing.T) {
	app := newTestApp(t)
	// Distinct autocomplete per call would need per-session shiki; here
	// verify the cache scope key derives from the session identity by
	// observing that two sessions with different logins do not share
	// cached search results.
	app.cfg.Web.Users["bob"] = app.cfg.Web.Users["alice"]
	app.shiki = &fakeShiki{}
	h := app.Router()

	_, p1 := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", loginBody(testPass, ""), nil)
	tok1, _ := p1["access_token"].(string)
	_, p2 := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", `{"login":"bob","password":"correct horse battery staple"}`, nil)
	tok2, _ := p2["access_token"].(string)

	// Alice has shiki creds? No — but her scope is "alice", bob's "bob".
	// The search cache itself is query-scoped in python (not
	// user-scoped); the snapshot cache is user-scoped. Assert the scope
	// helper directly.
	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	req1 = req1.WithContext(context.WithValue(req1.Context(), ctxTraceID, "t"))
	req1.Header.Set("Authorization", "Bearer "+tok1)
	// login created session without shiki: scope must fall back to login.
	claims1 := decodeForTest(t, tok1)
	sess1, err := app.store.AuthSessions.GetBySessionID(context.Background(), claims1.SessionID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	req1 = req1.WithContext(context.WithValue(req1.Context(), ctxAuthSession, sess1))
	if scope := cacheScopeOf(req1); scope != "alice" {
		t.Fatalf("scope = %q, want alice", scope)
	}

	claims2 := decodeForTest(t, tok2)
	sess2, _ := app.store.AuthSessions.GetBySessionID(context.Background(), claims2.SessionID)
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	req2 = req2.WithContext(context.WithValue(req2.Context(), ctxAuthSession, sess2))
	if scope := cacheScopeOf(req2); scope != "bob" {
		t.Fatalf("scope = %q, want bob", scope)
	}
}
