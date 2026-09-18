package providers

// Shiza (PR57) — the shizaproject.com provider (Russian dub team; "качественная озвучка + оригинальные субтитры"). Not a Python port: the
// source was characterized live on 2026-09-18 and the provider was
// written against the observed API.
//
// Archaeology summary (2026-09-18): the site is a Nuxt SPA whose
// runtime config (window.__NUXT__.config.public.PUBLIC_API_URL) exposes
// the real API — a GraphQL endpoint at https://shizaproject.com/graphql
// (Apollo Server; the REST paths /api/release answer 404, introspection
// is disabled in production). The site bundle (/_nuxt/Cw_SLnEj.js)
// carries the compiled Apollo documents; the provider's two query
// documents are minimal projections of the site's fetchReleases /
// fetchRelease operations, captured verbatim with their fixtures
// (testdata/shiza_*.json). Anonymous access is enough: search, release
// detail, episodes and embeds all answer without cookies; the service
// answers direct from RU networks (no Cloudflare challenge observed).
//
// Playback: every episode video is an embed of SHIZA's hosting —
// embedSource KODIK (kodikplayer.com, the team's dub) or SIBNET
// (video.sibnet.ru mirror), with a single VK outlier in the whole
// catalog. Both player hosts have extractors in the shared factory, so
// ResolveStream is the anidub pattern (resolveEmbeds); embedSource is
// the HOST, not a dub name — a release is one team's work, so all its
// embeds ride the single "SHIZA Project" dub key.
//
// Torrents: the Release type also carries torrent entries (magnet with
// the tr.shiza-project.com announces + an anonymous .torrent file on
// cdn.shizaproject.com), but the swarm is dead — the API reports 0
// seeders on every sampled torrent (100 newest + 50 oldest of 1881,
// 2026-09-18; Jackett's indexer request reached the same conclusion).
// A torrent sibling provider would therefore surface zero live
// results, and per the kodik-parity rule (never register a provider
// that cannot run) it is NOT registered; only this stream provider is.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// ShizaBase is the site root [LIVE-VERIFIED 2026-09-18]. The GraphQL
// endpoint rides the same host (the Nuxt PUBLIC_API_URL).
const ShizaBase = "https://shizaproject.com"

// shizaGraphQLPath is the GraphQL endpoint path on the site host.
const shizaGraphQLPath = "/graphql"

// shizaDub is the single dub key of the provider: a release is one
// team's dub, whichever embed host carries it.
const shizaDub = "SHIZA Project"

// shizaSearchLimit is the search page size sent as $first. The TUI
// search fan-out renders the top hits; the catalog ordering (relevance
// for $query searches) puts the best match first [LIVE-VERIFIED
// 2026-09-18: "черная лагуна" → Black Lagoon TV first].
const shizaSearchLimit = 20

// shizaReleasePathPrefix separates the release page URL from its slug.
// The slug IS the API handle (release(slug:)); the site serves
// /releases/<slug> where some slugs are @snowflake forms — both parse
// back the same way.
const shizaReleasePathPrefix = "/releases/"

// shizaSearchQuery is the exact search document the fixtures were
// captured with (2026-09-18) — a minimal projection of the site's
// fetchReleases. Pins live in shiza_test.go.
const shizaSearchQuery = `query fetchReleases($first: Int, $query: String) { releases(first: $first, query: $query) { edges { node { slug name posters { preview: resize(width: 360, height: 500) { url } } } } } }`

// shizaReleaseQuery is the exact release-detail document the fixtures
// were captured with (2026-09-18) — a minimal projection of the site's
// fetchRelease. viewerInBlockedCountry is the API's own geo flag: SHIZA
// restricts content per requester region, and the embeds of a flagged
// release do not resolve.
const shizaReleaseQuery = `query fetchRelease($slug: String!) { release(slug: $slug) { viewerInBlockedCountry episodes { number name videos { embedUrl } } } }`

// Shiza is the shizaproject.com provider (Russian dub).
type Shiza struct {
	Base
}

// newShiza builds the provider against baseURL. The service answers
// plain browser-profile requests (verified via curl, no extra headers).
func newShiza(baseURL string, http *netclient.Client) *Shiza {
	return &Shiza{Base: Base{
		id:          "shiza",
		name:        "SHIZA Project",
		baseURL:     baseURL,
		sourceType:  contracts.SourceTypeBoth,
		contentLang: "ru",
		http:        http,
	}}
}

// shizaGraphQLRequest is the JSON body of one GraphQL operation.
type shizaGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

// shizaGraphQLError is one entry of the Apollo errors array.
type shizaGraphQLError struct {
	Message string `json:"message"`
}

// shizaGraphQLEnvelope is the wire envelope of every GraphQL response.
type shizaGraphQLEnvelope struct {
	Data   json.RawMessage     `json:"data"`
	Errors []shizaGraphQLError `json:"errors"`
}

// graphql runs one GraphQL operation and decodes the data field into
// out. Transport failures and HTTP statuses are the netclient's typed
// errors (403 → ErrProvider403, 404 → ErrNotFound, others StatusError);
// an application-level errors array fails with its first message.
func (p *Shiza) graphql(ctx context.Context, op, query string, variables map[string]any, data any) error {
	payload, err := json.Marshal(shizaGraphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return contracts.WrapProvider(p.ID(), op, 0, fmt.Errorf("encode graphql request: %w", err))
	}
	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + shizaGraphQLPath,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    bytes.NewReader(payload),
		Op:      op,
	})
	if err != nil {
		return err
	}
	var envelope shizaGraphQLEnvelope
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			fmt.Errorf("decode graphql response: %w", err))
	}
	if len(envelope.Errors) > 0 {
		return contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			fmt.Errorf("graphql: %s", envelope.Errors[0].Message))
	}
	if len(envelope.Data) == 0 {
		return contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			errors.New("graphql response carries no data"))
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		return contracts.WrapProvider(p.ID(), op, resp.StatusCode,
			fmt.Errorf("decode graphql data: %w", err))
	}
	return nil
}

