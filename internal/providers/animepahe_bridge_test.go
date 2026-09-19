package providers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestAnimePaheDownloadLinks runs the download-menu parser over the
// verbatim live capture: the #pickDownload anchors carry the pahe.win
// interstitial URLs with the quality embedded in the anchor text
// ("OZC · 720p (70MB)"). PR71: the kwik /e/ embeds are hard WAF-blocked
// (re-verified live 2026-09-19, Pro included, both routes), so the
// download interstitials are the stream fallback chain.
func TestAnimePaheDownloadLinks(t *testing.T) {
	t.Parallel()

	links := animePaheDownloadLinks(fixture(t, "animepahe_play.html"))
	if len(links) != 2 {
		t.Fatalf("links = %v, want exactly the two interstitial qualities", links)
	}
	if links["720"] != "https://pahe.win/wExwm" {
		t.Errorf("720 = %q, want https://pahe.win/wExwm", links["720"])
	}
	if links["1080"] != "https://pahe.win/oJZfw" {
		t.Errorf("1080 = %q, want https://pahe.win/oJZfw", links["1080"])
	}
}

// Anchors without a resolvable quality token in their text are skipped:
// the menu must stay keyed by quality or the stream map is garbage.
func TestAnimePaheDownloadLinksSkipsUnparseable(t *testing.T) {
	t.Parallel()

	body := []byte(`<div class="dropdown-menu" id="pickDownload">` +
		`<a href="https://pahe.win/noQuality" class="dropdown-item">no token here</a>` +
		`<a href="https://pahe.win/hd" class="dropdown-item">OZC · 1080p (120MB)</a>` +
		`<a href="" class="dropdown-item">OZC · 360p</a>` +
		`</div>` +
		// Other dropdowns must not leak in (#resolutionMenu anchors carry
		// similar dropdown-item markup).
		`<div class="dropdown-menu" id="resolutionMenu">` +
		`<a href="https://pahe.win/leak" class="dropdown-item">OZC · 480p</a></div>`)

	links := animePaheDownloadLinks(body)
	if len(links) != 1 {
		t.Fatalf("links = %v, want exactly the one resolvable interstitial", links)
	}
	if links["1080"] != "https://pahe.win/hd" {
		t.Errorf("1080 = %q, want https://pahe.win/hd", links["1080"])
	}
}

// TestAnimePaheKwikFormMissing's sibling parse: the pahe.win
// interstitial exposes the kwik FILE page through its "Redirect me"
// anchor (live capture 2026-09-19: pahe.win/wExwm → kwik.cx/f/4piNxBJf1Qdx).
func TestAnimePaheInterstitialTarget(t *testing.T) {
	t.Parallel()

	body := []byte(`<html><body><nav><a href="https://pahe.win/">Home</a></nav>` +
		`<h2>AnimePahe_Black_Lagoon_-_01_BD_720p_OZC.mp4</h2>` +
		`<a href="https://kwik.cx/f/4piNxBJf1Qdx">Redirect me</a>` +
		`<a href="https://pahe.win/developers">API</a></body></html>`)

	got := animePaheInterstitialTarget(body)
	if got != "https://kwik.cx/f/4piNxBJf1Qdx" {
		t.Fatalf("target = %q, want the kwik file URL", got)
	}

	if got := animePaheInterstitialTarget([]byte("<html>challenge page</html>")); got != "" {
		t.Fatalf("target = %q, want empty on a page without a kwik link", got)
	}
}

