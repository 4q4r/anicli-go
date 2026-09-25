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
// providers consume per-provider configuration (kodik's API token).
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
	{"sameband", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newSameBand(SameBandBase, http)
	}},
	{"kodik", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newKodik(KodikAPIBase, cfg.Providers.Kodik.Token, http)
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
	// anitokyo (PR100): the anitokyo.tv RU DLE catalog with its
	// RalodePlayer module — the release page embeds ONE JSON blob with
	// every (dub, episode) pair (60-dub seasons hydrate from a single
	// fetch); stream refs are the site's own /video.php wrappers scraping
	// to kodik/sibnet embeds (shared extractor factory). Written from the
	// live site (2026-09-25); anonymous on every leg; no credentials, no
	// per-provider settings.
	{"anitokyo", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniTokyo(AniTokyoBase, http)
	}},
	// animiku (PR101): the beta.animiku.tokyo RU catalog (DLE under a
	// custom template, UTF-8, anonymous; live-verified 2026-09-25) —
	// search GET form in, the mrdeath/aaparser player bridge
	// (POST engine/ajax/controller.php?mod=anime_grabber&module=
	// kodik_playlist_ajax) out: translator row = dubs, episode grid =
	// per-(episode, dub) kodikplayer.com embeds through the shared
	// extractor. The advertised 4K/FHD tiers are runtime JS resolvers
	// (anilibria.top API by title) with no deterministic embed URLs —
	// documented in animiku.go, out of the stream contract. No frozen
	// Python original; written from the live site.
	{"animiku", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimiku(AniMikuBase, http)
	}},
	// anikado (PR102): the anikado.net RU catalog (DLE, UTF-8,
	// anonymous) — search POST form in, episodes off the title page's
	// server-rendered anchor list, per-(episode, dub) kodik embeds off
	// each episode page's b-translator__item table (fan-out bounded by
	// network.max_parallel, the kickassanime pattern); movies carry
	// their kodik /video/ embed directly in the title page's kodik tab.
	// The title page's vkg/tomion fallback tabs are client-side
	// hydrated or frame-gated — not anonymously resolvable, documented
	// walls. kodik.info embed hosts normalize onto the interchangeable
	// kodikplayer.com mirror (live-verified 2026-09-25). No frozen
	// Python original; written from the live site.
	{"anikado", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniKado(AniKadoBase, http, cfg.Network.MaxParallel)
	}},
	// animevib (PR103): the www.animevib.ru RU catalog — a DLE site
	// (the controller's WordPress intel was wrong: DLE's ?s= is
	// silently ignored; the real search is the index.php GET form).
	// Written from the live site (2026-09-25); no credentials. Every
	// post embeds ONE kodik player whose serial page lists the dub
	// teams (per-translation serial pages) and per-episode seria
	// hashes — the provider merges the (episode × dub) table and
	// resolves the synthesized seria embeds through the shared kodik
	// extractor. The per-translation fetch budget rides
	// cfg.Network.MaxParallel (the kickassanime pattern).
	{"animevib", func(http *netclient.Client, cfg config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimeVib(AnimeVibBase, http, cfg.Network.MaxParallel)
	}},
	// animeheaven (PR105): the animeheaven.me EN sub-only catalog —
	// direct-MP4 sources, the roster's first latin stream provider
	// since anizone. Written against the live site plus the AniVault
	// scraper family (SH0MIK/Anivault-Scraper, jsmat0m/Anivault-Scraper),
	// live-verified 2026-09-25; anonymous, NOT behind Cloudflare (the
	// reference skips FlareSolverr too): /fastsearch.php anchor cards
	// (id = href query part), /anime.php gateh/gatea episode keys
	// (the live markup's space-after-paren breaks the reference
	// regex — ours tolerates it), /gate.php with Cookie: key=<ep key>
	// → direct mp4 <source>s, first /video.mp4 source wins (the
	// site's onerror-fallback CDNs 404 when hit directly). No
	// credentials, no per-provider settings.
	{"animeheaven", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimeHeaven(AnimeHeavenBase, http)
	}},
	// anikoto (PR104): the anikototv.to EN catalog — a HiAnime/Zoro-style
	// clone (the anikoto.net platform family documented by the AniVault
	// Scraper and the PyPI anikoto downloader, live-verified 2026-09-25).
	// Written from the live site; no credentials: /filter?keyword= HTML
	// search in, the {"status":N,"result":…} AJAX envelope out (episode
	// list + SUB/DUB server groups + per-server stream resolver), and the
	// megaplay embed chain statically unpacked — XOR string table, AES-256-CBC
	// enc decrypt, HMAC-signed CDN URL — without executing any JavaScript.
	// The controller's /api/search lead is a decoy: the site answers every
	// parameter with the error envelope.
	{"anikoto", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniKoto(AniKotoBase, http)
	}},
	// anipub (PR107): the anipub.xyz EN catalog — an open Express+Mongo
	// API (github.com/AnimePub/AniPub, the site's own source; the
	// api. subdomain is static GitHub Pages, the real API rides the
	// apex host). Written from the live API + backend source
	// (2026-09-25); anonymous on every leg (the validkey middleware
	// next()s on a missing key). /api/searchAll name search in, the
	// /v1/api/details ep array out; each link is the site's own
	// /video/<n>/<sub|dub> player page wrapping a megaplay.buzz stream
	// whose same-origin getSourcesNew enc payload decrypts (static
	// AES-256-CBC params from megaplay's newclient.min.js) to the
	// master.m3u8 — no extractor factory hop. Sub and Dub emit per
	// episode (the site's own changeStreamType toggle); the megaplay
	// CDN 403s playback without the stream-origin Referer, so it rides
	// on the source.
	{"anipub", func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAniPub(AniPubBase, http)
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
// need browser-backed challenge solving (the netclient CF ladder).
// Providers
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
