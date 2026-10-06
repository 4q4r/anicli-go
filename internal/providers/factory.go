package providers

import (
	"fmt"
	"log/slog"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/lua"
	"github.com/an0nx/anicli-go/internal/luaproviders"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/storage"
)

// allFactories lists the provider constructors in registry order. Each
// entry gets its own netclient client: providers never share cookie
// jars, and errors are tagged with the provider id. The build function
// receives the full settings plus the shared CF manager (always built
// since PR80; nil only for callers that skip NewManager): wave-2
// providers consume per-provider configuration (kodik's API token).
//
// luaOnly entries (PR116) are served by the bundled Lua script of the
// same id — the Go constructor is GONE (the script replaced it) and
// the entry only PINS THE ROSTER SLOT: when [providers.lua] is enabled
// and the script loads, the Lua provider takes the slot; otherwise the
// slot drops (the script's load skip is logged by the loader).
var allFactories = []struct {
	id      string
	luaOnly bool
	build   func(http *netclient.Client, cfg config.Settings, cf *cfbrowser.Manager) contracts.Provider
}{
	// anilibria (PR37 → PR120): the aniliberty.top RU catalog rebased
	// onto the new Laravel API in PR37, migrated to the BUNDLED LUA
	// SCRIPT (internal/luaproviders/scripts/anilibria/main.lua) — the
	// fourth Go→Lua provider migration. luaOnly pins the roster slot;
	// the script serves the id (the API's per-requester quality
	// tiering and the 1080p ceiling live in the script header).
	{"anilibria", true, nil},
	// animevost (PR46 → PR119): the api.animevost.org JSON API (the
	// anicli-py animevost.py port) migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animevost/main.lua) — the fifth
	// Go→Lua provider migration. luaOnly pins the roster slot. Live
	// 2026-10-05: the cert expired 2026-09-20 was renewed and the
	// Chrome_150 uTLS tarpit that forced the compiled provider's
	// scoped plain-Go transport (PR46) is gone — the shared netclient
	// answers through the configured proxy; the Go transport escape
	// hatch died with the file.
	{"animevost", true, nil},
	// anilib (PR122): the api.cdnlibs.org JSON API migrated to the
	// BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anilib/main.lua) — the sixth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script carries the browser header set, the sequential PR53
	// contentless preflight and the PR44 release-dub-keys tier-1.
	{"anilib", true, nil},
	// animego (PR48 → PR124): the animego.me RU catalog (the live
	// continuation of the dead animego.org/.one original; same
	// /anime/{slug}-{id} scheme, kodik+aniboom player ecosystem)
	// migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animego/main.lua) — the seventh
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script carries the XHR header set and the PR44 release-dub-keys
	// tier-1 (the anilib.go helper's last Go consumer dies here — the
	// distribution lives in the script's episodes()).
	{"animego", true, nil},
	// gogoanime (PR128): the anitaku.io WordPress platform (the
	// gogoanime rebrand, Kohi-den extensions-source issue #410; the
	// anicli-py gogoanime.py port re-verified live 2026-09-18) migrated
	// to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/gogoanime/main.lua) — the twelfth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script carries the ts_ac_do_search admin-ajax search, the
	// newest-first .eplister listing reversed to ascending, the eager
	// per-episode mirror hydration (base64 select.mirror options, the
	// Lua contract has no DubsHydrator — the yummy/animedia/anikoto
	// precedent) and the fresh-sandbox resolve through the shared
	// extractor factory (anicli.extract).
	{"gogoanime", true, nil},
	// kickassanime (PR58 → PR129): the kaa.lt catalog — a fuzzy JSON
	// search, a paginated per-show episode API and per-episode server
	// lists whose media ids resolve onto the krussdomi HLS edge
	// (live-verified 2026-09-18, re-verified 2026-10-06: every leg
	// answers anonymously, no gate cookie on the wire). Migrated to the
	// BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/kickassanime/main.lua) — the
	// thirteenth Go→Lua provider migration. luaOnly pins the roster
	// slot; the script serves the id: eager per-episode dub hydration
	// in one bounded-parallel batch (the DubsHydrator delta — the Lua
	// contract has no such capability), the fan-out bound pinned at the
	// config default (get_batch cannot carry headers; the live probe
	// proves Accept optional), and the fresh-sandbox resolve
	// re-deriving the server list.
	{"kickassanime", true, nil},
	// anizone (PR59 → PR130): the anizone.to sub-only stream source —
	// the first provider with no frozen Python original, written from
	// the Anivexa-API AniZone recipe (providers/anizone.js) re-verified
	// live 2026-09-18. Livewire HTML payloads, /livewire/update episode
	// pagination and vidstackPlayer HLS on the watch page; no
	// credentials. Migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anizone/main.lua) — the fourteenth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script carries the Livewire continuation walk (the csrf/snapshot
	// round-trip re-derived per invocation — the fresh-sandbox
	// contract), the JSON-argument decoder and the {n,u} raw_id watch
	// state (the animevost/anilib precedent).
	{"anizone", true, nil},
	// sameband (PR131): the SameBand studio DLE catalog migrated to
	// the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/sameband/main.lua) — the fifteenth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the DLE search-form POST, the iframe-chained
	// Playerjs playlist chain and the direct no-network quality-map
	// resolve (the raw file field rides episode RawID). No
	// credentials; formContentType's last sameband consumer died here
	// (the helper itself followed when kodik migrated in PR140).
	{"sameband", true, nil},
	// kodik → PR140: the tokenled kodik-api.com search API plus the
	// scraped kodik.info player pages (the anicli-py kodik.py port)
	// migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/kodik/main.lua) — the TWENTY-THIRD
	// Go→Lua provider migration. luaOnly pins the roster slot; the API
	// token rides anicli.provider_setting("token") (the PR140 per-
	// provider config read — the reason that SDK leg exists) and an
	// empty or missing token fails loud on search exactly like the Go
	// constructor did. The parity smoke's credential-gated kodik SKIP
	// stays untouched. LIVE VERIFICATION IS IMPOSSIBLE for this
	// provider — no owner token exists; the fixture suite carries the
	// whole proof (the script header documents it loudly).
	{"kodik", true, nil},
	// anidub (PR132): the online.anidub.com RU DLE catalog migrated to
	// the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anidub/main.lua) — the sixteenth
	// Go→Lua provider migration. anidub is not a frozen anicli-py
	// port: the provider was characterized live (the anizone
	// precedent). luaOnly pins the roster slot; the script serves the
	// DLE search-form GET (the pyQuote %20 encoding re-derived in the
	// script — the SDK's query_escape is form-style +), the
	// «Запасной плеер» sibnet span walk (the ПЛЕЕР #1 playlist span
	// skipped by the «Серия» gate) and the sibnet resolve through the
	// shared extractor factory (anicli.extract). No credentials; the
	// live smoke keeps the documented sibnet 403 drift at resolve
	// (site-side; the shared extractor, not the port).
	{"anidub", true, nil},
	// animedia (PR56 → PR116): the amd.online DLE site migrated to
	// the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animedia/main.lua) — the second
	// Go→Lua provider migration. luaOnly pins the roster slot.
	{"animedia", true, nil},
	// shiza (PR57 → PR125): the shizaproject.com GraphQL catalog
	// migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/shiza/main.lua) — the eighth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the id: anonymous catalog search, kodik/sibnet
	// embeds resolved through the shared extractor factory
	// (anicli.extract). No credentials; its torrent entries are dead
	// (0 seeders, see the script header) so no torrent sibling is
	// registered.
	{"shiza", true, nil},
	// yummy (PR68 → PR123): the YummyAnime REST API (api.yani.tv behind
	// site.yummyani.me) — the first provider ported from the vypivshiy
	// anicli-api reference library (source/yummy_anime.py), verified
	// live 2026-09-19, migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/yummy/main.lua) — the fourth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the id.
	{"yummy", true, nil},
	// hdrezka → PR141: the RU rezka catalog's anime section — port of
	// the frozen anicli-api hdrezka source re-verified live 2026-09-19
	// — migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/hdrezka/main.lua) — the
	// TWENTY-FOURTH Go→Lua provider migration, and the roster's
	// HYBRID: the script scrapes, while the Anubis proof-of-work gate
	// every family mirror fronts solves in pure Go through the
	// anicli.solve_anubis SDK binding (internal/anubis — the solver
	// extracted from the compiled provider, NOT ported to Lua). PR72
	// route matrix: the family geo-fences per domain (hdrezka-home.tv
	// withholds stream links from datacenter exits — its session JWT
	// attests geo:"de" — while rezka-ua.tv serves them), so the
	// script's default pins the serving mirror and
	// [providers.hdrezka] base_url re-points every leg through the
	// PR140 provider_setting seam without a rebuild. No credentials;
	// translators are the dubs (one-voice included), hdrezka's own CDN
	// resolves to HLS/mp4. From ISP-blocked networks network.proxy_url
	// routes it (foreign hosting, SNI-blocked direct route — verified
	// killed mid-TLS on a RU-intercepted network).
	{"hdrezka", true, nil},
	// anistar (PR77 → PR138): the anistar.org DLE catalog with its
	// self-hosted an-media.org player stack — the roster's first
	// Windows-1251 site (search form POST and page bodies both ride
	// cp1251; the SDK's anicli.iconv decodes the wire, the script
	// carries the reverse map for the story field). Written from the
	// live site, not ported; no credentials. The p2p player page
	// exposes direct per-quality HLS/MP4 links behind a media_id; the
	// an-media edge requires the site Referer on playback. Migrated to
	// the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anistar/main.lua) — the
	// twenty-first Go→Lua provider migration; it died with anistar.go
	// (its last consumer), so anistarStreamHeaders/Type,
	// anistar1251Encode/Decode and anistarAbsURL are script-local now.
	// luaOnly pins the roster slot; the script serves the DLE search
	// POST, both player generations (the p2p playlst array and the
	// legacy #PlayList spans) and the per-quality resolve.
	{"anistar", true, nil},
	// anifilm (PR91 → PR135): the anifilm.pro RU stream+torrent catalog
	// — a custom Yii/Vue engine, NOT DLE; written from the live site
	// (2026-09-23; re-verified live through the configured proxy
	// 2026-10-06). Migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anifilm/main.lua) — the nineteenth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the GET-form search, the Vue player-component props
	// → api:online playlists → api:video pages wrapping kodik embeds
	// (shared extractor). No credentials. The per-release .torrent
	// downloads are a TorrentBase extension candidate, deliberately out
	// of scope here.
	{"anifilm", true, nil},
	// animemobi (PR92 → PR137): the animemobi.com RU mobile catalog
	// (DLE, UTF-8, anonymous) migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animemobi/main.lua) — the
	// TWENTIETH Go→Lua provider migration. luaOnly pins the roster
	// slot; the script serves the DLE full-search POST (both skins:
	// the smartphone div.shortstory rows and the desktop div.base/
	// div.bheading rows), the «Озвучка:»-credited single dub and the
	// per-episode a.onlinevideo kodik-family embeds (kodikplayer.com
	// and aniqit.com, both covered by the shared kodik extractor). The
	// release pages additionally carry per-release .torrent downloads
	// (documented out of the stream contract — the download pipeline
	// note lives in the PR92 Go source's git history). No frozen
	// Python original; written from the live site.
	{"animemobi", true, nil},
	// anitokyo (PR100 → PR116): the anitokyo.tv RU DLE catalog with
	// its RalodePlayer module migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anitokyo/main.lua) — the first
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the id.
	{"anitokyo", true, nil},
	// animiku (PR101 → PR134): the beta.animiku.tokyo RU catalog (DLE
	// under a custom template, UTF-8, anonymous; live-verified
	// 2026-09-25) migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animiku/main.lua) — the eighteenth
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the DLE search GET, the mrdeath/aaparser player
	// bridge (POST news_id+action=load_player: translator row = dubs,
	// episode grid = per-(episode, dub) kodikplayer.com embeds
	// re-derived at resolve time from the {n,id} raw_id state) and the
	// shared-extractor resolve (anicli.extract). The advertised 4K/FHD
	// tiers are runtime JS resolvers (anilibria.top API by title) with
	// no deterministic embed URLs — out of the stream contract. No
	// frozen Python original; written from the live site.
	{"animiku", true, nil},
	// anikado (PR102 → PR133): the anikado.net RU DLE catalog (DLE,
	// UTF-8, anonymous) migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anikado/main.lua) — the
	// seventeenth Go→Lua provider migration. luaOnly pins the roster
	// slot; the script serves the DLE search-form POST, the
	// server-side episode anchor listing fanned out bounded-parallel
	// over the per-episode b-translator__item pages (the kickassanime
	// pattern), the kodik.info → kodikplayer.com mirror
	// normalization and the service-dub movie tab — the movie embed
	// rides the {n, e} raw_id state, keeping the compiled provider's
	// zero-fetch movie resolve. The title page's vkg/tomion fallback
	// tabs stay documented walls (client-side hydrated / frame-gated).
	{"anikado", true, nil},
	// animevib (PR103 → PR116): the www.animevib.ru RU DLE catalog
	// migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animevib/main.lua) — the third
	// Go→Lua provider migration. luaOnly pins the roster slot.
	{"animevib", true, nil},
	// animeheaven (PR105 → PR126): the animeheaven.me EN sub-only
	// catalog migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/animeheaven/main.lua) — direct-MP4
	// sources (no extract leg), the roster's first latin stream
	// provider since anizone. luaOnly pins the roster slot.
	{"animeheaven", true, nil},
	// anikoto (PR104 → PR127): the anikototv.to EN catalog — a
	// HiAnime/Zoro-style clone (the anikoto.net platform family
	// documented by the AniVault Scraper and the PyPI anikoto
	// downloader, live-verified 2026-09-25). Written from the live site;
	// no credentials: /filter?keyword= HTML search in, the
	// {"status":N,"result":…} AJAX envelope out (episode list + SUB/DUB
	// server groups + per-server stream resolver), and the megaplay
	// embed chain statically unpacked — XOR string table, AES-256-CBC
	// enc decrypt, HMAC-signed CDN URL — without executing any
	// JavaScript. The controller's /api/search lead is a decoy: the site
	// answers every parameter with the error envelope. Migrated to the
	// BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anikoto/main.lua) — the eleventh
	// Go→Lua provider migration. luaOnly pins the roster slot; the
	// script serves the id (the crypto legs ride the anicli.crypto SDK
	// primitives, the XOR unpack and the AJAX shapes the script
	// header).
	{"anikoto", true, nil},
	// anipub (PR107 → PR139): the anipub.xyz EN catalog — an open
	// Express+Mongo API (github.com/AnimePub/AniPub, the site's own
	// source; the api. subdomain is static GitHub Pages, the real API
	// rides the apex host). Written from the live API + backend source
	// (2026-09-25); anonymous on every leg (the validkey middleware
	// next()s on a missing key). /api/searchAll name search in, the
	// /v1/api/details ep array out; each link is the site's own
	// /video/<n>/<sub|dub> player page wrapping a megaplay.buzz stream
	// whose same-origin getSourcesNew enc payload decrypts (static
	// AES-256-CBC params from megaplay's newclient.min.js) to the
	// master.m3u8 — no extractor factory hop. Sub and Dub emit per
	// episode (the site's own changeStreamType toggle); the megaplay
	// CDN 403s playback without the stream-origin Referer, so it rides
	// on the source. Migrated to the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anipub/main.lua) — the
	// twenty-second Go→Lua provider migration. luaOnly pins the roster
	// slot; the script serves the id: the AES decrypt rides the
	// anicli.crypto SDK primitive (no cipher reimplemented in Lua),
	// the fresh-sandbox raw_id state carries the catalog-flavor player
	// URL (the sameband/anidub single-value precedent) and the
	// megaplay embed origin is never a literal — the script follows
	// the video page's iframe (the anikoto precedent).
	{"anipub", true, nil},
	// anilibria-torrent (PR37): the aniliberty.top API's per-release
	// torrents on the same TorrentBase plumbing. Shares the release
	// search endpoint with the anilibria stream provider and expands
	// each hit into its torrent list; no credentials, engine injected
	// by NewRegistry when [torrent] is enabled.
	{"anilibria-torrent", false, func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnilibriaTorrent(AniLibriaAPIBase, http, nil)
	}},
	// animetosho (PR38): the animetosho.org newznab search on the same
	// TorrentBase plumbing — hex-infohash magnets, .torrent enclosure
	// fallback; no credentials, engine injected by NewRegistry when
	// [torrent] is enabled.
	{"animetosho", false, func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newAnimeTosho(AnimeToshoFeedBase, http, nil)
	}},
	// tokyotosho (PR38): the tokyo-tosho.net search RSS on the same
	// TorrentBase plumbing — direct .torrent <link> URLs; no
	// credentials, engine injected by NewRegistry when [torrent] is
	// enabled.
	{"tokyotosho", false, func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newTokyoTosho(TokyoToshoBase, http, nil)
	}},
	// rutor (PR87): the rutor.info public tracker's HTML search on the
	// same TorrentBase plumbing — the fifth torrent provider and the
	// first RU-indexed one (the Jackett rutor.yml recipe, re-verified
	// live 2026-09-23). Fully anonymous (search and .torrent
	// downloads); the engine is injected by NewRegistry when
	// [torrent] is enabled.
	{"rutor", false, func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newRutor(RutorBase, http, nil)
	}},
	// anirena (PR88 → PR143): the anirena.com search RSS migrated to
	// the BUNDLED LUA SCRIPT
	// (internal/luaproviders/scripts/anirena/main.lua) — the
	// twenty-sixth Go→Lua provider migration, the torrent family's
	// second Lua slot (the PR142 rutor migration was the first).
	// luaOnly pins the roster slot; the script serves the
	// search surface (the anonymous /rss?q= route — the documented
	// JSON API gates torrent search behind personal bearer keys,
	// live-verified 2026-09-23; the Anime scope and the 30-item cap
	// are client-side, the feed carries no seed fields). The torrent
	// plumbing stays GO: the slot rides the luaTorrent adapter
	// (luaTorrentFactories below) — the PR66 .torrent preflight, the
	// engine ingestion and the episodes/stream resolve behave
	// byte-identically to the compiled TorrentBase provider's.
	{"anirena", true, nil},
	// subsplease (PR89): the subsplease.org JSON API on the same
	// TorrentBase plumbing — the EN seasonal group's f=search catalog
	// (the RSS feeds are latest-only and queryless, the site search
	// endpoint is the API) with tracker-rich magnet links and the
	// show-page sid hop for batch back-catalog; no credentials, engine
	// injected by NewRegistry when [torrent] is enabled.
	{"subsplease", false, func(http *netclient.Client, _ config.Settings, _ *cfbrowser.Manager) contracts.Provider {
		return newSubsPlease(SubsPleaseBase, http, nil)
	}},
}

