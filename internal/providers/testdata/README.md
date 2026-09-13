# Provider test fixtures

Every fixture is hand-modeled from the parsing logic of the frozen Python
original (`/home/whoami/projects/anicli-py`, branch `snapshot/broken-2026-09-12`):
each file reproduces the minimal real-world response shape the referenced
Python code consumes. No fixture is a byte-for-byte network capture; fields
irrelevant to the parsing logic are trimmed. If the live API drifts, the
provenance reference below points at the Python parsing code that defines
the expected input.

Fixtures marked **[LIVE-VERIFIED 2026-09-13]** diverge from that rule on
purpose: they are derived from real network captures of the same day (page
chrome trimmed, values verbatim) because the live site drifted away from
the frozen Python shapes — see the per-fixture notes.

| Fixture | Consumed by (provenance) |
| --- | --- |
| `anilibria_search.json` | `anicli/providers/anilibria.py:21-34` — search response is a JSON array; per item reads `name.main` (default `Unknown`), `alias`, `id`. |
| `anilibria_release.json` | `anicli/providers/anilibria.py:36-58` — release object; reads `episodes[]` with truthy-checked `hls_1080`/`hls_720`/`hls_480`, `ordinal`, `id`. Empty `hls_480` models the falsy field skip. |
| `animevost_search.json` | `anicli/providers/animevost.py:20-37` — search response object with `data[]`; per item reads `title`, `id`, `urlImagePreview`. |
| `animevost_playlist.json` | `anicli/providers/animevost.py:39-64` — playlist is a JSON array (or an `{"error": ...}` object, modeled inline in tests); per item reads `name`, `hd`, `std` (truthy-checked). |
| `anilib_search.json` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET api.cdnlibs.org/api/anime?q=naruto&limit=20&site_id=5` — the API rejects the legacy `fields[]`/`site_id[]` params with HTTP 422; the flat `site_id=5` form answers the same `data[]` shape the Python parser consumes (`rus_name`/`name`/`eng_name` None-coalesce chain, `slug_url`, `cover.default`). First three entries are real naruto results; the fourth is a modeled null-chain entry keeping the fallback coverage. |
| `anilib_episodes.json` | `anicli/providers/anilib.py:86-114` — episodes response object with `data[]`; per item reads `id`, `name` (or `Episode`), `number` (may be `null` → Python `str(None)` = `"None"`). Endpoint shape re-verified live 2026-09-13 (`/episodes?anime_id=11`, 220 entries). |
| `anilib_episode_players.json` | `anicli/providers/anilib.py:116-139` — episode object with `data.players[]`; per player reads `team.name`, `player`, `src` (Kodik) and `video.quality[]` with `href`/`quality` (AnimeLib internal). Endpoint shape re-verified live 2026-09-13 (`/episodes/455`, 5 Kodik players). |
| `animego_search.html` | `anicli/providers/animego.py:27-50` — search page; selectolax selectors `.row > .col-ul-2`, `.text-truncate a[title]`, `.lazy[data-original]`. |
| `animego_anime.html` | `anicli/providers/animego.py:52-59` — anime page; selector `.br-2 .my-list-anime` with `id="my-list-<id>"`. |
| `animego_player_series.json` | `anicli/providers/animego.py:61-81` — player API JSON with `content` HTML; selector `#video-carousel .mb-0` with `data-episode`, `data-id`, `data-episode-title`. |
| `animego_player_film.json` | `anicli/providers/animego.py:82-105` — player API JSON with `content` HTML lacking `#video-carousel`; film path parses `#video-dubbing .mb-1` (`data-dubbing`) and `#video-players > span` children WITHOUT `.mb-1` so the Python fallback selector branch (`#video-players > span`, animego.py:115) is exercised (`data-player`, `data-provide-dubbing`). |
| `animego_series.json` | `anicli/providers/animego.py:93-105` — `/anime/series` API JSON with `content` HTML carrying the same dubbing/players markup. |
| `sovetromantica_search.html` | `anicli/providers/sovetromantica.py:29-53` — search page; selectors `.anime--block`, `.anime--block__name`, first `a` descendant. |
| `sovetromantica_anime.html` | `anicli/providers/sovetromantica.py:55-82` — anime page; selectors `.episodes-slick .episode` (fallback `.episodes-list .episode`), first `a`, first `span` text `Эпизод N`. |
| `sovetromantica_episode.html` | `anicli/providers/sovetromantica.py:84-98` — episode page embedding `file: "<url>.m3u8"` in inline JS (regex `file\s*:\s*"([^"]+\.m3u8[^"]*)"`). |
| `gogoanime_search.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://gogoanime.by/?s=one+piece` — the site moved to WordPress/dramastream (gogoanime3.co is Cloudflare-blocked). Search results are the `a.tip` cards of the FIRST `.listupd` (title attr, absolute `/series/` href, `.limit img` poster); the second `.listupd` (external-results placeholder) and the sidebar `.leftseries` card must not leak in. Cards verbatim from the capture. |
| `gogoanime_anime.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://gogoanime.by/series/naruto-shippuuden/` — the dramastream series page renders ALL `.episodes-container .episode-item` entries server-side (live: 499), newest-first; the five items are verbatim. |
| `gogoanime_episode.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://gogoanime.by/naruto-shippuuden-episode-500-english-subbed/` — `#w-servers li.player-type-link[data-src]` server list (name = li text, value = same-origin `/player/` proxy URL). The "Mega" li is verbatim from that page; the "HD" li is the real entry captured from the One Piece 1178 page the same day; the data-src-less li models the skip. |
| `gogoanime_player.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://gogoanime.by/player/?source=embed&url=…` with the episode-page Referer (without it the proxy redirects to the site root) — wraps the real embed in `iframe.player-iframe` (megavid.buzz). |
| `gogoanime_megavid.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://megavid.buzz/mal/1735/500/sub` — megavid-family embed; the `#player-payload` JSON element (verbatim) points the player bootstrap at a `sourceUrl` JSON endpoint (`Accept: application/json`). |
| `gogoanime_megavid_source.json` | **[LIVE-VERIFIED 2026-09-13]** real capture of `GET https://megavid.buzz/mal/1735/500/sub/source` — `{"status":"ok","source":"https://megavid.buzz/vid/…","type":"hls"}`. |
| `gogoanime_megaplay.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://megaplay.su/embed.php?sid=…` — megaplay-family embed with an inline jwplayer `file: "<hls url>"` setup (verbatim). |
| `animepahe_search.json` | `anicli/providers/animepahe.py:26-46` — `/api?m=search` response object with `data[]`; per item reads `title`, `session`, `poster`. |
| `animepahe_episodes_p1.json` | `anicli/providers/animepahe.py:48-91` — `/api?m=release` first page carrying `total`/`per_page`/`last_page`=2 and `data[]` (`episode`, `session`). |
| `animepahe_episodes_p2.json` | same endpoint, page 2; a fractional `episode: 2.5` pins the wire-form number rendering (`str(2.5)` → `"2.5"`). |
| `dreamcast_anime.html` | `anicli/providers/dreamcast.py:50-88` — anime page with an inline `new Playerjs("<encoded>")` script and a `/js/playerjs/...` script src. **Oracle-generated**: the encoded blob and the key inside the playerjs decode through the frozen Python `_decode_playlist`, verified 2026-09-12. |
| `dreamcast_player.js` | `anicli/providers/dreamcast.py:101-258` — unpacked playerjs carrying the `u: '#0<key>=\\'` crypt marker. Same oracle run as above. |
| `sameband_catalog.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://sameband.studio/anime` — the DLE POST search is dead server-side (200 with an empty `fastsearch_results` shell; AJAX-only now), so Search fetches the catalog (94 entries, no pagination) and filters client-side. Cards reuse the DLE results template verbatim (`.col-auto`, `.image[href]`, `.poster[title]`, `img.swiper-lazy`; three real cards). |
| `sameband_anime.html` | `anicli/providers/sameband.py:48-59` — anime page; selector `.player > .player-content > iframe[src]`. Chain re-verified live 2026-09-13 (anime 122 → `/v/play/…` → Playerjs `file:"/v/list/…"` → playlist JSON). |
| `sameband_playlist.json` | `anicli/providers/sameband.py:67-81` — player JSON playlist array; per item reads `title` (optional) and `file` (`[NNNp]url` comma-joined). Live playlist shape confirmed 2026-09-13 (`Серия NN` titles carry poster/duration HTML, kept raw like the Python original). |
| `kodik_search.json` | `anicli/providers/kodik.py:42-95` — `/search` response with `results[]`; per item reads `title`/`title_orig` (fallback chain), `link` (protocol-relative, may be null → skip), `year`, `type`, `material_data.poster_url`/`anime_poster_url`. |
| `kodik_serial.html` | `anicli/providers/kodik.py:97-170, 185-247` — serial player page: `.serial-series-box select` options (episode numbers) + `.serial-translations-box select` options (`data-media-id`/`data-media-hash`; an option missing the id models the skip). |
| `kodik_movie.html` | `anicli/providers/kodik.py:153-168` — movie page: `.movie-translations-box select` only → single film episode (`Фильм`). |
| `kodik_default.html` | `anicli/providers/kodik.py:219-247` — no translations box; inline script with `.media_id`/`.media_hash` globals → the "Default" translation fallback. |