// shizaImagePreview mirrors one poster entry of the Release node (the
// site's ReleasePosterCommon shape: the 360x500 resize preview).
type shizaImagePreview struct {
	Preview struct {
		URL string `json:"url"`
	} `json:"preview"`
}

// shizaReleaseNode is the search projection of one release.
type shizaReleaseNode struct {
	Slug    string              `json:"slug"`
	Name    string              `json:"name"`
	Posters []shizaImagePreview `json:"posters"`
}

// Search resolves the query through the GraphQL releases search
// [LIVE-VERIFIED 2026-09-18: the query matches BOTH the Russian names
// ("черная лагуна" → 2 releases) and the romaji originals ("watashi" →
// 13); the API does the matching, no client-side filter]. Results keep
// the site's release-page URL (search → GetEpisodes round-trips the
// slug through it).
func (p *Shiza) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	// Empty queries are a caller bug: reject before any network I/O.
	if strings.TrimSpace(query) == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: пустой поисковый запрос", contracts.ErrInvalidInput))
	}
	var data struct {
		Releases struct {
			Edges []struct {
				Node shizaReleaseNode `json:"node"`
			} `json:"edges"`
		} `json:"releases"`
	}
	if err := p.graphql(ctx, contracts.OpSearch, shizaSearchQuery, map[string]any{
		"first": shizaSearchLimit,
		"query": query,
	}, &data); err != nil {
		return nil, err
	}
	results := make([]contracts.SearchResult, 0, len(data.Releases.Edges))
	for _, edge := range data.Releases.Edges {
		node := edge.Node
		if node.Slug == "" {
			continue // not a navigable release
		}
		var poster string
		if len(node.Posters) > 0 {
			poster = node.Posters[0].Preview.URL
		}
		results = append(results, contracts.SearchResult{
			Title:    node.Name,
			URL:      p.baseURL + shizaReleasePathPrefix + node.Slug,
			SourceID: p.ID(),
			Poster:   poster,
		})
	}
	return results, nil
}

// shizaEpisode is the detail projection of one ReleaseEpisode.
type shizaEpisode struct {
	// Number is nullable on the wire (announcement stubs); null-number
	// entries are not consumable episodes and are skipped.
	Number json.Number `json:"number"`
	Name   string      `json:"name"`
	Videos []struct {
		EmbedURL string `json:"embedUrl"`
	} `json:"videos"`
}

// GetEpisodes fetches the release detail by the slug carried in the
// search result's URL and maps the episodes onto the standard listing:
// one dub key ("SHIZA Project") whose embed list preserves the API
// order (kodik dub first, sibnet mirror second). A geo-flagged release
// fails with ErrGeoBlocked (the API's viewerInBlockedCountry — its own
// region restriction), a missing slug with ErrNotFound [LIVE-VERIFIED
// 2026-09-18: unknown slug → {"data":{"release":null}}].
func (p *Shiza) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	slug := strings.TrimPrefix(animeURL, p.baseURL+shizaReleasePathPrefix)
	if slug == "" || slug == animeURL {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: URL не содержит слаг релиза: %s", contracts.ErrInvalidInput, animeURL))
	}
	var data struct {
		Release *struct {
			ViewerInBlockedCountry bool           `json:"viewerInBlockedCountry"`
			Episodes               []shizaEpisode `json:"episodes"`
		} `json:"release"`
	}
	if err := p.graphql(ctx, contracts.OpGetEpisodes, shizaReleaseQuery, map[string]any{
		"slug": slug,
	}, &data); err != nil {
		return nil, err
	}
	if data.Release == nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: релиз %q", contracts.ErrNotFound, slug))
	}
	if data.Release.ViewerInBlockedCountry {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, 0,
			fmt.Errorf("%w: контент SHIZA закрыт для этого региона", contracts.ErrGeoBlocked))
	}
	episodes := make([]contracts.Episode, 0, len(data.Release.Episodes))
	for _, ep := range data.Release.Episodes {
		if ep.Number == "" {
			continue // announcement stub without a number
		}
		num := ep.Number.String()
		embeds := make([]string, 0, len(ep.Videos))
		for _, v := range ep.Videos {
			if u := strings.TrimSpace(v.EmbedURL); u != "" {
				embeds = append(embeds, u)
			}
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: ep.Name,
			RawID: num, // the wire number literal ("1", "2.5")
			RawEmbeds: map[string][]string{
				shizaDub: embeds,
			},
		})
	}
	return episodes, nil
}

// ResolveStream runs the episode's embeds through the extractor
// factory (the anidub pattern): the kodik extractor resolves the
// team's player pages into quality-keyed sources, the sibnet extractor
// the mirrors; later links overwrite earlier quality keys (the shared
// resolveEmbeds merge).
func (p *Shiza) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}
	sources, err := resolveEmbeds(ctx, p.http, episode.RawEmbeds[dubID])
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	stream.Links = sources
	return stream, nil
}
