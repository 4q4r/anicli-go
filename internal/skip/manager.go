package skip

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/an0nx/anicli-go/internal/config"
)

// ProviderIDMerged marks a bundle combining more than one provider.
const ProviderIDMerged = "merged"

// knownProviders is the canonical provider order used to complete a
// configured order (python _KNOWN).
var knownProviders = []string{ProviderAniSkip, ProviderAnimeSkip, ProviderIntroSkipper}

// ResolveRequest describes one skip resolution.
type ResolveRequest struct {
	// ShikimoriID is the anime binding; zero skips the API providers
	// (python `if not mal_id: continue`).
	ShikimoriID int64
	// EpisodeNum is the episode number.
	EpisodeNum float64
	// MediaInput is an optional local media path for intro_skipper.
	MediaInput string
}

// Bundle is the resolved skip set.
type Bundle struct {
	// FFMetadata is the FFMETADATA1 chapter content; empty when no
	// provider found skip times.
	FFMetadata string
	// ChapterTypes is the sorted unique set of skip types found.
	ChapterTypes []string
	// ProviderID is the answering provider, ProviderIDMerged when
	// several contributed, empty when nothing was found.
	ProviderID string
	// Details carries diagnostics: "ok", per-provider degradation
	// notes or "no_provider_result".
	Details string
}

// Empty reports whether any chapter content was resolved.
func (b Bundle) Empty() bool { return b.FFMetadata == "" }

// WriteChaptersFile materializes the bundle content as a temp
// FFMETADATA file (mpv --chapters-file / ffmpeg -i input).
func (b Bundle) WriteChaptersFile(dir string) (string, error) {
	if b.FFMetadata == "" {
		return "", errors.New("skip: write chapters file: empty bundle")
	}
	return WriteChaptersFile(dir, "anicli_", b.FFMetadata)
}

// Manager orchestrates the skip providers with ordered fallback and
// per-provider toggles (ported from anicli-py skip_manager.SkipManager,
// FEATURE_INVENTORY D rulings).
//
// Resolution policy: the enabled API providers (aniskip, anime_skip)
// are all consulted and smart-merged (MergeByType); intro_skipper runs
// only when the API providers found nothing and a local media file was
// supplied.
type Manager struct {
	cfg       config.Skip
	http      HTTP
	aniskip   *AniSkipClient
	animeSkip *AnimeSkipClient
	intro     *IntroSkipperClient
}

// ManagerOption customizes a Manager.
type ManagerOption func(*Manager)

// WithAniSkip overrides the AniSkip client options.
func WithAniSkip(opts AniSkipOptions) ManagerOption {
	return func(m *Manager) { m.aniskip = NewAniSkipClient(m.http, opts) }
}

// WithAnimeSkip overrides the Anime Skip client options.
func WithAnimeSkip(opts AnimeSkipOptions) ManagerOption {
	return func(m *Manager) { m.animeSkip = NewAnimeSkipClient(m.http, opts) }
}

// WithIntroSkipper overrides the IntroSkipper client options.
func WithIntroSkipper(opts IntroSkipperOptions) ManagerOption {
	return func(m *Manager) { m.intro = NewIntroSkipperClient(opts) }
}