// luaTorrentFactories lists the TORRENT roster slots served by bundled
// Lua scripts (PR143): when a script serves one of these ids, the
// factory wraps it in the luaTorrent adapter — the torrent capability
// (IsTorrent, the engine injection, the PR66 .torrent preflight and
// the episodes/stream resolve) stays Go around the script's search
// surface. The wrap applies to the SLOT: a user script shadowing the
// id rides the same adapter, because the slot itself is torrent-shaped
// (the compiled factories behind these ids carried the identical
// plumbing).
var luaTorrentFactories = map[string]bool{
	"anirena": true,
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
	out, _, err := allWithCFDisabled(cfg, extra, cf, slog.Default())
	return out, err
}

// luaScriptSources assembles the Lua provider script sources in
// LoadSources precedence order (PR116, order fixed in PR120): the
// user config dir first (highest precedence), then [providers.lua].dir,
// then the bundled embeds — LoadSources keeps the FIRST occurrence of
// an id, so this order is what makes a user script override a bundled
// one without a rebuild (the documented shadowing contract; the
// PR116 bundled-first assembly silently inverted it — exposed and
// fixed when anilibria joined the bundled roster in PR120). Missing
// dirs scan to nothing.
func luaScriptSources(cfg config.Settings) []lua.Source {
	out := make([]lua.Source, 0)
	if dir, ok := lua.ProvidersDir(); ok {
		out = append(out, lua.ScanDir(dir)...)
	}
	if cfg.Providers.Lua.Dir != "" {
		out = append(out, lua.ScanDir(cfg.Providers.Lua.Dir)...)
	}
	out = append(out, luaproviders.Sources()...)
	return out
}

