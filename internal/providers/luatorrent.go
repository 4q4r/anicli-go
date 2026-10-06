package providers

// luaTorrent is the factory-built adapter that grafts the shared
// TorrentBase engine plumbing onto a Lua provider script which
// declared torrent = true (the rutor PR142 precedent — the first
// torrent Go→Lua migration). The migration boundary the owner ruled:
// the torrent ENGINE (internal/torrent, anacrolix) stays Go; the Lua
// script owns ONLY the search surface (row parsing, the seedless
// filter, the magnet/.torrent link choice, the quality badge); this
// adapter owns everything downstream of the surfaced links — the
// PR66 dead-host preflight, the metadata wait (EpisodesWait) and the
// loopback stream resolve — byte-faithfully the machinery the
// compiled torrent providers embed.
//
// The capability-forwarding note (the SearchDelegator.FetchDubs
// lesson): Go type assertions do not promote methods through a
// nested interface embed, so this adapter re-exposes the optional
// capability surfaces (ContentLanguage, NamePreference, SmokeQuery)
// by duck-asserting the wrapped provider — without the forwards the
// registry would read the wrapped Lua provider's declarations as
// absent. SetLogger forwards the same way (the script's engine sink)
// while keeping the preflight's own sink.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// luaTorrent serves one torrent-declared Lua script over the shared
// TorrentBase. The engine may be nil (fails loud on use — the
// kodik-parity not-wired convention; Search then skips the preflight,
// exactly the compiled providers' nil-engine rule).
type luaTorrent struct {
	contracts.Provider
	*TorrentBase

	// http is the preflight transport (the provider's own netclient
	// route — the same one-client isolation the compiled torrent
	// providers keep).
	http *netclient.Client

	// log receives the preflight drop reasons; nil degrades to
	// discard inside preflightResults — never stderr (PR62 #4).
	log *slog.Logger

	// preflightTimeout overrides the per-URL preflight budget
	// (tests); 0 keeps torrentPreflightTimeout.
	preflightTimeout time.Duration
}

// Compile-time proof of the consumer-side torrent contract.
var _ contracts.TorrentProvider = (*luaTorrent)(nil)

// newLuaTorrent builds the adapter over the script provider. A nil
// client falls back to the wrapped provider's HTTPClient() seam (the
// one-transport-per-provider isolation — the factory passes the same
// netclient the script's search used); the engine arrives later (the
// registry injects it when [torrent] is enabled).
func newLuaTorrent(p contracts.Provider, client *netclient.Client, engine *torrent.Engine) *luaTorrent {
	if client == nil {
		if hc, ok := p.(interface{ HTTPClient() *netclient.Client }); ok {
			client = hc.HTTPClient()
		}
	}
	return &luaTorrent{
		Provider:    p,
		TorrentBase: NewTorrentBase(engine),
		http:        client,
	}
}

// IsTorrent implements contracts.TorrentProvider: the surfaced
// results are torrent links the engine resolves (metadata+files, not
// dubs+streams).
func (p *luaTorrent) IsTorrent() bool { return true }

// Search runs the script's search surface and applies the torrent
// family rules on top: the PR44 seedless filter (defensive re-applied
// — the script filters row-level too) and the PR66 dead-host
// preflight feeding the engine. Errors keep the compiled provider's
// wrapping contract: a failure that is not already a ProviderError
// gains the provider tag (the rutorGet behavior).
func (p *luaTorrent) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	results, err := p.Provider.Search(ctx, query)
	if err != nil {
		var perr *contracts.ProviderError
		if errors.As(err, &perr) {
			return nil, err
		}
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0, err)
	}
	return p.preflight(ctx, filterSeedless(results)), nil
}

// preflightBudget is the effective per-URL fetch budget (the shared
// torrentPreflightTimeout default, owner ruling: short, ~10s).
func (p *luaTorrent) preflightBudget() time.Duration {
	if p.preflightTimeout > 0 {
		return p.preflightTimeout
	}
	return torrentPreflightTimeout
}

// preflight drops search results whose .torrent bytes are not
// fetchable and parseable (dead hosts never surface), and hands the
// surviving bytes to the engine under the original link so ingestion
// never re-fetches (the shared TorrentBase.preflightResults).
func (p *luaTorrent) preflight(ctx context.Context, results []contracts.SearchResult) []contracts.SearchResult {
	return p.preflightResults(ctx, p.http, p.loggerOrDiscard(), p.ID(), p.preflightBudget(), results)
}

// loggerOrDiscard resolves the preflight sink.
func (p *luaTorrent) loggerOrDiscard() *slog.Logger {
	if p.log != nil {
		return p.log
	}
	return discardLogger
}

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch, then
// maps the release's files onto standard episodes (the TorrentBase
// contract; single-episode releases yield one playable entry, batches
// one per file with the parsed episode numbers).
func (p *luaTorrent) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *luaTorrent) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}

// SetLogger routes the adapter's preflight diagnostics to log and
// forwards to the wrapped script's engine sink (the registry logger
// seam).
func (p *luaTorrent) SetLogger(log *slog.Logger) {
	if log == nil {
		return
	}
	p.log = log
	if fwd, ok := p.Provider.(interface{ SetLogger(*slog.Logger) }); ok {
		fwd.SetLogger(log)
	}
}

// ContentLanguage forwards the wrapped script's declaration (the
// capability surfaces do not promote through the interface embed).
func (p *luaTorrent) ContentLanguage() string {
	if cl, ok := p.Provider.(interface{ ContentLanguage() string }); ok {
		return cl.ContentLanguage()
	}
	return ""
}

// NamePreference forwards the wrapped script's declaration.
func (p *luaTorrent) NamePreference() contracts.NamePreference {
	if np, ok := p.Provider.(contracts.NamePreferenceProvider); ok {
		return np.NamePreference()
	}
	return contracts.NamePrefDefault
}

// SmokeQuery forwards the wrapped script's declaration.
func (p *luaTorrent) SmokeQuery() string {
	if sq, ok := p.Provider.(contracts.SmokeQueryProvider); ok {
		return sq.SmokeQuery()
	}
	return ""
}
