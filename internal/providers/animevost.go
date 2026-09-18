package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	nethttp "net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimeVostBase is the JSON API root (anicli-py anicli/providers/
// animevost.py:17). The 2026-09-18 revival probe confirmed the v1
// methods still answer there — POST /search and POST /playlist —
// alongside the newer /animevost/api/v0.2/GetInfo/<id> the site's own
// frontend calls. The search envelope grew to
// {"state":{...},"data":[...]}; /playlist still returns a bare array.
const AnimeVostBase = "https://api.animevost.org/v1"

// animevostHTTPClient is the provider-scoped DIRECT transport.
//
// Root cause of the PR46 breakage (verified live 2026-09-18):
// api.animevost.org tarpits the shared netclient's Chrome_150 uTLS
// fingerprint host-wide — every endpoint stalls without response
// headers until the watchdog kills the attempt (the smoke's
// "provider timeout"), while non-Chrome fingerprints (Go stdlib,
// curl) answer in ~0.3s. The provider therefore carries its own
// plain-Go client: the site is RU-hosted, so the connection is
// DIRECT (no proxy), the 30s timeout mirrors netclient's
// RequestTimeout, and the UA/Accept-Language headers mirror the
// netclient defaults.
var animevostHTTPClient = &nethttp.Client{Timeout: 30 * time.Second}

// animevostUserAgent and animevostAcceptLanguage mirror the netclient
// defaults (internal/config Default() Network.UserAgent and
// netclient's defaultAcceptLanguage): the API sits behind
// fingerprint-sensitive fronting, so requests keep browser-grade
// headers.
const (
	animevostUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	animevostAcceptLanguage = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"

	// animevostBodyLimit caps one API response; the largest observed
	// live body (search over a broad latin word) is ~100KB.
	animevostBodyLimit = 8 << 20
)

// formContentType marks a request body as form-encoded (the header
// PostForm used to set; kept explicit for Do-based calls).
var formContentType = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

// AnimeVost is the port of anicli-py anicli/providers/animevost.py.
// Source type BOTH, single fixed dub "AnimeVost", mp4 links carried in a
// JSON object raw embed between GetEpisodes and ResolveStream.
type AnimeVost struct {
	Base
}

// newAnimevost builds the provider against baseURL. The shared
// netclient is deliberately refused (unused): its Chrome_150 uTLS
// fingerprint is tarpitted host-wide by api.animevost.org — see
// animevostHTTPClient. The factory signature is kept for the registry
// wiring.
func newAnimevost(baseURL string, _ *netclient.Client) *AnimeVost {
	return &AnimeVost{Base: Base{
		id:          "animevost",
		name:        "AnimeVost",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
	}}
}

// postForm issues one form-encoded POST and returns the body bytes.
// Status mapping mirrors netclient's classification: 403 -> ErrProvider403,
// 404 -> ErrNotFound (carrying the server's miss text when the body
// names it), other non-2xx -> netclient.StatusError; everything is
// wrapped with the provider id and operation tag.
func (p *AnimeVost) postForm(ctx context.Context, op, path string, form url.Values) ([]byte, error) {
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, p.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), op, 0, fmt.Errorf("build request: %w", err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", animevostUserAgent)
	req.Header.Set("Accept-Language", animevostAcceptLanguage)

	resp, err := animevostHTTPClient.Do(req)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), op, 0, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, animevostBodyLimit+1))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), op, 0, fmt.Errorf("read body: %w", err))
	}
	if int64(len(body)) > animevostBodyLimit {
		return nil, contracts.WrapProvider(p.ID(), op, 0, fmt.Errorf("response body exceeds limit: %w",
			netclient.ErrBodyLimit))
	}

	switch {
	case resp.StatusCode == nethttp.StatusForbidden:
		return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode, contracts.ErrProvider403)
	case resp.StatusCode == nethttp.StatusNotFound:
		// A search miss answers 404 with {"error":"Ничего не
		// найдено"}: surface the server's text with the typed miss.
		if msg := animevostErrorText(body); msg != "" {
			return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode,
				fmt.Errorf("%w: %s", contracts.ErrNotFound, msg))
		}
		return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode, contracts.ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			&netclient.StatusError{StatusCode: resp.StatusCode, Status: resp.Status})
	}
	return body, nil
}

// animevostErrorText extracts the human-readable error message from an
// error body, both shapes observed live: {"error":"..."} (search miss)
// and {"status":"fail","error":"..."} (playlist, unknown id).
func animevostErrorText(body []byte) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Error
}

// NamePreference declares the search routing of the 2026-09-18 index:
// canonical latin names match reliably (black lagoon, naruto), while
// Cyrillic phrases only hit in their exact inflected site-title form
// ("моя геройская академия" hits, "черная лагуна" misses the title
// «Пираты «Черной лагуны»»), so the fan-out must send the
// romaji/english variant.
func (p *AnimeVost) NamePreference() contracts.NamePreference {
	return contracts.NamePrefLatin
}

