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