// TestAnimePaheKwikForm pins the kwik /f/ file-page form parse: a plain
// (unpacked) Laravel form posting to /d/<id> with a hidden _token
// (live capture 2026-09-19: kwik.cx/f/4piNxBJf1Qdx).
func TestAnimePaheKwikForm(t *testing.T) {
	t.Parallel()

	body := []byte(`<html><body>` +
		// A decoy value= attribute BEFORE the form must not confuse the parse.
		`<input type="hidden" name="other" value="decoy"/>` +
		`<form method="post" action="https://kwik.cx/d/4piNxBJf1Qdx">` +
		`<input type="hidden" name="_token" value="tok-abc123"/>` +
		`<button type="submit" class="button">Download (70.35 MB)</button></form>` +
		`</body></html>`)

	action, token, ok := animePaheKwikForm(body)
	if !ok {
		t.Fatal("ok = false, want the /d/ form parsed")
	}
	if action != "https://kwik.cx/d/4piNxBJf1Qdx" {
		t.Errorf("action = %q", action)
	}
	if token != "tok-abc123" {
		t.Errorf("token = %q, want tok-abc123", token)
	}
}

// A page without the /d/ form (challenge interstitial, shape drift) is
// reported as not-ok, never as an empty action.
func TestAnimePaheKwikFormMissing(t *testing.T) {
	t.Parallel()

	t.Run("challenge page", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := animePaheKwikForm([]byte("<html><body>Just a moment...</body></html>")); ok {
			t.Fatal("ok = true, want false on a challenge page")
		}
	})

	t.Run("form without token", func(t *testing.T) {
		t.Parallel()
		body := []byte(`<form method="post" action="https://kwik.cx/d/x"></form>`)
		if _, _, ok := animePaheKwikForm(body); ok {
			t.Fatal("ok = true, want false when the _token input is missing")
		}
	})
}

// fakePaheBrowser scripts the bridge seam: per-URL bodies, per-(page,
// action) media results, and a call log the assertions lean on.
type fakePaheBrowser struct {
	fetches     map[string][]byte
	htmls       map[string][]byte
	media       map[string]string // pageURL+"\x00"+action → media URL
	fetchErrs   map[string]error
	submitErr   error
	fetchCalls  []string
	htmlCalls   []string
	submitCalls []string
}

func (f *fakePaheBrowser) PageFetch(_ context.Context, url string) ([]byte, error) {
	f.fetchCalls = append(f.fetchCalls, url)
	if err := f.fetchErrs[url]; err != nil {
		return nil, err
	}
	if body, ok := f.fetches[url]; ok {
		return body, nil
	}
	return nil, fmt.Errorf("in-page fetch %s: status 404", url)
}

func (f *fakePaheBrowser) PageHTML(_ context.Context, url string) ([]byte, error) {
	f.htmlCalls = append(f.htmlCalls, url)
	if body, ok := f.htmls[url]; ok {
		return body, nil
	}
	return nil, fmt.Errorf("navigate %s: page load error", url)
}

func (f *fakePaheBrowser) SubmitDownload(_ context.Context, pageURL, action string) (string, error) {
	f.submitCalls = append(f.submitCalls, pageURL+"|"+action)
	if f.submitErr != nil {
		return "", f.submitErr
	}
	if media, ok := f.media[pageURL+"\x00"+action]; ok {
		return media, nil
	}
	return "", fmt.Errorf("no scripted media for %s", pageURL)
}

// bridgeProvider builds the provider on the fake bridge with the same
// fixture-server pattern the netclient tests use.
func bridgeProvider(t *testing.T, srvURL string, browser paheBrowser) *AnimePahe {
	t.Helper()
	p := newAnimePahe(srvURL, testClient(t, "animepahe"), browser)
	return p
}

// TestAnimePaheBridgeSearch pins the transport swap: with a bridge, the
// search runs as ONE in-page fetch of the absolute API URL (site
// cookies + real fingerprint), the netclient path stays untouched.
func TestAnimePaheBridgeSearch(t *testing.T) {
	t.Parallel()

	fake := &fakePaheBrowser{fetches: map[string][]byte{
		AnimePaheBase + "/api?" + (url.Values{"m": {"search"}, "q": {"black lagoon"}}).Encode(): fixture(t, "animepahe_search.json"),
	}}
	p := bridgeProvider(t, AnimePaheBase, fake)

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 8 || results[0].Title != "Black Lagoon" {
		t.Fatalf("results = %d/%q, want 8/Black Lagoon", len(results), results[0].Title)
	}
	if len(fake.fetchCalls) != 1 || len(fake.htmlCalls) != 0 {
		t.Fatalf("bridge calls = fetch %v html %v, want exactly one fetch", fake.fetchCalls, fake.htmlCalls)
	}
}

