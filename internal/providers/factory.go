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
// receives the full settings plus the shared CF manager (always built
// since PR80; nil only for callers that skip NewManager): wave-2
// providers consume per-provider configuration (kodik's API token),
// allanime consumes the browser bridge.
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
	// animepahe (PR71): the site ops ride the [cf] browser bridge (the
	// serving origin re-challenges non-browser fingerprints even with a
	// replayed clearance — PR71 dossier A/B); nil bridge keeps the
	// netclient + CF-ladder path for [cf]-disabled configs.
	{"animepahe", func(http *netclient.Client, _ config.Settings, cf *cfbrowser.Manager) contracts.Provider {
		return newAnimePahe(AnimePaheBase, http, buildPaheBridge(cf))
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
	// anicrush (PR90): the EN streaming catalog anicrush.to — written
	// from three independent wrapper implementations of its anonymous
	// JSON API (DrBrainlessLol/anicrush-api, shimizudev/anicrush-api,
	// gojo). NOT live-verified: the whole anicrush.to family has been
	// origin-dead behind Cloudflare (edge-served 521 for every vantage)
	// since ~2026-08-07, so the wire shapes are pinned by the reference
	// sources and the embed→HLS hop (a rabbit/megacloud WASM player)
	// stays with the shared extractor factory, failing typed until an
	// extractor lands. No credentials.
	{"anicrush", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniCrush(AniCrushAPIBase, http)
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
	// yummy (PR68): the YummyAnime REST API (api.yani.tv behind
	// site.yummyani.me) — the first provider ported from the vypivshiy
	// anicli-api reference library (source/yummy_anime.py), verified
	// live 2026-09-19. No credentials and no per-provider settings;
	// cfg.Network.UserAgent rides on the CVH video sources (okcdn ties
	// playback to the extraction UA).
	{"yummy", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newYummy(YummySiteBase, YummyAPIBase, yummyCDNVideoHubBase, cfg.Network.UserAgent, http)
	}},
	// hdrezka (PR69): the RU rezka catalog's anime section — port of
	// the frozen anicli-api hdrezka source, PLUS a pure-Go Anubis
	// proof-of-work gate solver the site fronts every path with (see
	// hdrezka.go). PR72 route matrix: the family geo-fences per domain
	// (hdrezka-home.tv withholds stream links from datacenter exits —
	// its session JWT attests geo:"de" — while rezka-ua.tv serves
	// them), so the built-in default pins the serving mirror and
	// [providers.hdrezka] base_url re-points it without a rebuild. No
	// credentials; translators are the dubs (one-voice included),
	// hdrezka's own CDN resolves to HLS/mp4. From ISP-blocked networks
	// network.proxy_url routes it (foreign hosting, SNI-blocked direct
	// route — verified killed mid-TLS on a RU-intercepted network).
	{"hdrezka", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		base := cfg.Providers.HDRezka.BaseURL
		if base == "" {
			base = HDRezkaBase
		}
		return newHDRezka(base, http)
	}},
	// anistar (PR77): the anistar.org DLE catalog with its self-hosted
	// an-media.org player stack — the roster's first Windows-1251 site
	// (search form POST and page bodies both ride cp1251). Written
	// from the live site, not ported; no credentials. The p2p player
	// page exposes direct per-quality HLS/MP4 links behind a media_id;
	// the an-media edge requires the site Referer on playback.
	{"anistar", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniStar(AniStarBase, http)
	}},
	// anifilm (PR91): the anifilm.pro RU stream+torrent catalog — a
	// custom Yii/Vue engine, NOT DLE. Written from the live site
	// (2026-09-23); no credentials: GET-form search, Vue
	// player-component props → api:online playlists → api:video pages
	// wrapping kodik embeds (shared extractor). The per-release
	// .torrent downloads are a TorrentBase extension candidate,
	// deliberately out of scope here.
	{"anifilm", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniFilm(AniFilmBase, http)
	}},
	// animemobi (PR92): the animemobi.com RU mobile catalog (DLE, UTF-8,
	// anonymous) — search POST form in, per-episode kodik-family embeds
	// out (kodikplayer.com and aniqit.com, both covered by the shared
	// kodik extractor); the release pages additionally carry per-release
	// .torrent downloads (documented in animemobi.go, out of the stream
	// contract). No frozen Python original; written from the live site.
	{"animemobi", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimeMobi(AnimeMobiBase, http)
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
	// rutor (PR87): the rutor.info public tracker's HTML search on the
	// same TorrentBase plumbing — the fifth torrent provider and the
	// first RU-indexed one (the Jackett rutor.yml recipe, re-verified
	// live 2026-09-23). Fully anonymous (search and .torrent
	// downloads); the engine is injected by NewRegistry when
	// [torrent] is enabled.
	{"rutor", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newRutor(RutorBase, http, nil)
	}},
	// anirena (PR88): the anirena.com search RSS on the same
	// TorrentBase plumbing — the <enclosure> is the direct
	// .torrent URL on the site itself, the Anime category scope is
	// enforced client-side (the documented ?category= filter is
	// ignored server-side, live-verified 2026-09-23); no credentials,
	// engine injected by NewRegistry when [torrent] is enabled.
	{"anirena", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniRena(AniRenaBase, http, nil)
	}},
	// subsplease (PR89): the subsplease.org JSON API on the same
	// TorrentBase plumbing — the EN seasonal group's f=search catalog
	// (the RSS feeds are latest-only and queryless, the site search
	// endpoint is the API) with tracker-rich magnet links and the
	// show-page sid hop for batch back-catalog; no credentials, engine
	// injected by NewRegistry when [torrent] is enabled.
	{"subsplease", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newSubsPlease(SubsPleaseBase, http, nil)
	}},
}

// buildAABridge wires the AllAnime crypto bridge (CF is always on —
// the manager always supplies a stealth browser; a nil-manager call
// remains guarded for API users who skip NewManager).
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
	// cfBrowserLogger is the cfbrowser diagnostics sink (PR85); nil
	// degrades to discard inside cfbrowser — never stderr.
	cfBrowserLogger *slog.Logger
}

// RegistryOption customizes NewRegistry.
type RegistryOption func(*registryOptions)

// WithTorrentLogger routes the shared torrent engine's diagnostics to
// log. The TUI passes its file logger here (stderr corrupts
// alt-screen); a nil logger keeps the engine default.
func WithTorrentLogger(log *slog.Logger) RegistryOption {
	return func(o *registryOptions) { o.torrentLogger = log }
}

// WithCFBrowserLogger routes the CF-bypass stack's diagnostics (the
// solver, updater, verdict store and install ladder) to log (PR85).
func WithCFBrowserLogger(log *slog.Logger) RegistryOption {
	return func(o *registryOptions) { o.cfBrowserLogger = log }
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
// nil: searches then simply are not recorded. The CF challenge ladder
// is always wired into every client (PR80); Close releases it.
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

	cfMgr, err := cfbrowser.NewManager(cfg, cfbrowser.WithManagerLogger(o.cfBrowserLogger))
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
	reg.cfMgr = cfMgr
	return reg, nil
}