// NewManager builds the orchestrator from the config Skip section.
// The AniSkip provider is always enabled (it is the default-first
// source and carries no toggle in config.Skip); anime_skip and
// intro_skipper follow their config flags.
func NewManager(cfg config.Skip, http HTTP, opts ...ManagerOption) *Manager {
	normalized := normalizeOrder(cfg.ProvidersOrder)
	m := &Manager{
		cfg:       cfg,
		http:      http,
		aniskip:   NewAniSkipClient(http, AniSkipOptions{}),
		animeSkip: NewAnimeSkipClient(http, AnimeSkipOptions{}),
		intro:     NewIntroSkipperClient(DefaultIntroSkipperOptions()),
	}
	m.cfg.ProvidersOrder = normalized
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// ActiveOrder returns the validated ordered provider list: configured
// known ids first (in given order), then any known provider missing
// from the configuration (python active_order).
func (m *Manager) ActiveOrder() []string {
	return normalizeOrder(m.cfg.ProvidersOrder)
}

// normalizeOrder folds casing, drops unknown ids and appends missing
// known providers.
func normalizeOrder(configured []string) []string {
	var order []string
	seen := make(map[string]struct{}, len(knownProviders))
	for _, raw := range configured {
		id := strings.ToLower(strings.TrimSpace(raw))
		if !knownProvider(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		order = append(order, id)
	}
	for _, fallback := range knownProviders {
		if _, ok := seen[fallback]; !ok {
			order = append(order, fallback)
		}
	}
	if order == nil {
		order = []string{}
	}
	return order
}

func knownProvider(id string) bool {
	for _, known := range knownProviders {
		if id == known {
			return true
		}
	}
	return false
}

// providerEnabled maps a provider id to its toggle (python
// enabled_map).
func (m *Manager) providerEnabled(id string) bool {
	switch id {
	case ProviderAniSkip:
		return true
	case ProviderAnimeSkip:
		return m.cfg.AnimeSkipEnabled
	case ProviderIntroSkipper:
		return m.cfg.IntroSkipperEnabled
	default:
		return false
	}
}

// Resolve runs the provider orchestration for one episode and renders
// the merged FFMETADATA content. Errors are reserved for the case where
// every consulted provider failed; clean empty answers yield an empty
// bundle with Details "no_provider_result".
func (m *Manager) Resolve(ctx context.Context, req ResolveRequest) (Bundle, error) {
	var (
		sets        [][]Interval
		providerIDs []string
		errs        []error
	)

	for _, id := range m.ActiveOrder() {
		if !m.providerEnabled(id) {
			continue
		}

		switch id {
		case ProviderAniSkip, ProviderAnimeSkip:
			if req.ShikimoriID == 0 {
				continue
			}
			intervals, err := m.queryAPIProvider(ctx, id, req)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", id, err))
				continue
			}
			sets = append(sets, intervals)
			providerIDs = append(providerIDs, id)
		case ProviderIntroSkipper:
			// Local-only: needs a media file and never runs for URLs;
			// also only consulted when the API merge found nothing, so
			// it is deferred below.
		}
	}

	merged, contributors := MergeByType(sets)
	if len(merged) > 0 {
		bundle := Bundle{
			FFMetadata:   GenerateFFMetadata(merged),
			ChapterTypes: chapterTypes(merged),
			Details:      "ok",
		}
		switch len(contributors) {
		case 0:
			bundle.ProviderID = ""
		case 1:
			bundle.ProviderID = providerIDs[0]
		default:
			bundle.ProviderID = ProviderIDMerged
		}
		if len(errs) > 0 {
			bundle.Details = fmt.Sprintf("ok (degraded: %v)", joinErrors(errs))
		}
		return bundle, nil
	}

	// API providers empty: fall back to the local detector.
	if m.providerEnabled(ProviderIntroSkipper) && req.MediaInput != "" {
		intervals, details, err := m.intro.GetSkipTimes(ctx, req.MediaInput)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ProviderIntroSkipper, err))
		} else if len(intervals) > 0 {
			return Bundle{
				FFMetadata:   GenerateFFMetadata(intervals),
				ChapterTypes: chapterTypes(intervals),
				ProviderID:   ProviderIntroSkipper,
				Details:      details,
			}, nil
		}
	}

	if len(errs) > 0 && m.allConsultedFailed(errs) {
		return Bundle{Details: joinErrors(errs)}, errors.Join(errs...)
	}
	return Bundle{Details: "no_provider_result"}, nil
}

// queryAPIProvider fetches intervals from one API provider.
func (m *Manager) queryAPIProvider(ctx context.Context, id string, req ResolveRequest) ([]Interval, error) {
	if id == ProviderAnimeSkip {
		return m.animeSkip.GetSkipTimes(ctx, req.ShikimoriID, req.EpisodeNum)
	}
	return m.aniskip.GetSkipTimes(ctx, req.ShikimoriID, req.EpisodeNum)
}

// allConsultedFailed reports whether hard errors outnumber any clean
// provider completion. With no sets collected at all and any error
// present, the run is treated as failed (loud aggregate).
func (m *Manager) allConsultedFailed(errs []error) bool {
	return len(errs) > 0
}

// chapterTypes extracts the sorted unique skip types.
func chapterTypes(intervals []Interval) []string {
	seen := make(map[string]struct{}, len(intervals))
	var types []string
	for _, iv := range intervals {
		if _, dup := seen[iv.SkipType]; dup {
			continue
		}
		seen[iv.SkipType] = struct{}{}
		types = append(types, iv.SkipType)
	}
	sort.Strings(types)
	return types
}

// joinErrors renders collected provider errors for Details.
func joinErrors(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}
