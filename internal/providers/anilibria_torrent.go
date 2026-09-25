package providers

// AniLibriaTorrent (PR37) — torrent search over the aniliberty.top
// API on the TorrentBase plumbing. The new API has no query
// parameter on its torrents feed (/anime/torrents ignores query
// params — live-verified 2026-09-17); search therefore rides the same
// release search as the stream provider and expands each hit into the
// per-release torrent list (GET /anime/torrents/release/{id}, the
// endpoint the site's own ReleaseTorrentsTab calls). Every torrent of
// a release (quality/codec variants) becomes one search result
// carrying the API's magnet — the infohash and announce trackers ride
// inside it, so the engine ingests it directly and the passkey-gated
// .torrent file endpoint (/anime/torrents/{hash}/file needs the
// profile passkey) is never touched. Not a Python port: written
// against the live API, endpoints and field set captured by curl on
// 2026-09-17 (controller dossier + implementer archaeology of the
// site's lazy chunks torrentsProxy.Dmr6gpee.js / ReleaseTorrentsTab).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// AniLibriaTorrentSearchReleaseLimit caps how many release-search hits
// are probed for torrents. Broad queries match dozens of releases and
// each probe is a sequential API call — six top hits bound the search
// latency while every practical query still yields torrent results.
const AniLibriaTorrentSearchReleaseLimit = 6

// AniLibriaTorrent is the aniliberty.top torrent provider over the
// shared Base identity and TorrentBase engine plumbing.
type AniLibriaTorrent struct {
	Base
	*TorrentBase
}

// newAnilibriaTorrent builds the provider. The engine may be nil
// (fails loud on use): the registry injects the shared engine when
// [torrent] is enabled.
func newAnilibriaTorrent(apiBase string, http *netclient.Client, engine *torrent.Engine) *AniLibriaTorrent {
	return &AniLibriaTorrent{
		Base: Base{
			id:          "anilibria-torrent",
			name:        "АниЛибрия (торренты)",
			baseURL:     apiBase,
			sourceType:  contracts.SourceTypeBoth,
			contentLang: "ru",
			http:        http,
		},
		TorrentBase: NewTorrentBase(engine),
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (p *AniLibriaTorrent) IsTorrent() bool { return true }

// anilibriaTorrentItem mirrors one entry of the per-release torrent
// list. Only the consumed fields are modeled; the API also embeds a
// full release object and codec/color descriptors that the parse
// ignores. quality.value is a nullable descriptor ("1080p").
type anilibriaTorrentItem struct {
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
	Label    string `json:"label"`
	Magnet   string `json:"magnet"`
	Seeders  int    `json:"seeders"`
	Leechers int    `json:"leechers"`
	Quality  struct {
		Value string `json:"value"`
	} `json:"quality"`
}

// Search resolves the query to releases through the shared release
// search endpoint, then expands the top hits into their per-release
// torrent lists. The torrent link is the API's magnet verbatim (its
// tr= announces aid peer discovery) when it carries a usable infohash,
// a magnet built from the API hash otherwise; entries with neither are
// dropped instead of handed downstream as dead results (the TorrentBase rule).
func (p *AniLibriaTorrent) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}

	searchURL := p.baseURL + "/app/search/releases?query=" + url.QueryEscape(query)
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    searchURL,
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}
	var items []anilibriaSearchItem
	if err := json.Unmarshal(resp.Body, &items); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode search response: %w", err))
	}

	results := make([]contracts.SearchResult, 0, len(items))
	probed := 0
	for _, item := range items {
		if probed >= AniLibriaTorrentSearchReleaseLimit {
			break
		}
		probed++
		torrents, err := p.releaseTorrents(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		for _, tr := range torrents {
			res, ok := anilibriaTorrentResult(p.ID(), item, tr)
			if !ok {
				continue
			}
			results = append(results, res)
		}
	}
	// Seedless entries are dead results: drop them before they surface
	// (the API's seeders int is authoritative; nothing to fail-soft).
	return filterSeedless(results), nil
}

// releaseTorrents fetches one release's torrent list by numeric id. A
// 404 is a live-verified legitimate answer: the API geo-hides content
// per requester IP (e.g. Dandadan's torrents from a RU exit), and the
// release-search response can still carry the release stub — such a
// release contributes nothing instead of failing the whole search.
func (p *AniLibriaTorrent) releaseTorrents(ctx context.Context, id json.Number) ([]anilibriaTorrentItem, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    p.baseURL + "/anime/torrents/release/" + id.String(),
		Op:     contracts.OpSearch,
	})
	if err != nil {
		if errors.Is(err, contracts.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var torrents []anilibriaTorrentItem
	if err := json.Unmarshal(resp.Body, &torrents); err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("decode torrent list for release %s: %w", id.String(), err))
	}
	return torrents, nil
}

// anilibriaTorrentResult maps one API torrent onto a search result. ok
// is false when the entry has no usable torrent link (dropped by the
// caller) or no usable title.
func anilibriaTorrentResult(providerID string, release anilibriaSearchItem, tr anilibriaTorrentItem) (contracts.SearchResult, bool) {
	title := strings.TrimSpace(tr.Label)
	if title == "" {
		return contracts.SearchResult{}, false
	}
	link, ok := anilibriaTorrentLink(title, tr.Magnet, tr.Hash)
	if !ok {
		return contracts.SearchResult{}, false
	}
	quality := strings.TrimSpace(tr.Quality.Value)
	if quality == "" {
		quality = torrent.ParseQuality(title).Badge()
	}
	return contracts.SearchResult{
		Title:    title,
		URL:      link,
		SourceID: providerID,
		Meta: map[string]any{
			SearchMetaSize:     humanBytes(tr.Size),
			SearchMetaSeeders:  strconv.Itoa(tr.Seeders),
			SearchMetaLeechers: strconv.Itoa(tr.Leechers),
			SearchMetaQuality:  quality,
		},
	}, true
}

// anilibriaTorrentLink picks the torrent link of one API entry: the
// magnet field verbatim when it carries a well-formed 40-hex btih
// hash, a magnet built from the hash field otherwise, unusable (false)
// when neither is present.
func anilibriaTorrentLink(label, magnet, hash string) (string, bool) {
	if magnetURI(magnet) {
		return magnet, true
	}
	h := strings.ToLower(strings.TrimSpace(hash))
	if len(h) == infoHashHexLen && isHex(h) {
		return "magnet:?xt=urn:btih:" + h + "&dn=" + url.QueryEscape(label), true
	}
	return "", false
}

// magnetURI reports whether s is a magnet: URI whose btih parameter is
// a well-formed 40-hex infohash (the engine ingests it directly).
func magnetURI(s string) bool {
	const prefix = "magnet:?"
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	for _, param := range strings.Split(s[len(prefix):], "&") {
		if hash, ok := strings.CutPrefix(param, "xt=urn:btih:"); ok {
			hash = strings.ToLower(hash)
			return len(hash) == infoHashHexLen && isHex(hash)
		}
	}
	return false
}

// humanBytes renders a byte count in binary units with one decimal —
// the TUI torrent suffix convention ("16.2 GiB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (the TorrentBase
// contract; single-episode torrents yield one playable entry, batches
// one per file with the parsed episode numbers).
func (p *AniLibriaTorrent) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *AniLibriaTorrent) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}