// TestAnimePaheBridgeGetEpisodes pins the release walk on the bridge:
// one in-page fetch per page, last_page bounds the loop.
func TestAnimePaheBridgeGetEpisodes(t *testing.T) {
	t.Parallel()

	p1, err := clampReleaseLastPage(fixture(t, "animepahe_episodes_p1.json"), 2)
	if err != nil {
		t.Fatalf("clamp p1: %v", err)
	}
	const animeSession = "76d59a16-e57d-4ad1-7ec6-e88f0fe9469b"
	fake := &fakePaheBrowser{fetches: map[string][]byte{}}
	fetchURL := func(page int) string {
		return AnimePaheBase + "/api?" + (url.Values{
			"m": {"release"}, "id": {animeSession}, "sort": {"episode_asc"}, "page": {fmt.Sprint(page)},
		}).Encode()
	}
	fake.fetches[fetchURL(1)] = p1
	fake.fetches[fetchURL(2)] = fixture(t, "animepahe_episodes_p2.json")
	p := bridgeProvider(t, AnimePaheBase, fake)

	episodes, err := p.GetEpisodes(context.Background(), animeSession)
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(episodes) != 61 {
		t.Fatalf("episodes = %d, want 61 across both pages", len(episodes))
	}
	if len(fake.fetchCalls) != 2 {
		t.Fatalf("fetches = %v, want one per page", fake.fetchCalls)
	}
}

// TestAnimePaheResolveStreamBridgeInterstitial is the PR71 chain:
// embeds (attempt a) wall against kwik's WAF, the interstitial chain
// (attempt b) carries the resolve: play page → #pickDownload → pahe.win
// → kwik /f/ form → captured download URL.
func TestAnimePaheResolveStreamBridgeInterstitial(t *testing.T) {
	t.Parallel()

	const (
		interstitial720  = "https://pahe.win/wExwm"
		interstitial1080 = "https://pahe.win/oJZfw"
		kwikFile720      = "https://kwik.cx/f/4piNxBJf1Qdx"
		kwikFile1080     = "https://kwik.cx/f/OTHER1080"
		media720         = "https://s99.file.nextcdn.org/download/01/720.mp4"
		media1080        = "https://s99.file.nextcdn.org/download/01/1080.mp4"
	)
	playBody := fixture(t, "animepahe_play.html")
	fake := &fakePaheBrowser{
		fetches: map[string][]byte{},
		htmls: map[string][]byte{
			interstitial720:  []byte(`<html><body><a href="` + kwikFile720 + `">Redirect me</a></body></html>`),
			interstitial1080: []byte(`<html><body><a href="` + kwikFile1080 + `">Redirect me</a></body></html>`),
			kwikFile720: []byte(`<form method="post" action="https://kwik.cx/d/4piNxBJf1Qdx">` +
				`<input type="hidden" name="_token" value="tok1"/></form>`),
			kwikFile1080: []byte(`<form method="post" action="https://kwik.cx/d/OTHER1080">` +
				`<input type="hidden" name="_token" value="tok2"/></form>`),
		},
		media: map[string]string{
			kwikFile720 + "\x00https://kwik.cx/d/4piNxBJf1Qdx": media720,
			kwikFile1080 + "\x00https://kwik.cx/d/OTHER1080":   media1080,
		},
	}

	// The play page rides the bridge; the /e/ embed URL is a dead
	// listener so attempt (a) fails fast and typed.
	dead := "http://" + newDeadListener(t).Addr().String() + "/kwik/e/abc"
	fake.fetches[AnimePaheBase+"/play/s/e"] = []byte(
		strings.Replace(string(playBody), "https://kwik.cx/e/zYMpempjBVFT", dead, 1))

	p := bridgeProvider(t, AnimePaheBase, fake)
	episode := contracts.Episode{
		Num:       "1",
		RawID:     "s|e",
		RawEmbeds: map[string][]string{"Original (Pahe)": {AnimePaheBase + "/play/s/e"}},
	}

	stream, err := p.ResolveStream(context.Background(), episode, "Original (Pahe)")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if got := stream.Links["720"].URL; got != media720 {
		t.Errorf("720 URL = %q, want the captured media URL", got)
	}
	if got := stream.Links["1080"].URL; got != media1080 {
		t.Errorf("1080 URL = %q, want the captured media URL", got)
	}
	if stream.Links["720"].Type != "mp4" {
		t.Errorf("720 type = %q, want mp4", stream.Links["720"].Type)
	}
	// Chain shape per quality: interstitial navigation, kwik file
	// navigation, one submission.
	if len(fake.htmlCalls) != 4 || len(fake.submitCalls) != 2 {
		t.Errorf("html calls = %v submits = %v, want 4 navigations and 2 submits", fake.htmlCalls, fake.submitCalls)
	}
}

