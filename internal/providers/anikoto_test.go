package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// anikototv.to is NOT a Python-tree port: the provider was written
// against the live site characterized 2026-09-25 (PR104). The site is
// a HiAnime/Zoro-style clone (the same platform family as anikoto.net
// — the AniVault-Scraper and PyPI anikoto downloader both document the
// identical AJAX shape): the search is the /filter?keyword= HTML page,
// the AJAX endpoints answer the {"status":N,"result":...} envelope
// (episodes and server lists carry HTML in result, the stream resolver
// a JSON object), and every embed lands on megaplay.buzz whose e1
// player decrypts an AES-256-CBC `enc` sources blob and HMAC-signs the
// resulting CDN URL. All fixtures below are REAL captures of
// 2026-09-25 (provenance in testdata/anikoto_PROVENANCE.md); the
// resolution-chain tests serve them from a local mux with the
// megaplay origin rewritten to the test server so the whole chain
// stays offline.

// akTestServer builds a dedicated mux server (NOT the shared
// fixtureServer recorder — the resolution chain issues several
// requests and the eager dub hydration runs parallel fetches, both
// race-unsafe on the single-slot recorder) answering the anikoto
// fixtures. Fixture bodies have their megaplay origin rewritten to the
// test server so the embed chain never leaves the loopback.
type akTestServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []akRecordedRequest
}

type akRecordedRequest struct {
	Method string
	Path   string
	Query  string
	Header http.Header
}

func newAkTestServer(t *testing.T) *akTestServer {
	t.Helper()
	s := &akTestServer{}
	mux := http.NewServeMux()

	serve := func(name string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			s.record(r)
			// Rewrite the megaplay origin onto the test server in both
			// the plain (HTML, JS) and JSON-escaped (\/) forms so the
			// whole resolution chain stays on the loopback.
			body := string(fixture(t, name))
			body = strings.ReplaceAll(body, "https://megaplay.buzz", s.URL)
			body = strings.ReplaceAll(body, `https:\/\/megaplay.buzz`, strings.ReplaceAll(s.URL, "/", `\/`))
			_, _ = w.Write([]byte(body))
		}
	}

	mux.HandleFunc("/filter", serve("anikoto_search.html"))
	mux.HandleFunc("/watch/", serve("anikoto_watch.html"))
	mux.HandleFunc("/ajax/episode/list/", serve("anikoto_episodes.json"))
	mux.HandleFunc("/ajax/server/list", serve("anikoto_servers.json"))
	mux.HandleFunc("/ajax/server", serve("anikoto_stream.json"))
	mux.HandleFunc("/stream/s-2/5731/sub", serve("anikoto_megaplay.html"))
	mux.HandleFunc("/lib/e1-player.min.js", serve("anikoto_e1player.js"))
	mux.HandleFunc("/stream/getSourcesNew", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		// The captured enc blob decrypts to the LIVE CDN URL, which
		// cannot be rewritten (it only exists under AES). The handler
		// therefore re-encrypts the real capture's payload — same
		// AES-256-CBC scheme, keyed by the REAL unpacked player
		// parameters — with the file URL pointed at the loopback; the
		// provider's decrypt/sign legs exercise real crypto either way.
		var payload map[string]any
		if err := json.Unmarshal(fixture(t, "anikoto_getsources.json"), &payload); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		file := s.URL + "/anime/577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a/master.m3u8"
		payload["enc"] = akTestEncBlob(t, file)
		out, err := json.Marshal(payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/anime/577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a/master.m3u8", serve("anikoto_master.m3u8"))

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *akTestServer) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, akRecordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
	})
}

func (s *akTestServer) last() akRecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

