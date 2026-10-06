package providers

// The Lua torrent hybrid (PR145): the FIRST torrent provider on the
// bundled-script path. The script serves SEARCH only — the surfaced
// .torrent/magnet links feed the shared engine, whose ingest,
// metadata wait and loopback resolve stay Go (the owner ruling: the
// engine consumes the surfaced links unchanged). The factory wraps
// the loaded script provider in luaTorrentProvider the moment the
// script declares torrent = true, so the roster slot keeps the
// contracts.TorrentProvider capability the registry, the TUI and the
// parity smoke's torrent rule route by.

import (
	"context"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/lua"
	"github.com/an0nx/anicli-go/internal/torrent"
)

// luaTorrentProvider is a Lua-served torrent provider: identity and
// search delegate to the script surface (the capability adapter
// included), the episode/stream legs ride the embedded TorrentBase.
// The engine stays nil until the registry injects it (SetEngine) —
// the same fail-loud-on-use convention the compiled torrent
// providers carry. The optional capability surfaces are re-surfaced
// explicitly: embedding the contracts.Provider INTERFACE promotes
// only the core methods, and the adapter's extras (ContentLanguage &
// co) would be hidden from the registry's duck checks otherwise. The
// zero values are observationally identical to not-implemented (the
// caps.go composite doctrine).
type luaTorrentProvider struct {
	contracts.Provider
	*TorrentBase

	contentLang string
	smokeQuery  string
	namePref    contracts.NamePreference
}

// Compile-time proof the hybrid keeps the full torrent capability.
var (
	_ contracts.TorrentProvider               = luaTorrentProvider{}
	_ interface{ SetEngine(*torrent.Engine) } = luaTorrentProvider{}
)

// newLuaTorrent wraps one loaded script provider (already through the
// capability adapter, so declared surfaces like content_lang
// survive) with the Go torrent legs over a not-yet-wired engine.
func newLuaTorrent(p contracts.Provider) luaTorrentProvider {
	h := luaTorrentProvider{Provider: p, TorrentBase: NewTorrentBase(nil)}
	if lc, ok := p.(interface{ ContentLanguage() string }); ok {
		h.contentLang = lc.ContentLanguage()
	}
	if sq, ok := p.(contracts.SmokeQueryProvider); ok {
		h.smokeQuery = sq.SmokeQuery()
	}
	if np, ok := p.(contracts.NamePreferenceProvider); ok {
		h.namePref = np.NamePreference()
	}
	return h
}

// ContentLanguage re-surfaces the script's declared content language
// (the [RU]/[JA] dub tags derive from it).
func (p luaTorrentProvider) ContentLanguage() string { return p.contentLang }

// SmokeQuery re-surfaces the script's declared probe (the parity
// smoke routes by it).
func (p luaTorrentProvider) SmokeQuery() string { return p.smokeQuery }

// NamePreference re-surfaces the script's declared search-name
// routing.
func (p luaTorrentProvider) NamePreference() contracts.NamePreference { return p.namePref }

// IsTorrent implements contracts.TorrentProvider: the surfaced
// results are torrent links the engine resolves.
func (p luaTorrentProvider) IsTorrent() bool { return true }

// GetEpisodes ingests the result's torrent link and waits — bounded
// by the caller's context — for the engine's metadata fetch (the
// TorrentBase contract). The script's episodes function is never
// reached on this path.
func (p luaTorrentProvider) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p luaTorrentProvider) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}

// wrapLuaTorrentProviders re-homes the episode/stream legs of every
// torrent-declaring script provider onto the shared TorrentBase (the
// anilibria-torrent hybrid; generic over the declaration, never over
// a hard-coded id). The Lua peel only DECIDES — the wrap keeps the
// outer provider so declared capabilities survive.
func wrapLuaTorrentProviders(provs map[string]contracts.Provider) {
	for id, p := range provs {
		lp, ok := lua.ScriptProvider(p)
		if !ok || !lp.Torrent() {
			continue
		}
		provs[id] = newLuaTorrent(p)
	}
}
