package providers

// SubsPlease (subsplease.org) is the EN seasonal TORRENT provider
// (PR89) — the biggest EN subtitle group — on the shared TorrentBase
// plumbing (the TorrentBase PR36 pattern). Anonymous, no credentials,
// nothing to configure. Not a Python port: written against the live
// site, endpoints and field set captured by curl on 2026-09-23.
//
// Search does NOT ride the site's RSS feeds. They exist and are
// documented (https://subsplease.org/rss-feeds/: /rss/?t&r=1080
// torrent links, /rss magnet links — both captured live), but they
// are latest-releases-only (~75 items), take no query parameter
// (verified: s= is ignored, byte-identical answer), and the torrent
// feed's <link> points at the upstream view/torrent page — a 504-flaky
// host from blocked networks (the documented RST/504 class).
// The site's own JSON API is strictly better and the same one the
// site's JavaScript drives:
//
//	GET /api/?f=search&s={query}&tz=0   → matching releases, whole
//	                                      catalog, newest first (≤30)
//	GET /api/?f=show&sid={id}&tz=0      → one show's {"batch":…,
//	                                      "episode":…} releases
//	GET /shows/{slug}/                  → the show page; its
//	                                      show-release-table tag
//	                                      carries sid="…" — the only
//	                                      slug→sid hop
//
// The tz=0 parameter is REQUIRED: without it every /api/ leg answers
// HTTP 200 with zero bytes (live-verified), which would decode as a
// silent empty catalog.
//
// Each release entry carries downloads[{res, magnet}] — the magnet is
// the release's REAL distribution link, tracker-rich (13 tr= announces
// from the upstream tracker pool), so the PR66 bytes-over-magnet rationale does
// not apply here: that ruling exists because SYNTHESIZED tracker-less
// magnets resolve metadata via DHT only and time out under real
// budgets (the PR52 smoke FAIL class). These magnets announce to a
// dozen live trackers, the engine owns magnets, and magnet results
// skip the dead-host preflight by construction. The f=show payload
// also carries per-download torrent= URLs — view-page links on the
// flaky host above — deliberately not taken.
//
// The magnet xt= hashes are 32-char BASE32 (the upstream convention). That
// is engine-ingestable as-is: anacrolix ParseMagnetUri accepts both
// 40-hex and 32-base32 encodings (the animetosho "40-hex contract"
// note covers tracker-list FILES, not magnet URIs) — pinned by
// TestSubsPleaseLinkIsEngineIngestable.
//
// Batch back-catalog: search surfaces the matched episode releases
// first (capped — the wire head is the newest), then expands the TOP
// matched show through the show-page sid hop into its batch torrents,
// so full-season watching never depends on what happens to air this
// week. The hop is fail-soft: a show page without a usable sid is a
// logged skip, never a failed search.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// SubsPleaseBase is the site root: both the JSON API and the show
// pages live under it.
const SubsPleaseBase = "https://subsplease.org"

// subsplease search constants (live-verified): the tz value is
// arbitrary but MUST be present; the result caps bound the surfaced
// surface — 18 episode results (the 6 newest releases × 3
// resolutions) plus 6 batch results keep the TUI list and the
// torrent smoke's resolve surface predictable.
const (
	subspleaseTZ               = "0"
	subspleaseEpisodeResultCap = 18
	subspleaseBatchResultCap   = 6
	subspleaseBatchShowLimit   = 1
	subspleaseSmokeQuery       = "re:zero"
	subspleaseSIDPattern       = `id="show-release-table"[^>]*\ssid="(\d+)"`
)

// subspleaseSIDRe digs the show id out of the show-page HTML (the
// live tag: <table id="show-release-table" cellpadding="0" … sid="87">).
var subspleaseSIDRe = regexp.MustCompile(subspleaseSIDPattern)

// SubsPlease is the subsplease.org torrent provider over the shared
// Base identity and TorrentBase engine plumbing.
type SubsPlease struct {
	Base
	*TorrentBase
}

