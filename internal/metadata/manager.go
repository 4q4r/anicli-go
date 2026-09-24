package metadata

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// DefaultProviderOrder is the canonical provider sequence used when no
// configured order exists (python metadata_providers.order default).
var DefaultProviderOrder = []string{"anilist", "kitsu", "anisearch", "anidb"}

// Manager limits, ported from the python manager defaults.
const (
	// defaultPerProviderTimeout bounds one provider call (python
	// asyncio.wait_for timeout=15).
	defaultPerProviderTimeout = 15 * time.Second
	// defaultPerProviderLimit caps aliases accepted from one provider.
	defaultPerProviderLimit = 20
	// defaultGlobalLimit caps the merged alias set.
	defaultGlobalLimit = 80
)

// maxQueryVariants caps query expansion (FEATURE_INVENTORY B:
// MAX_QUERY_VARIANTS=8).
// PR97: 16 — a Shikimori card carries 5+ distinct names (original,
// russian, english[], japanese[], synonyms[]); the larger cap keeps
// the whole inventory as query variants.
const maxQueryVariants = 16

// Provider is one metadata source the manager can consult.
type Provider interface {
	// ID is the stable provider key used in ordering ("anilist",
	// "kitsu", "anisearch", "anidb").
	ID() string
	// SearchAlternativeTitles returns normalized title aliases for a
	// query. Errors mean provider-unavailable; the manager skips.
	SearchAlternativeTitles(ctx context.Context, query string) ([]string, error)
}

// Manager aggregates metadata aliases from multiple providers with
// ordered fallback: providers are consulted in order, each bounded by a
// per-provider timeout; failures are skipped and the next provider still
// contributes; successful results merge into one deduplicated alias set
// (python AnimeMetadataManager, python `except Exception: continue`).
type Manager struct {
	// ranked is the normalized consultation sequence (all known
	// providers, configured ones first).
	ranked             []string
	providers          []Provider
	perProviderTimeout time.Duration
	perProviderLimit   int
	globalLimit        int
	log                *slog.Logger
}

// NewManager builds the manager over the given providers. The effective
// consultation order follows the python active_order contract: known
// configured entries first (trimmed, lowercased, in configured
// sequence), then the remaining known providers in canonical order;
// unknown entries are dropped. providers may be nil (order-only use).
func NewManager(providers []Provider, order []string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}

	ranked := normalizeOrder(order)
	byID := make(map[string]Provider, len(providers))
	for _, p := range providers {
		byID[strings.ToLower(strings.TrimSpace(p.ID()))] = p
	}
	ordered := make([]Provider, 0, len(byID))
	for _, id := range ranked {
		if p, ok := byID[id]; ok {
			ordered = append(ordered, p)
		}
	}

	return &Manager{
		ranked:             ranked,
		providers:          ordered,
		perProviderTimeout: defaultPerProviderTimeout,
		perProviderLimit:   defaultPerProviderLimit,
		globalLimit:        defaultGlobalLimit,
		log:                logger,
	}
}

// DefaultProviders builds the four production clients over the shared
// transport, ready for NewManager.
func DefaultProviders(net *netclient.Client) []Provider {
	return []Provider{
		NewAniListClient(net, "", nil),
		NewKitsuClient(net, "", nil),
		NewAniSearchClient(net, "", nil),
		NewAniDBClient(net, "", nil),
	}
}

// normalizeOrder validates a configured order against the known
// providers: known entries keep their configured sequence, missing known
// providers are appended in canonical order, duplicates collapse.
func normalizeOrder(configured []string) []string {
	known := DefaultProviderOrder
	lower := make([]string, 0, len(configured))
	for _, item := range configured {
		lower = append(lower, strings.ToLower(strings.TrimSpace(item)))
	}

	order := make([]string, 0, len(known))
	for _, item := range lower {
		if slices.Contains(known, item) && !slices.Contains(order, item) {
			order = append(order, item)
		}
	}
	for _, id := range known {
		if !slices.Contains(order, id) {
			order = append(order, id)
		}
	}
	return order
}

// order exposes the normalized consultation sequence
// (diagnostics/tests).
func (m *Manager) order() []string {
	return append([]string(nil), m.ranked...)
}

// SearchAlternativeTitles returns normalized unique aliases across the
// providers: ordered consultation, per-provider timeout and limit,
// global cap, dedupe on the lowercase key seeded with the query itself
// (the query never leaks into its own alias set).
func (m *Manager) SearchAlternativeTitles(ctx context.Context, query string) ([]string, error) {
	normalizedQuery := normalizeTitle(query)
	if normalizedQuery == "" {
		return nil, nil
	}

	collected := make([]string, 0, m.perProviderLimit)
	seen := map[string]bool{strings.ToLower(normalizedQuery): true}

	for _, provider := range m.providers {
		pctx, cancel := context.WithTimeout(ctx, m.perProviderTimeout)
		titles, err := provider.SearchAlternativeTitles(pctx, normalizedQuery)
		cancel()
		if err != nil {
			// python: except Exception: continue — provider down never
			// breaks the aggregate.
			m.log.Debug("metadata provider unavailable",
				"provider", provider.ID(), "error", err)
			continue
		}

		// Deterministic per-provider order (python sorted(titles)).
		sorted := append([]string(nil), titles...)
		sort.Strings(sorted)

		providerCount := 0
		for _, title := range sorted {
			normalized := normalizeTitle(title)
			if normalized == "" {
				continue
			}
			key := strings.ToLower(normalized)
			if seen[key] {
				continue
			}
			seen[key] = true
			collected = append(collected, normalized)
			providerCount++
			if providerCount >= m.perProviderLimit {
				break
			}
			if len(collected) >= m.globalLimit {
				return collected, nil
			}
		}
	}
	return collected, nil
}

// QueryVariants expands a title with its metadata aliases for search
// fan-out: the original title first, then the aliases, then the
// lowercase variants of both, deduplicated in order and capped at
// maxQueryVariants (16).
//
// Divergence note: the frozen Python tree lost build_query_variants
// (FEATURE_INVENTORY B regression list); the original MAX_QUERY_VARIANTS
// semantics are reconstructed here in the minimal documented form per
// the PR8 spec — original + aliases + lowercase variants, cap 16.
func QueryVariants(title string, aliases []string) []string {
	candidates := make([]string, 0, 1+2*len(aliases))
	candidates = append(candidates, title)
	candidates = append(candidates, aliases...)
	candidates = append(candidates, strings.ToLower(title))
	for _, alias := range aliases {
		candidates = append(candidates, strings.ToLower(alias))
	}

	seen := map[string]bool{}
	variants := make([]string, 0, maxQueryVariants)
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		variants = append(variants, candidate)
		if len(variants) >= maxQueryVariants {
			break
		}
	}
	return variants
}
