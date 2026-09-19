package providers

import (
	"fmt"
	"log/slog"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/storage"
)

// allFactories lists the provider constructors in registry order. Each
// entry gets its own netclient client: providers never share cookie
// jars, and errors are tagged with the provider id. The build function
// receives the full settings plus the shared CF manager (nil when
// [cf] is disabled): wave-2 providers consume per-provider
// configuration (kodik's API token), allanime consumes the browser
// bridge.
var allFactories = []struct {
	id    string
	build func(http *netclient.Client, cfg config.Settings, cf *cfbrowser.Manager) contracts.Provider
}{
	{"anilibria", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnilibria(AniLibriaAPIBase, AniLibriaHost, http)
	}},
	{"animevost", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimevost(AnimeVostBase, http)
	}},
	{"anilib", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnilib(AnilibAPIBase, http)
	}},
	{"animego", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimego(AnimeGoBase, http)
	}},
	{"gogoanime", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newGogoAnime(GogoAnimeBase, http)
	}},
	{"animepahe", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimePahe(AnimePaheBase, http)
	}},
	// kickassanime (PR58): the kaa.lt JSON API (fsearch → show →
	// paginated episodes → per-episode servers on the krussdomi HLS
	// edge). No credentials and no per-provider settings;
	// network.proxy_url routes it from blocked networks like every
	// foreign site.
	{"kickassanime", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newKickassanime(KickassAnimeBase, http, cfg.Network.MaxParallel)
	}},
	// anizone (PR59): the anizone.to sub-only stream source — the first
	// provider with no frozen Python original, written from the
	// Anivexa-API AniZone recipe (providers/anizone.js) re-verified live
	// 2026-09-18. Livewire HTML payloads, /livewire/update episode
	// pagination and vidstackPlayer HLS on the watch page; no
	// credentials.
	{"anizone", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniZone(AniZoneBase, http)
	}},
	{"dreamcast", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newDreamCast(DreamCastBase, http)
	}},
	{"sameband", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newSameBand(SameBandBase, http)
	}},
	{"kodik", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newKodik(KodikAPIBase, cfg.Providers.Kodik.Token, http)
	}},
	{"allanime", func(http *netclient.Client, cfg config.Settings, cf *cfbrowser.Manager) contracts.Provider {
		return newAllAnime(AllAnimeAPIBase, AllAnimeReferer, AllAnimeInternalBase, http, buildAABridge(cf), cacheDirFor(cfg))
	}},
	{"anidub", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnidub(AnidubBase, http)
	}},
	{"yanima", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newYanima(YanimaBase, cfg.Providers.Yanima.DDoSP1,
			cfg.Providers.Yanima.DDoSP2, cfg.Providers.Yanima.Session, http)
	}},
	// animedia (PR56): the amd.online DLE site (the animedia.online
	// JSON v3 API is dead). Written against the live site, not ported;
	// no credentials — DLE search form POST in, kodik embeds out
	// (resolved through the shared extractor factory).
	{"animedia", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniMedia(AniMediaBase, http)
	}},
	// shiza (PR57): the shizaproject.com GraphQL on the anidub stream
	// plumbing — anonymous catalog search, kodik/sibnet embeds through
	// the shared extractor factory. No credentials; its torrent
	// entries are dead (0 seeders, see shiza.go) so no torrent
	// sibling is registered.
	{"shiza", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newShiza(ShizaBase, http)
	}},
	// anime365 (PR55): the smotret-anime (anime365.ru) documented JSON
	// API — open catalog/episodes/translations, tokened embed
	// resolution (account with an active subscription). No frozen
	// Python original (anidub precedent); written against the live
	// API + the official OpenAPI spec (probed 2026-09-18).
	{"anime365", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnime365(Anime365Mirrors, cfg.Providers.Anime365.Token, http)
	}},
	// hdrezka (PR69): hdrezka-home.tv (RU rezka catalog, anime section) —
	// port of the frozen anicli-api hdrezka source re-verified live
	// 2026-09-19, PLUS a pure-Go Anubis proof-of-work gate solver: the
	// site fronts every path with Anubis 1.25.0 and the upstream
	// reference crashes against it. No credentials; translators are the
	// dubs (one-voice included), hdrezka's own CDN resolves to
	// HLS/mp4. From ISP-blocked networks network.proxy_url routes it
	// (foreign hosting, Roskomnadzor-class SNI block on the direct
	// route); stream links additionally require a site-accepted exit
	// (see hdrezka.go).
	{"hdrezka", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newHDRezka(HDRezkaBase, http)
	}},
	// nyaa (PR36): the first torrent search provider. No credentials
	// and no per-provider settings; the shared torrent engine is
	// injected by NewRegistry when [torrent] is enabled (All() leaves
	// it nil — the base fails loud until wired).
	{"nyaa", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newNyaa(NyaaBase, http, nil)
	}},
	// anilibria-torrent (PR37): the aniliberty.top API's per-release
	// torrents on the same TorrentBase plumbing. Shares the release
	// search endpoint with the anilibria stream provider and expands
	// each hit into its torrent list; no credentials, engine injected
	// by NewRegistry when [torrent] is enabled.
	{"anilibria-torrent", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnilibriaTorrent(AniLibriaAPIBase, http, nil)
	}},
	// animetosho (PR38): the animetosho.org newznab search on the same
	// TorrentBase plumbing — hex-infohash magnets, .torrent enclosure
	// fallback; no credentials, engine injected by NewRegistry when
	// [torrent] is enabled.
	{"animetosho", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimeTosho(AnimeToshoFeedBase, http, nil)
	}},
	// tokyotosho (PR38): the tokyo-tosho.net search RSS on the same
	// TorrentBase plumbing — direct .torrent <link> URLs; no
	// credentials, engine injected by NewRegistry when [torrent] is
	// enabled.
	{"tokyotosho", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newTokyoTosho(TokyoToshoBase, http, nil)
	}},
}