// newSubsPlease builds the provider. The engine may be nil (fails
// loud on use): the registry injects the shared engine when
// [torrent] is enabled.
func newSubsPlease(baseURL string, http *netclient.Client, engine *torrent.Engine) *SubsPlease {
	return &SubsPlease{
		Base: Base{
			id:          "subsplease",
			name:        "SubsPlease",
			baseURL:     baseURL,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ja",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *SubsPlease) IsTorrent() bool { return true }

// NamePreference implements contracts.NamePreferenceProvider: the
// SubsPlease index matches EN/romaji release names only — the search
// fan-out must route it the latin variants, never the Cyrillic ones.
func (p *SubsPlease) NamePreference() contracts.NamePreference {
	return contracts.NamePrefLatin
}

// SmokeQuery implements contracts.SmokeQueryProvider (the PR51
// precedent): the SubsPlease catalog is EN-seasonal, the
// shared smoke probe ("black lagoon") is outside it entirely
// (live-verified 2026-09-23: f=search answers the literal "[]"), so
// the provider declares a catalog answer of its own. Re Zero is a
// multi-cour flagship: its episode AND batch releases stay in the
// searchable catalog for the long haul.
func (p *SubsPlease) SmokeQuery() string { return subspleaseSmokeQuery }

// spDownload is one download slot of a release: the resolution label
// ("480"/"720"/"1080") and the magnet link. The torrent= URL (view
// page) appears on f=show payloads and is deliberately not
// consumed — see the header rationale.
type spDownload struct {
	Res     string `json:"res"`
	Magnet  string `json:"magnet"`
	Torrent string `json:"torrent"`
}

// spRelease is one release entry of the /api/ payloads. Title is the
// JSON object KEY — the site keys releases by their display name —
// and encoding/json into maps cannot preserve key order, so the
// decode walks the object with a token decoder and fills it
// (spDecodeReleases): the wire order is meaningful (newest first)
// and the result caps are only honest if the head is the newest.
type spRelease struct {
	Title     string       `json:"-"`
	Show      string       `json:"show"`
	Episode   string       `json:"episode"`
	Page      string       `json:"page"`
	Downloads []spDownload `json:"downloads"`
}

// spDecodeReleases decodes a JSON OBJECT of release entries preserving
// the wire key order. The API's no-match answer is the literal "[]"
// (the PHP empty-assoc quirk: empty result serializes as an array
// while a populated one is an object — live-verified 2026-09-23,
// f=search&s=black lagoon) and settles as zero entries; an EMPTY body
// (the tz-less zero-byte answer) and structurally broken JSON fail
// loud — the caller wraps them typed.
func spDecodeReleases(body []byte) ([]spRelease, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); ok {
		if d == '[' {
			return nil, nil // the documented no-match answer
		}
		if d != '{' {
			return nil, fmt.Errorf("release payload opens with %q, want an object", string(d))
		}
	} else {
		return nil, fmt.Errorf("release payload is %T, want an object", tok)
	}
	out := make([]spRelease, 0, 32)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("release key is %T, want string", keyTok)
		}
		var rel spRelease
		if err := dec.Decode(&rel); err != nil {
			return nil, err
		}
		rel.Title = key
		out = append(out, rel)
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	return out, nil
}

// spShowEnvelope is the f=show envelope: batch and per-episode
// release objects, either of which may be absent ({} entries).
type spShowEnvelope struct {
	Batch   json.RawMessage `json:"batch"`
	Episode json.RawMessage `json:"episode"`
}

// Search queries the site's JSON API. Results are the matched
// episode releases first (wire order head, capped), then the top
// matched show's batch torrents (the back-catalog expansion,
// fail-soft). Every result link is the API's tracker-rich magnet
// verbatim — the engine owns magnets, no dead-host preflight applies.
func (p *SubsPlease) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/api/?" + url.Values{
		"f":  {"search"},
		"s":  {query},
		"tz": {subspleaseTZ},
	}.Encode()
	releases, err := p.fetchReleases(ctx, searchURL)
	if err != nil {
		return nil, err
	}

	results := make([]contracts.SearchResult, 0, subspleaseEpisodeResultCap+subspleaseBatchResultCap)
	for _, rel := range releases {
		if len(results) >= subspleaseEpisodeResultCap {
			break
		}
		results = p.releaseResults(rel, results, subspleaseEpisodeResultCap)
	}

	// Batch back-catalog expansion (fail-soft): the top matched
	// show's page keys its f=show payload; batches surface AFTER the
	// episode results under their own cap. Any hop failure is a
	// logged skip.
	if slug := subspleaseTopPage(releases); slug != "" {
		batches, err := p.showBatches(ctx, slug)
		if err != nil {
			p.loggerOrDiscard().Info("subsplease: batch expansion skipped",
				"slug", slug, "reason", err)
		} else {
			batchStart := len(results)
			for _, rel := range batches {
				if len(results) >= batchStart+subspleaseBatchResultCap {
					break
				}
				results = p.releaseResults(rel, results, batchStart+subspleaseBatchResultCap)
			}
		}
	}
	return results, nil
}