// providerSettingsFor flattens config.Settings onto the per-provider
// settings seam the sandbox exposes through anicli.provider_setting
// (PR140): every per-provider string setting config carries today,
// keyed under its providers.<id>.<key> name. A provider without a
// settings section yields nil — its scripts read Lua nil for every
// key. The values are secret-bearing (kodik's API token): they ride
// the engine config only and are never logged.
func providerSettingsFor(cfg config.Settings) lua.SettingsFor {
	return func(id string) map[string]string {
		switch id {
		case "kodik":
			return map[string]string{"token": cfg.Providers.Kodik.Token}
		case "hdrezka":
			return map[string]string{"base_url": cfg.Providers.HDRezka.BaseURL}
		default:
			return nil
		}
	}
}

// luaProviders builds the Lua provider set for cfg: the assembled
// script sources loaded through the sandboxed engine, each provider
// wired to its OWN netclient (the compiled providers' transport
// isolation) and its OWN settings map (PR140). The [providers].exclude
// list applies to Lua ids the same way it applies to the Go factories.
// Returns the providers by id plus their assembly order (tail-append
// order for non-shadowing ids). A broken script is a skip inside
// LoadSources — never an error; a client build failure IS an error
// (the Go factories' fail-loud transport contract).
func luaProviders(cfg config.Settings, extra []netclient.Option, excluded map[string]bool, log *slog.Logger) (map[string]contracts.Provider, []string, error) {
	if !cfg.Providers.Lua.Enabled {
		return nil, nil, nil
	}

	// First occurrence of an id wins the precedence; only winners
	// get a transport.
	seen := map[string]bool{}
	ordered := make([]lua.Source, 0)
	for _, src := range luaScriptSources(cfg) {
		if seen[src.ID] || excluded[src.ID] {
			continue
		}
		seen[src.ID] = true
		ordered = append(ordered, src)
	}

	clients := make(map[string]*netclient.Client, len(ordered))
	for _, src := range ordered {
		opts := append([]netclient.Option{netclient.WithProvider(src.ID)}, extra...)
		client, err := netclient.New(cfg.Network, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("build lua provider %s client: %w", src.ID, err)
		}
		clients[src.ID] = client
	}

	provs, _ := lua.LoadSources(lua.DefaultConfig(), log, ordered, func(id string) *netclient.Client {
		return clients[id]
	}, providerSettingsFor(cfg))
	byID := make(map[string]contracts.Provider, len(provs))
	order := make([]string, 0, len(provs))
	for _, p := range provs {
		byID[p.ID()] = p
		order = append(order, p.ID())
	}
	return byID, order, nil
}

