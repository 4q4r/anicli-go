package providers

// The search-in-Lua torrent hybrid (PR144): the bundled Lua script
// owns the provider's SEARCH surface (the surfaced magnet links and
// their metadata are byte-faithful with the compiled Go provider the
// script replaced), while the torrent engine legs stay Go — the PR144
// owner ruling keeps internal/torrent (the anacrolix core) out of the
// sandbox, and the engine consumes the surfaced links unchanged.
//
// The hybrid embeds TorrentBase for exactly the legs the compiled
// provider had: EpisodesWait (the bounded metadata wait) and
// ResolveStream (the loopback-server playback). The registry's
// SetEngine walk reaches the embedded base through method promotion,
// so the wiring the compiled torrent factories enjoyed applies
// unchanged; TorrentProviderIDs sees IsTorrent and the parity smoke
// keeps the torrent pass rule.

import (
	"context"
	"log/slog"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// luaTorrentHybrids names the bundled Lua scripts whose surfaced
// links resolve through the Go torrent engine: the factory wraps
// their roster slots in the hybrid. The ids double as the set whose
// slots honor the [torrent]-disabled rule even when a script shadows
// them (a shadowed slot that cannot run must not register — the
// kodik-parity rule, never a provider that cannot run).
var luaTorrentHybrids = map[string]bool{
	"subsplease": true,
}

// luaTorrentProvider serves one Lua-scripted torrent provider: the
// script surfaces the links (Search), TorrentBase consumes them
// (GetEpisodes/ResolveStream). The optional capability surfaces
// (content language, name preference, smoke query) forward to the
// wrapped script provider so the registry and the parity smoke probe
// the declarations the script carries.
type luaTorrentProvider struct {
	contracts.Provider
	*TorrentBase
}

// newLuaTorrentProvider wraps one script provider in the torrent
// engine adapter. The engine arrives later through SetEngine (the
// registry injects the shared engine when [torrent] is enabled; nil
// fails loud on use — the TorrentBase convention).
func newLuaTorrentProvider(script contracts.Provider) *luaTorrentProvider {
	return &luaTorrentProvider{
		Provider:    script,
		TorrentBase: NewTorrentBase(nil),
	}
}

// IsTorrent implements contracts.TorrentProvider: the surfaced links
// resolve through the embedded core, not the HTTP-embed pipeline.
func (p *luaTorrentProvider) IsTorrent() bool { return true }

// GetEpisodes ingests the result's magnet and waits — bounded by the
// caller's context — for the engine's metadata fetch (the
// TorrentBase contract the compiled provider rode verbatim).
func (p *luaTorrentProvider) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	return p.EpisodesWait(ctx, animeURL)
}

// ResolveStream resolves playback through the torrent core: the link
// rides the episode's RawEmbeds, the stream points at the loopback
// server.
func (p *luaTorrentProvider) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	return p.TorrentBase.ResolveStream(episode, dubID)
}

// ContentLanguage forwards the script's content_lang declaration.
func (p *luaTorrentProvider) ContentLanguage() string {
	if c, ok := p.Provider.(interface{ ContentLanguage() string }); ok {
		return c.ContentLanguage()
	}
	return ""
}

// NamePreference forwards the script's name_preference declaration.
func (p *luaTorrentProvider) NamePreference() contracts.NamePreference {
	if np, ok := p.Provider.(contracts.NamePreferenceProvider); ok {
		return np.NamePreference()
	}
	return contracts.NamePrefDefault
}

// SmokeQuery forwards the script's smoke_query declaration.
func (p *luaTorrentProvider) SmokeQuery() string {
	if sq, ok := p.Provider.(contracts.SmokeQueryProvider); ok {
		return sq.SmokeQuery()
	}
	return ""
}

// SetLogger routes the wrapped script engine's diagnostics to log
// (the registry's logger seam; the duck assertion keeps the wrap
// honest when the script provider predates the seam).
func (p *luaTorrentProvider) SetLogger(log *slog.Logger) {
	if se, ok := p.Provider.(interface{ SetLogger(*slog.Logger) }); ok {
		se.SetLogger(log)
	}
}