// fetchReleases GETs one /api/ release-map URL and decodes it
// order-preserving. HTTP failures ride the netclient's typed errors;
// a broken body is wrapped with the provider/op tags.
func (p *SubsPlease) fetchReleases(ctx context.Context, apiURL string) ([]spRelease, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    apiURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}
	releases, err := spDecodeReleases(resp.Body)
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode releases: %w", err))
	}
	return releases, nil
}

// subspleaseTopPage returns the first non-empty show slug of the
// wire-ordered releases (the top match), bounded to one expansion.
func subspleaseTopPage(releases []spRelease) string {
	for i, rel := range releases {
		if i >= subspleaseBatchShowLimit {
			return ""
		}
		if slug := strings.TrimSpace(rel.Page); slug != "" {
			return slug
		}
	}
	return ""
}

// showBatches walks the back-catalog hop: show page (the sid
// attribute) → f=show payload → the batch release entries. Every leg
// failure is typed — the caller fail-softs.
func (p *SubsPlease) showBatches(ctx context.Context, slug string) ([]spRelease, error) {
	pageURL := p.baseURL + "/shows/" + url.PathEscape(slug) + "/"
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    pageURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, fmt.Errorf("show page: %w", err)
	}
	m := subspleaseSIDRe.FindSubmatch(resp.Body)
	if m == nil {
		return nil, fmt.Errorf("show page %s: no show-release-table sid", pageURL)
	}

	showURL := p.baseURL + "/api/?" + url.Values{
		"f":   {"show"},
		"sid": {string(m[1])},
		"tz":  {subspleaseTZ},
	}.Encode()
	resp, err = p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    showURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, fmt.Errorf("show payload: %w", err)
	}
	var payload spShowEnvelope
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode show payload: %w", err))
	}
	return spDecodeReleases(payload.Batch)
}

// releaseResults appends one result per usable download of the
// release, stopping at cap. A download without an engine-ingestable
// magnet (missing magnet, missing/invalid btih) is dropped; a release
// left with none contributes nothing.
func (p *SubsPlease) releaseResults(rel spRelease, out []contracts.SearchResult, limit int) []contracts.SearchResult {
	for _, dl := range rel.Downloads {
		if len(out) >= limit {
			return out
		}
		magnet := strings.TrimSpace(dl.Magnet)
		if magnet == "" {
			continue
		}
		m, err := metainfo.ParseMagnetUri(magnet)
		if err != nil {
			// The engine could not ingest this link (no usable
			// xt=urn:btih:) — drop it like an empty title instead of
			// handing downstream a dead result (the TorrentBase rule).
			continue
		}
		title := strings.TrimSpace(m.DisplayName)
		if title == "" {
			// The dn= is the canonical release name; the fallback
			// synthesizes one from the API fields so the TUI never
			// renders an empty row.
			title = fmt.Sprintf("%s - %s (%sp)", rel.Show, rel.Episode, dl.Res)
		}
		meta := map[string]any{
			// The PR35 release parser rides the search result too:
			// the badge is known before ingest.
			SearchMetaQuality: torrent.ParseQuality(title).Badge(),
		}
		if xl := m.Params.Get("xl"); xl != "" {
			meta[SearchMetaSize] = humanBytesAttr(xl)
		}
		out = append(out, contracts.SearchResult{
			Title:    title,
			URL:      magnet,
			SourceID: p.ID(),
			Meta:     meta,
		})
	}
	return out
}

// GetEpisodes ingests the result's magnet and waits — bounded by the
// caller's context — for the engine's metadata fetch, then maps the
// release's files onto standard episodes (the TorrentBase contract:
// single-episode releases yield one playable entry, batches one per
// file with the parsed episode numbers — SubsPlease releases are
// clean `[SubsPlease] Title - NN (1080p) [.mkv]` names the PR35
// parser handles).
func (p *SubsPlease) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *SubsPlease) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