// buildAABridge wires the AllAnime crypto bridge when [cf].enabled
// supplies a stealth browser (nil otherwise — pure-Go typed errors).
func buildAABridge(cf *cfbrowser.Manager) aaBridgeSource {
	if cf == nil || cf.Solver == nil {
		return nil
	}
	return &aaCFBrowserBridge{Solver: cf.Solver, RootURL: AllAnimeReferer + "/", Lane: aaContentLane}
}

// registryOptions carries the NewRegistry customizations.
type registryOptions struct {
	// torrentLogger routes the shared torrent engine's diagnostics;
	// nil keeps the engine default (slog.Default).
	torrentLogger *slog.Logger
	// providerLogger routes provider-level diagnostics (search
	// preflight drops, …); nil degrades to discard inside the
	// provider — never stderr (PR62 #4).
	providerLogger *slog.Logger
}

// RegistryOption customizes NewRegistry.
type RegistryOption func(*registryOptions)

// WithTorrentLogger routes the shared torrent engine's diagnostics to
// log. The TUI passes its file logger here (stderr corrupts
// alt-screen); a nil logger keeps the engine default.
func WithTorrentLogger(log *slog.Logger) RegistryOption {
	return func(o *registryOptions) { o.torrentLogger = log }
}

// WithProviderLogger routes provider-level diagnostics (search
// preflight drops, …) to log. The TUI passes its file logger here
// (stderr corrupts alt-screen, PR62 #4); a nil logger degrades to
// discard inside the provider — never slog.Default.
func WithProviderLogger(log *slog.Logger) RegistryOption {
	return func(o *registryOptions) { o.providerLogger = log }
}

// cacheDirFor resolves the persistent cache directory for provider
// state ("" when unset — in-memory).
func cacheDirFor(cfg config.Settings) string {
	if cfg.General.DataDir != "" {
		return cfg.General.DataDir
	}
	dir, err := config.DataDir()
	if err != nil {
		return ""
	}
	return dir
}

// All builds the provider set from cfg: one netclient client each
// (browser-fingerprint profile, own cookie jar, provider-tagged errors)
// constructed from cfg.Network, plus per-provider settings where a
// source needs them (kodik's token). Providers listed in
// [providers].exclude are skipped (PR23). Grow allFactories as later
// waves land.
func All(cfg config.Settings) ([]contracts.Provider, error) {
	return all(cfg, nil)
}

// all is All with extra netclient options applied to every client
// (the CF solver wiring).
func all(cfg config.Settings, extra []netclient.Option) ([]contracts.Provider, error) {
	return allWithCF(cfg, extra, nil)
}