func (s *akTestServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func TestAniKotoSearch(t *testing.T) {
	t.Parallel()

	srv := newAkTestServer(t)
	p := newAniKoto(srv.URL, testClient(t, "anikoto"))

	results, err := p.Search(context.Background(), "black lagoon")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	req := srv.last()
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.Path != "/filter" {
		t.Errorf("path = %q, want /filter", req.Path)
	}
	if !strings.Contains(req.Query, "keyword=black+lagoon") {
		t.Errorf("query = %q, want keyword=black+lagoon", req.Query)
	}

	// The real capture carries 30 result cards on page 1.
	if len(results) != 30 {
		t.Fatalf("results = %d, want 30 fixture cards", len(results))
	}
	first := results[0]
	if first.Title != "Black Lagoon: The Second Barrage" {
		t.Errorf("Title = %q, want the d-title text", first.Title)
	}
	// The card links point at /watch/{slug}/ep-1; the provider
	// canonicalizes onto the bare watch slug so GetEpisodes can fetch
	// it directly.
	if first.URL != AniKotoBase+"/watch/black-lagoon-the-second-barrage-omdia" {
		t.Errorf("URL = %q, want the ep-suffix-stripped watch slug", first.URL)
	}
	if first.SourceID != "anikoto" {
		t.Errorf("SourceID = %q", first.SourceID)
	}
	if first.Poster != "https://cdn.anipixcdn.co/thumbnail/577bcc914f9e55d5e4e4f82f9f00e7d4.jpg" {
		t.Errorf("Poster = %q, want the fixture img src", first.Poster)
	}
}

func TestAniKotoSearchMiss(t *testing.T) {
	t.Parallel()

	// A junk query renders the filter shell with zero #list-items
	// items — an empty result list, not an error.
	mux := http.NewServeMux()
	mux.HandleFunc("/filter", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><div id="list-items" class="ani items"></div></body></html>`))
	})
	p := newAniKoto(muxHost(t, mux), testClient(t, "anikoto"))

	results, err := p.Search(context.Background(), "zzzz")
	if err != nil {
		t.Fatalf("Search on empty page: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0", len(results))
	}
}

func TestAniKotoGetEpisodes(t *testing.T) {
	t.Parallel()

	srv := newAkTestServer(t)
	p := newAniKoto(srv.URL, testClient(t, "anikoto"))

	episodes, err := p.GetEpisodes(context.Background(), srv.URL+"/watch/black-lagoon-the-second-barrage-omdia/ep-1")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}

	// The watch page was fetched (the search-result URL with its ep
	// suffix is accepted verbatim).
	foundWatch := false
	for _, req := range srv.reqs {
		if strings.HasPrefix(req.Path, "/watch/") {
			foundWatch = true
		}
	}
	if !foundWatch {
		t.Fatal("the watch page was never fetched")
	}

	// The AJAX episode list request: X-Requested-With + the anime id
	// from the watch page (1359 in the fixture).
	var epsReq *akRecordedRequest
	for i := range srv.reqs {
		if strings.HasPrefix(srv.reqs[i].Path, "/ajax/episode/list/") {
			epsReq = &srv.reqs[i]
		}
	}
	if epsReq == nil {
		t.Fatal("the ajax episode list was never fetched")
	}
	if epsReq.Path != "/ajax/episode/list/1359" {
		t.Errorf("episodes path = %q, want /ajax/episode/list/1359 (the watch-page data-id)", epsReq.Path)
	}
	if epsReq.Header.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q, want XMLHttpRequest", epsReq.Header.Get("X-Requested-With"))
	}

	// The fixture carries the real 12-episode Second Barrage list.
	if len(episodes) != 12 {
		t.Fatalf("episodes = %d, want 12", len(episodes))
	}
	if episodes[0].Num != "1" {
		t.Errorf("Num = %q, want 1", episodes[0].Num)
	}
	// RawID composes "{ep data-id}:{data-ids}" — both halves present.
	if !strings.Contains(episodes[0].RawID, "23918:") {
		t.Errorf("RawID = %q, want the ep data-id prefix 23918:", episodes[0].RawID)
	}
	// The dub list hydrates lazily (DubsHydrator): episodes arrive
	// with empty embeds.
	if len(episodes[0].RawEmbeds) != 0 {
		t.Errorf("RawEmbeds = %v, want empty (lazy hydration)", episodes[0].RawEmbeds)
	}
}

func TestAniKotoFetchDubs(t *testing.T) {
	t.Parallel()

	srv := newAkTestServer(t)
	p := newAniKoto(srv.URL, testClient(t, "anikoto"))

	ep := contracts.Episode{Num: "1", RawID: akFixtureEpisodeRawID(t)}
	hydrated, err := p.FetchDubs(context.Background(), &ep)
	if err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	req := srv.last()
	if req.Path != "/ajax/server/list" {
		t.Errorf("path = %q, want /ajax/server/list", req.Path)
	}
	if req.Header.Get("X-Requested-With") != "XMLHttpRequest" {
		t.Errorf("X-Requested-With = %q", req.Header.Get("X-Requested-With"))
	}

	// The site's own audio axis: one SUB group and one DUB group, each
	// with the fixture's four servers (Vidstream-2, Vidstream-1 beta,
	// HD-1, HD-2) — the values are the per-server link-ids
	// ResolveStream consumes.
	for _, dub := range []string{"SUB", "DUB"} {
		links, ok := hydrated.RawEmbeds[dub]
		if !ok {
			t.Fatalf("RawEmbeds missing %q: %v", dub, hydrated.RawEmbeds)
		}
		if len(links) != 4 {
			t.Errorf("RawEmbeds[%q] = %d links, want 4 servers", dub, len(links))
		}
	}
}

func TestAniKotoFetchDubsBadRawID(t *testing.T) {
	t.Parallel()

	p := newAniKoto(AniKotoBase, testClient(t, "anikoto"))
	_, err := p.FetchDubs(context.Background(), &contracts.Episode{Num: "1", RawID: "no-colon-here"})
	if err == nil || !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

func TestAniKotoResolveStream(t *testing.T) {
	t.Parallel()

	srv := newAkTestServer(t)
	p := newAniKoto(srv.URL, testClient(t, "anikoto"))

	ep := contracts.Episode{Num: "1", RawID: akFixtureEpisodeRawID(t)}
	if _, err := p.FetchDubs(context.Background(), &ep); err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	stream, err := p.ResolveStream(context.Background(), ep, "SUB")
	if err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}

	// The full chain walked: server list → stream resolver → megaplay
	// page → player script → getSourcesNew → master playlist.
	paths := make([]string, 0, srv.count())
	for _, req := range srv.reqs {
		paths = append(paths, req.Path)
	}
	for _, want := range []string{
		"/ajax/server/list",
		"/ajax/server",
		"/stream/s-2/5731/sub",
		"/lib/e1-player.min.js",
		"/stream/getSourcesNew",
		"/anime/577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a/master.m3u8",
	} {
		if !akContainsPath(paths, want) {
			t.Errorf("chain missing %s; walked %v", want, paths)
		}
	}

	// The AJAX stream resolver and the getSources hop are XHR calls.
	for i := range srv.reqs {
		switch srv.reqs[i].Path {
		case "/ajax/server", "/stream/getSourcesNew":
			if got := srv.reqs[i].Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
				t.Errorf("%s X-Requested-With = %q", srv.reqs[i].Path, got)
			}
		}
	}

	// The megaplay master serves 1080/720/480 variants; every link
	// keeps the megaplay Referer (the CDN checks it on playback).
	if len(stream.Links) != 3 {
		t.Fatalf("links = %v, want 3 quality variants", stream.Links)
	}
	for _, quality := range []string{"1080", "720", "480"} {
		src, ok := stream.Links[quality]
		if !ok {
			t.Errorf("links missing quality %q", quality)
			continue
		}
		if src.Quality != quality || src.Type != "m3u8" {
			t.Errorf("links[%q] = %+v", quality, src)
		}
		if src.Headers["Referer"] != srv.URL+"/" {
			t.Errorf("links[%q] Referer = %q, want the megaplay origin", quality, src.Headers["Referer"])
		}
	}
	if stream.DubName != "SUB" {
		t.Errorf("DubName = %q, want SUB", stream.DubName)
	}

	// The manifest request must carry the HMAC token: a signing
	// regression to plain passthrough must fail the chain, not just
	// the unit-level format pin.
	for _, req := range srv.reqs {
		if strings.HasPrefix(req.Path, "/anime/577bcc914f9e55d5e4e4f82f9f00e7d4/") {
			if !strings.Contains(req.Query, "token=") {
				t.Errorf("master.m3u8 query = %q, want the HMAC token", req.Query)
			}
			return
		}
	}
	t.Fatal("the master.m3u8 request was never recorded")
}

func TestAniKotoResolveStreamUnknownDub(t *testing.T) {
	t.Parallel()

	srv := newAkTestServer(t)
	p := newAniKoto(srv.URL, testClient(t, "anikoto"))

	ep := contracts.Episode{Num: "1", RawID: akFixtureEpisodeRawID(t)}
	if _, err := p.FetchDubs(context.Background(), &ep); err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	_, err := p.ResolveStream(context.Background(), ep, "Дубляж")
	if err == nil || !errors.Is(err, contracts.ErrInvalidInput) {
		t.Errorf("err = %v, want ErrInvalidInput", err)
	}
}

func TestAniKotoResolveStreamAllServersDead(t *testing.T) {
	t.Parallel()

	// The AJAX stream resolver answers the site's error envelope for
	// every server: the failure surfaces typed (the no-silent-failure
	// rule), not as an empty success.
	mux := http.NewServeMux()
	mux.HandleFunc("/ajax/server/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anikoto_servers.json"))
	})
	mux.HandleFunc("/ajax/server", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":500,"result":"Bad request"}`))
	})
	p := newAniKoto(muxHost(t, mux), testClient(t, "anikoto"))

	ep := contracts.Episode{Num: "1", RawID: akFixtureEpisodeRawID(t)}
	if _, err := p.FetchDubs(context.Background(), &ep); err != nil {
		t.Fatalf("FetchDubs: %v", err)
	}

	_, err := p.ResolveStream(context.Background(), ep, "SUB")
	if err == nil {
		t.Fatal("err = nil, want the all-servers-dead failure")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("err = %v, want ErrExtractFailed", err)
	}
}