// When both attempts wall (embeds fail AND the play page carries no
// download menu) the resolve is a typed provider error naming both.
func TestAnimePaheResolveStreamBridgeBothWall(t *testing.T) {
	t.Parallel()

	dead := "http://" + newDeadListener(t).Addr().String() + "/kwik/e/abc"
	fake := &fakePaheBrowser{fetches: map[string][]byte{
		AnimePaheBase + "/play/s/e": []byte(`<html><body><p>no menus at all</p></body></html>`),
	}}
	p := bridgeProvider(t, AnimePaheBase, fake)
	episode := contracts.Episode{
		RawID:     "s|e",
		RawEmbeds: map[string][]string{"Original (Pahe)": {dead}},
	}

	// The embeds[0] URL doubles as the play page under the bridge; use
	// the canonical play path so the fake matches.
	episode.RawEmbeds["Original (Pahe)"] = []string{AnimePaheBase + "/play/s/e"}
	_, err := p.ResolveStream(context.Background(), episode, "Original (Pahe)")
	var pe *contracts.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a *contracts.ProviderError", err)
	}
	if pe.Op != contracts.OpResolveStream {
		t.Errorf("op = %q, want %q", pe.Op, contracts.OpResolveStream)
	}
}

// buildPaheBridge gating: no manager or no solver (the [cf]-disabled
// world) leaves the bridge nil — the provider stays on the netclient
// ladder.
func TestBuildPaheBridge(t *testing.T) {
	t.Parallel()

	if buildPaheBridge(nil) != nil {
		t.Fatal("buildPaheBridge(nil) != nil, want nil")
	}
	if buildPaheBridge(&cfbrowser.Manager{}) != nil {
		t.Fatal("buildPaheBridge(manager without solver) != nil, want nil")
	}
}

// TestJSStringEscapes pins the bridge's JS string-literal escaping:
// backslash, quote, newlines, tab and NUL must survive the Go→JS
// handoff byte-exactly (URLs/keys are interpolated through this).
func TestJSStringEscapes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`plain`:         `'plain'`,
		`back\slash`:    `'back\\slash'`,
		`quo'te`:        `'quo\'te'`,
		"new\nline":     `'new\nline'`,
		"carriage\rret": `'carriage\rret'`,
		"tab\there":     `'tab\there'`,
		"nul\x00byte":   `'nul\u0000byte'`,
	}
	for in, want := range cases {
		if got := jsString(in); got != want {
			t.Errorf("jsString(%q) = %s, want %s", in, got, want)
		}
	}
}