// allWithCF is all with the shared CF manager handed to providers that
// need browser capabilities (the AllAnime crypto bridge). Providers
// whose id is listed in [providers].exclude are skipped entirely — no
// client, no registry slot — and the exclusion is logged at startup
// (PR23). Providers that cannot run without user configuration (kodik
// without a token) are skipped the same way and returned in the
// disabled set (PR24).
func allWithCF(cfg config.Settings, extra []netclient.Option, cf *cfbrowser.Manager) ([]contracts.Provider, error) {
	out, _, err := allWithCFDisabled(cfg, extra, cf)
	return out, err
}

// allWithCFDisabled is allWithCF that also returns the
// unconfigured-provider set for registry bookkeeping.
func allWithCFDisabled(cfg config.Settings, extra []netclient.Option, cf *cfbrowser.Manager) ([]contracts.Provider, []DisabledProvider, error) {
	excluded := make(map[string]bool, len(cfg.Providers.Exclude))
	for _, id := range cfg.Providers.Exclude {
		excluded[id] = true
	}
	disabledMap := unconfiguredIDs(cfg)
	out := make([]contracts.Provider, 0, len(allFactories))
	for _, factory := range allFactories {
		if excluded[factory.id] {
			slog.Info("provider excluded: " + factory.id)
			continue
		}
		if d, off := disabledMap[factory.id]; off {
			// The red startup notice already covers this; slog would
			// duplicate the line next to user-facing output.
			_ = d
			continue
		}
		opts := append([]netclient.Option{netclient.WithProvider(factory.id)}, extra...)
		client, err := netclient.New(cfg.Network, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("build %s client: %w", factory.id, err)
		}
		out = append(out, factory.build(client, cfg, cf))
	}
	disabled := make([]DisabledProvider, 0, len(disabledMap))
	for _, d := range disabledMap {
		disabled = append(disabled, d)
	}
	sortDisabled(disabled)
	return out, disabled, nil
}

// sortDisabled orders the disabled set by provider id for stable
// output.
func sortDisabled(disabled []DisabledProvider) {
	for i := 1; i < len(disabled); i++ {
		for j := i; j > 0 && disabled[j].ID < disabled[j-1].ID; j-- {
			disabled[j], disabled[j-1] = disabled[j-1], disabled[j]
		}
	}
}

// NewRegistry builds the full provider set with every provider wrapped
// in a SearchDelegator recording into stats, and — when
// [providers].exclude_streams is configured — in a dub stream filter
// that drops trash streams from episode listings (PR23). stats may be
// nil: searches then simply are not recorded. When [cf].enabled the CF
// challenge ladder is wired into every client; Close releases it.
// When [torrent].enabled the registry also builds the ONE shared lazy
// torrent engine, injects it into every torrent provider (SetEngine)
// and owns its teardown (PR36): the TUI reuses the same engine via
// TorrentEngine instead of booting a second client.
func NewRegistry(cfg config.Settings, stats *storage.ProviderStatRepo, opts ...RegistryOption) (*Registry, error) {
	var o registryOptions
	for _, opt := range opts {
		opt(&o)
	}

	filter, err := NewStreamFilter(cfg.Providers.ExcludeStreams)
	if err != nil {
		return nil, err
	}

	cfMgr, err := cfbrowser.NewManager(cfg)
	if err != nil {
		return nil, err
	}
	cfOpts, cfClose, err := buildCFOptions(cfg, cfMgr)
	if err != nil {
		return nil, err
	}
	bare, disabled, err := allWithCFDisabled(cfg, cfOpts, cfMgr)
	if err != nil {
		return nil, err
	}

	reg := NewEmptyRegistry()
	if cfg.Torrent.Enabled {
		if err := reg.wireTorrentEngine(cfg, bare, o.torrentLogger); err != nil {
			return nil, err
		}
	}
	// PR62 #4: every provider carrying the Base logger seam gets the
	// configured sink (SetLogger probe, the SetEngine pattern); nil
	// degrades to discard inside the provider — never stderr.
	providerLog := o.providerLogger
	if providerLog == nil {
		providerLog = discardLogger
	}
	for _, p := range bare {
		if se, ok := p.(interface{ SetLogger(*slog.Logger) }); ok {
			se.SetLogger(providerLog)
		}
	}
	for _, p := range bare {
		if filter != nil {
			p = dubFilteredProvider{Provider: p, filter: filter}
		}
		if err := reg.Register(SearchDelegator{Provider: p, stats: stats, logger: providerLog}); err != nil {
			return nil, err
		}
	}
	reg.disabled = disabled
	reg.cfClose = cfClose
	return reg, nil
}