func TestAniKotoAjaxEnvelopeError(t *testing.T) {
	t.Parallel()

	// The live site answers bad AJAX calls with HTTP 200 and the error
	// INSIDE the envelope: {"status":500,"result":"Bad request"} (the
	// controller's own probe of /api/search hit exactly this). The
	// provider surfaces the envelope status, not the HTTP one.
	mux := http.NewServeMux()
	mux.HandleFunc("/watch/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "anikoto_watch.html"))
	})
	mux.HandleFunc("/ajax/episode/list/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":500,"result":"Bad request"}`))
	})
	host := muxHost(t, mux)
	p := newAniKoto(host, testClient(t, "anikoto"))

	_, err := p.GetEpisodes(context.Background(), host+"/watch/x")
	if err == nil {
		t.Fatal("err = nil, want the envelope error")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "Bad request") {
		t.Errorf("err = %v, want the envelope status and message quoted", err)
	}
}

func TestAniKotoUnpackShortPayloadTypedError(t *testing.T) {
	t.Parallel()

	// A hostile/rotated bundle can match all three wrapper regexes yet
	// carry a payload shorter than the known plaintext prefix: the
	// unpack must fail TYPED, never index out of range (the reviewer
	// reproduced a SIGSEGV here on a 2-byte payload).
	script := `let o;return eval("W})(\"ab\")");`
	_, err := akUnpackPlayerStrings(script)
	if err == nil {
		t.Fatal("err = nil, want the short-payload failure")
	}
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Errorf("err = %v, want ErrExtractFailed", err)
	}
	if !strings.Contains(err.Error(), "too short") || !strings.Contains(err.Error(), "2") {
		t.Errorf("err = %v, want the offending length quoted", err)
	}
}

func TestAniKotoPlayerParameters(t *testing.T) {
	t.Parallel()

	// The REAL obfuscated player bundle (97KB capture): the static
	// unpack must recover the exact AES key/IV/secret the live chain
	// used, without ever executing JavaScript.
	script := string(fixture(t, "anikoto_e1player.js"))
	params, err := akPlayerParameters(script)
	if err != nil {
		t.Fatalf("akPlayerParameters: %v", err)
	}
	if params.key != "i?LMTAx0Q6,:}50U" {
		t.Errorf("key = %q, want the live-captured AES key", params.key)
	}
	if params.iv != "W0;27ToaUpl_P%'c" {
		t.Errorf("iv = %q, want the live-captured AES IV", params.iv)
	}
	if params.secret != "MpCdnT0k3n!9f2K#xQ7vL5mR8wN1pY4s" {
		t.Errorf("secret = %q, want the live-captured HMAC secret", params.secret)
	}
	if params.ttl != 90 {
		t.Errorf("ttl = %d, want 90", params.ttl)
	}
}

func TestAniKotoDecryptSource(t *testing.T) {
	t.Parallel()

	// The REAL enc blob of the capture decrypts with the REAL player
	// parameters to the real master-manifest URL.
	script := string(fixture(t, "anikoto_e1player.js"))
	params, err := akPlayerParameters(script)
	if err != nil {
		t.Fatalf("akPlayerParameters: %v", err)
	}

	var payload struct {
		Enc string `json:"enc"`
	}
	if err := json.Unmarshal(fixture(t, "anikoto_getsources.json"), &payload); err != nil {
		t.Fatalf("getsources fixture: %v", err)
	}

	file, err := akDecryptSource(payload.Enc, params.key, params.iv)
	if err != nil {
		t.Fatalf("akDecryptSource: %v", err)
	}
	if file != "https://fetch.nexabloom.top/anime/577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a/master.m3u8" {
		t.Errorf("file = %q, want the live master-manifest URL", file)
	}
}

func TestAniKotoSignedURL(t *testing.T) {
	t.Parallel()

	// The HMAC signing key captured live from the player bundle — a
	// public constant embedded in the site's client script, not a
	// credential.
	const (
		hmacKey = "MpCdnT0k3n!9f2K#xQ7vL5mR8wN1pY4s"
		raw     = "https://fetch.nexabloom.top/anime/577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a/master.m3u8"
	)
	now := time.Unix(1758800000, 0)
	signed := akSignedURL(raw, hmacKey, 90, now)

	// Token shape: b64url("{expires}|{h1}/{h2}").b64url(hmac) appended
	// with the right separator; the message timestamps at now+ttl.
	if !strings.HasPrefix(signed, raw+"?token=") {
		t.Fatalf("signed = %q, want the raw URL + ?token=", signed)
	}
	token := strings.TrimPrefix(signed, raw+"?token=")
	msgB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token = %q, want msg.sig", token)
	}
	if msgB64 != akTokenMessageB64("1758800090|577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a") {
		t.Errorf("token message mismatch for expires=1758800090")
	}
	// The signature runs over the RAW message bytes (the player signs
	// the message, not its base64 form).
	if sigB64 != akTokenSignatureB64(hmacKey, "1758800090|577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a") {
		t.Errorf("token signature mismatch")
	}

	// A URL already carrying a token passes through untouched.
	already := raw + "?token=abc"
	if got := akSignedURL(already, hmacKey, 90, now); got != already {
		t.Errorf("signed = %q, want the pre-tokened URL verbatim", got)
	}
	// A URL without the two 32-hex path segments cannot be signed —
	// returned unchanged (the CDN would reject it; the caller sees the
	// original).
	noHex := "https://fetch.nexabloom.top/anime/plain/master.m3u8"
	if got := akSignedURL(noHex, hmacKey, 90, now); got != noHex {
		t.Errorf("signed = %q, want the un-signable URL verbatim", got)
	}
}

// akTestEncBlob encrypts file the way the live megaplay player does:
// AES-256-CBC over `{"file":…}` with the REAL player parameters
// (unpacked from the captured bundle), PKCS7-padded, URL-safe base64 —
// the exact inverse of akDecryptSource.
func akTestEncBlob(t *testing.T, file string) string {
	t.Helper()
	params, err := akPlayerParameters(string(fixture(t, "anikoto_e1player.js")))
	if err != nil {
		t.Fatalf("player parameters: %v", err)
	}

	plaintext, err := json.Marshal(map[string]string{"file": file})
	if err != nil {
		t.Fatal(err)
	}
	kb := make([]byte, 32)
	copy(kb, params.key)
	ib := make([]byte, 16)
	copy(ib, params.iv)

	pad := 16 - len(plaintext)%16
	padded := make([]byte, len(plaintext)+pad)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(pad)
	}

	block, err := aes.NewCipher(kb)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, ib).CryptBlocks(ct, padded)
	return base64.RawURLEncoding.EncodeToString(ct)
}

func TestAniKotoNamePreference(t *testing.T) {
	t.Parallel()

	p := newAniKoto(AniKotoBase, testClient(t, "anikoto"))
	if got := p.NamePreference(); got != contracts.NamePrefLatin {
		t.Errorf("NamePreference = %v, want NamePrefLatin (EN-only index)", got)
	}
}

// akFixtureEpisodeRawID extracts episode 1's provider RawID from the
// real episodes fixture the same way GetEpisodes composes it.
func akFixtureEpisodeRawID(t *testing.T) string {
	t.Helper()
	var env struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(fixture(t, "anikoto_episodes.json"), &env); err != nil {
		t.Fatalf("episodes fixture: %v", err)
	}
	id, ids, ok := akFirstEpisodeRef(env.Result)
	if !ok {
		t.Fatal("fixture carries no episode anchor")
	}
	return id + ":" + ids
}

func akContainsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// muxHost builds a plain httptest server around mux (the
// race-landmine-free variant for tests not needing the full chain).
func muxHost(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}
