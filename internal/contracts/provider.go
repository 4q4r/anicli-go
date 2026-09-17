package contracts

import "context"

// Provider operation names used in ProviderError.Op. Exported so callers can
// match on them without sprinkling magic strings.
const (
	OpSearch        = "search"
	OpGetEpisodes   = "get_episodes"
	OpResolveStream = "resolve_stream"
)

// Provider is the consumer-side contract every source must satisfy. It is
// deliberately small: consumers only search, list episodes and resolve one
// stream for a chosen dub.
//
// All network-touching methods take a context and must honour its
// cancellation. Errors returned by implementations should wrap one of the
// sentinel errors of this package (usually inside a ProviderError) so
// consumers can route on the failure class.
type Provider interface {
	// ID returns the stable provider identifier, e.g. "animego".
	ID() string
	// Name returns the human-readable provider name.
	Name() string
	// BaseURL returns the provider site root URL.
	BaseURL() string
	// SourceType reports what kind of content the provider serves.
	SourceType() SourceType

	// Search queries the provider for anime matching query.
	Search(ctx context.Context, query string) ([]SearchResult, error)
	// GetEpisodes lists episodes for the anime at animeURL.
	GetEpisodes(ctx context.Context, animeURL string) ([]Episode, error)
	// ResolveStream resolves playable links for episode under the dub
	// identified by dubID.
	ResolveStream(ctx context.Context, episode Episode, dubID string) (MediaStream, error)
}

// DubsHydrator is the optional capability of providers whose episode
// listings arrive with EMPTY RawEmbeds and hydrate the dub list per
// episode (anilib, animego; python fetch_dubs_for_episode). Since
// PR44 the session runs the hydration EAGERLY in the episode-fetch
// phase (bounded-concurrent, one request per episode — the verified
// API shape of both providers): the steady flow never surfaces an
// episode whose embeds are not hydrated. The capability stays
// exported for the «🔄 Обновить источники» recovery, which re-runs
// the hydration after a transient failure. Providers without the
// capability list their dubs eagerly.
//
// FetchDubs fills episode.RawEmbeds in place (a copy is passed by
// callers) and returns the same episode. A transport failure returns
// the error: callers treat it as "no dubs known" but log it (fail loud
// over silent swallow).
type DubsHydrator interface {
	FetchDubs(ctx context.Context, episode *Episode) (*Episode, error)
}
