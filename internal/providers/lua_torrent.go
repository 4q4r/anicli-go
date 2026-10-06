package providers

// luaTorrent adapts a Lua-served TORRENT roster slot onto the shared
// TorrentBase plumbing (PR143, the anirena migration): the bundled
// script serves the search surface only, and the adapter owns
// everything the sandbox cannot carry — the PR66 dead-host .torrent
// preflight, the engine ingestion, the EpisodesWait resolve and the
// loopback stream. The torrent ENGINE (internal/torrent, anacrolix)
// stays Go and consumes the surfaced links unchanged; the adapter is
// byte-identical to what the compiled torrent providers' Search did
// in Go code (preflight the survivors, feed the engine, no re-fetch).
//
// The capability surfaces are FORWARDED EXPLICITLY (the caps.go
// lesson: interface embedding hides the inner provider's concrete
// methods — ContentLanguage, NamePreference, SmokeQuery and SetLogger
// would silently vanish behind the wrapper otherwise).

import (
	"context"
	"log/slog"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// luaTorrent wraps one Lua provider carrying a torrent slot. The
// embedded contracts.Provider forwards the base surface (ID, Name,
// BaseURL, SourceType, and the Search/GetEpisodes/ResolveStream
// methods this type overrides); TorrentBase carries the engine
// plumbing (the registry's SetEngine duck injection lands there).
type luaTorrent struct {
	contracts.Provider
	*TorrentBase

	// preflightBudget overrides the per-URL preflight budget (tests);
	// 0 keeps torrentPreflightTimeout.
	preflightBudget time.Duration

	log *slog.Logger
}

// newLuaTorrent wraps p: the engine arrives later (the registry
// injects it when [torrent] is enabled), the transport is pulled from
// the inner provider at search time (its own netclient — the
// one-transport-per-provider isolation).
func newLuaTorrent(p contracts.Provider) *luaTorrent {
	return &luaTorrent{
		Provider:    p,
		TorrentBase: NewTorrentBase(nil),
		log:         discardLogger,
	}
}

// IsTorrent implements contracts.TorrentProvider.
func (a *luaTorrent) IsTorrent() bool { return true }

// httpClient pulls the inner provider's transport (the lua.Provider
// HTTPClient seam). Nil — a provider built without a wired transport
// (hand-built tests, plain sandboxes) — skips the preflight: nothing
// to probe with (unreachable in production wiring, the factory always
// builds one).
func (a *luaTorrent) httpClient() *netclient.Client {
	hc, ok := a.Provider.(interface{ HTTPClient() *netclient.Client })
	if !ok {
		return nil
	}
	return hc.HTTPClient()
}

// Search runs the script's search surface, then the shared PR66
// preflight: every survivor's .torrent bytes are fetched bounded-
// concurrent, parsed as metainfo and handed to the engine under the
// original link — dead hosts never surface, and the resolve leg never
// re-fetches. An inner search failure is the caller's error,
// untouched.
func (a *luaTorrent) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	results, err := a.Provider.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	client := a.httpClient()
	if client == nil {
		return results, nil
	}
	budget := a.preflightBudget
	if budget <= 0 {
		budget = torrentPreflightTimeout
	}
	return a.preflightResults(ctx, client, a.log, a.ID(), budget, results), nil
}

// GetEpisodes rides the base's bounded metadata wait (the search
// result resolves long after the search; unreachable metadata fails
// loud on the caller's deadline, never silent-empty).
func (a *luaTorrent) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return a.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (a *luaTorrent) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return a.TorrentBase.ResolveStream(episode, dubID)
}

// SetEngine forwards the registry's engine injection into the base.
func (a *luaTorrent) SetEngine(engine *torrent.Engine) {
	a.TorrentBase.SetEngine(engine)
}

// SetLogger re-routes the preflight diagnostics AND the inner Lua
// engine's logs (the registry threads its provider sink through this
// seam; nil keeps the current routing).
func (a *luaTorrent) SetLogger(log *slog.Logger) {
	if log == nil {
		return
	}
	a.log = log
	if sl, ok := a.Provider.(interface{ SetLogger(*slog.Logger) }); ok {
		sl.SetLogger(log)
	}
}

// ContentLanguage forwards the script's declaration ("" when the
// inner provider declares none — observationally identical to
// not-implemented at every consumer).
func (a *luaTorrent) ContentLanguage() string {
	if lc, ok := a.Provider.(interface{ ContentLanguage() string }); ok {
		return lc.ContentLanguage()
	}
	return ""
}

// NamePreference forwards the script's declaration (NamePrefDefault
// when undeclared).
func (a *luaTorrent) NamePreference() contracts.NamePreference {
	if np, ok := a.Provider.(contracts.NamePreferenceProvider); ok {
		return np.NamePreference()
	}
	return contracts.NamePrefDefault
}

// SmokeQuery forwards the script's declaration ("" when undeclared —
// the parity smoke keeps the shared probes).
func (a *luaTorrent) SmokeQuery() string {
	if sq, ok := a.Provider.(contracts.SmokeQueryProvider); ok {
		return sq.SmokeQuery()
	}
	return ""
}