// allWithCFDisabled is allWithCF that also returns the
// unconfigured-provider set for registry bookkeeping. log receives
// the assembly diagnostics (exclusions, Lua shadows, script skips) —
// NewRegistry threads the configured provider sink, the direct
// constructors keep slog.Default (the startup-lines precedent).
func allWithCFDisabled(cfg config.Settings, extra []netclient.Option, cf *cfbrowser.Manager, log *slog.Logger) ([]contracts.Provider, []DisabledProvider, error) {
	excluded := make(map[string]bool, len(cfg.Providers.Exclude))
	for _, id := range cfg.Providers.Exclude {
		excluded[id] = true
	}
	disabledMap := unconfiguredIDs(cfg)

	// PR116: the Lua provider set shadows the compiled Go factories
	// by id — the script takes the Go slot in roster order, a
	// non-shadowing id appends at the roster tail.
	luaByID, luaOrder, err := luaProviders(cfg, extra, excluded, log)
	if err != nil {
		return nil, nil, err
	}
	luaPending := make(map[string]bool, len(luaOrder))
	for _, id := range luaOrder {
		luaPending[id] = true
	}

	out := make([]contracts.Provider, 0, len(allFactories)+len(luaOrder))
	for _, factory := range allFactories {
		if excluded[factory.id] {
			log.Info("provider excluded: " + factory.id)
			continue
		}
		if lp, shadow := luaByID[factory.id]; shadow {
			delete(luaPending, factory.id)
			if !factory.luaOnly {
				log.Info("provider " + factory.id + " shadowed by its lua script")
			}
			if luaTorrentFactories[factory.id] {
				lp = newLuaTorrent(lp)
			}
			out = append(out, lp)
			continue
		}
		if factory.luaOnly {
			// The Lua-only slot with no loaded script: the loader
			// logged the skip (or [providers.lua] is off) — the slot
			// drops, never faked with a Go fallback.
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
	// Non-shadowing Lua providers append at the roster tail, in
	// assembly order.
	for _, id := range luaOrder {
		if luaPending[id] {
			out = append(out, luaByID[id])
		}
	}
	// A Lua script serving a normally-unconfigured id (a user kodik
	// with its own token handling) un-disables that id: the notice
	// must not fire for a provider that IS active.
	disabled := make([]DisabledProvider, 0, len(disabledMap))
	for _, d := range disabledMap {
		if _, active := luaByID[d.ID]; active {
			continue
		}
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
	// PR62 #4: every provider carrying the Base logger seam gets the
	// configured sink (SetLogger probe, the SetEngine pattern); nil
	// degrades to discard inside the provider — never stderr.
	providerLog := o.providerLogger
	if providerLog == nil {
		providerLog = discardLogger
	}
	// The STARTUP assembly lines (exclusions, Lua shadows, script
	// skips) are pre-alt-screen output: they keep slog.Default when
	// no provider sink was configured — silent exclusion must never
	// regress (TestNewRegistryLogsExcludedProviders).
	assemblyLog := o.providerLogger
	if assemblyLog == nil {
		assemblyLog = slog.Default()
	}
	bare, disabled, err := allWithCFDisabled(cfg, cfOpts, cfMgr, assemblyLog)
	if err != nil {
		return nil, err
	}

	reg := NewEmptyRegistry()
	if cfg.Torrent.Enabled {
		if err := reg.wireTorrentEngine(cfg, bare, o.torrentLogger); err != nil {
			return nil, err
		}
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
	// PR116: the Lua providers are part of `bare` already (assembled
	// in allWithCFDisabled, shadowing the Go factories by id) — the
	// SetLogger loop above routes their engine diagnostics to the
	// configured sink through the promoted lua.Provider.SetLogger.
	reg.disabled = disabled
	reg.cfClose = cfClose
	reg.cfMgr = cfMgr
	return reg, nil
}