// animevostSearch mirrors the fields consumed by animevost.py:29-36.
// The 2026-09-18 envelope wraps the list in {"state":{...},"data":
// [...]}; state.count is stale metadata (it stays 0 while data carries
// hits) and is not consulted.
type animevostSearch struct {
	Error string `json:"error"`
	Data  []struct {
		ID              json.Number `json:"id"`
		Title           string      `json:"title"`
		URLImagePreview string      `json:"urlImagePreview"`
	} `json:"data"`
}

// animevostPlaylistEntry mirrors the fields consumed by animevost.py:50-62.
type animevostPlaylistEntry struct {
	Name string `json:"name"`
	HD   string `json:"hd"`
	Std  string `json:"std"`
}

// Search POSTs {"name": query} to /search (anicli-py animevost.py:20-37).
// Misses and decode failures surface as typed ProviderErrors — the
// silent empty list of the Python original (except Exception: return
// []) faked a healthy-but-dead provider.
func (p *AnimeVost) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	body, err := p.postForm(ctx, contracts.OpSearch, "/search", url.Values{"name": {query}})
	if err != nil {
		return nil, err
	}

	var data animevostSearch
	if jsonErr := json.Unmarshal(body, &data); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("decode search response: %w", jsonErr))
	}
	// A 200 body can still carry the error envelope; surface it.
	if data.Error != "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: %s", contracts.ErrNotFound, data.Error))
	}

	results := make([]contracts.SearchResult, 0, len(data.Data))
	for _, item := range data.Data {
		results = append(results, contracts.SearchResult{
			Title:    item.Title,
			URL:      pythonStr(item.ID), // Python str(item.get("id")) → "None" when missing
			SourceID: p.ID(),
			Poster:   item.URLImagePreview,
		})
	}
	return results, nil
}

// GetEpisodes POSTs {"id": animeURL} to /playlist (anicli-py
// animevost.py:39-64). Episodes are numbered 1-based; the name falls back
// to the index string; links are stashed as one JSON object raw embed.
// Unknown ids answer HTTP 200 with {"status":"fail","error":"Тайтл с
// таким id не найден"} — surfaced as a typed miss instead of the
// Python original's silent empty list.
func (p *AnimeVost) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	body, err := p.postForm(ctx, contracts.OpGetEpisodes, "/playlist", url.Values{"id": {animeURL}})
	if err != nil {
		return nil, err
	}

	// A JSON body is either the playlist array or a fail object; route
	// on the first non-space byte instead of inferring from decode
	// failures.
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return p.decodePlaylist(trimmed)
	}

	var fail struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if jsonErr := json.Unmarshal(trimmed, &fail); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("decode playlist response: %w", jsonErr))
	}
	msg := fail.Error
	if msg == "" {
		msg = fmt.Sprintf("unexpected playlist response: %.120s", trimmed)
	}
	return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
		fmt.Errorf("%w: %s", contracts.ErrNotFound, msg))
}

// decodePlaylist maps the playlist array to episodes: 1-based enumerate
// supplies num and raw_id; the name falls back to the index string
// (animevost.py:51-52); hd/std links ride as one JSON object raw embed.
func (p *AnimeVost) decodePlaylist(body []byte) ([]contracts.Episode, error) {
	var entries []animevostPlaylistEntry
	if jsonErr := json.Unmarshal(body, &entries); jsonErr != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("decode playlist response: %w", jsonErr))
	}

	episodes := make([]contracts.Episode, 0, len(entries))
	for i, item := range entries {
		name := item.Name
		if name == "" {
			name = strconv.Itoa(i + 1)
		}
		links := map[string]string{}
		if item.HD != "" {
			links["hd"] = item.HD
		}
		if item.Std != "" {
			links["std"] = item.Std
		}
		payload, err := json.Marshal(links)
		if err != nil {
			// map[string]string is always marshalable; kept for honesty.
			return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
				fmt.Errorf("encode links for episode %d: %w", i+1, err))
		}

		idx := strconv.Itoa(i + 1)
		episodes = append(episodes, contracts.Episode{
			Num:   idx,
			Title: name,
			RawID: idx,
			RawEmbeds: map[string][]string{
				"AnimeVost": {string(payload)},
			},
		})
	}
	return episodes, nil
}

// ResolveStream decodes the JSON links payload: hd maps to quality 720
// and std to 480, both typed mp4 (anicli-py animevost.py:66-76). A
// missing dub defaults the payload to "{}" and yields an empty stream.
func (p *AnimeVost) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	raw := "{}"
	if embeds := episode.RawEmbeds[dubID]; len(embeds) > 0 {
		raw = embeds[0]
	}

	var links struct {
		HD  string `json:"hd"`
		Std string `json:"std"`
	}
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}
	if err := json.Unmarshal([]byte(raw), &links); err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0,
			fmt.Errorf("decode links payload: %w", err))
	}
	if links.HD != "" {
		stream.Links["720"] = contracts.VideoSource{URL: links.HD, Quality: "720", Type: "mp4"}
	}
	if links.Std != "" {
		stream.Links["480"] = contracts.VideoSource{URL: links.Std, Quality: "480", Type: "mp4"}
	}
	return stream, nil
}
