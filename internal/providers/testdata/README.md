# Provider test fixtures

Every fixture is hand-modeled from the parsing logic of the frozen Python
original (`/home/whoami/projects/anicli-py`, branch `snapshot/broken-2026-09-12`):
each file reproduces the minimal real-world response shape the referenced
Python code consumes. No fixture is a byte-for-byte network capture; fields
irrelevant to the parsing logic are trimmed. If the live API drifts, the
provenance reference below points at the Python parsing code that defines
the expected input.

Fixtures marked **[LIVE-VERIFIED …]** diverge from that rule on
purpose: they are derived from real network captures of the marked day
(2026-09-13 for the wave-2 revivals, 2026-09-18 for the gogoanime →
Anitaku rebrand; page chrome trimmed, values verbatim) because the live
site drifted away from the frozen Python shapes — see the per-fixture
notes.

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
| `gogoanime_search.json` | **[LIVE-VERIFIED 2026-09-18]** derived from `POST https://anitaku.io/wp-admin/admin-ajax.php` (`action=ts_ac_do_search&ts_ac_query=one piece`) — the Anitaku rebrand (gogoanime platform, Kohi-den extensions-source issue #410) dropped the WordPress `/?s=` search (it 301s to `/browse/`); the GET form of this endpoint silently ignores the query and answers with recent posts. The response is `{"series":[{"all":[…]}]}`; per item reads `post_title`, `post_image`, `post_link`. First three of eight live entries verbatim, array trimmed. |
| `gogoanime_series.html` | **[LIVE-VERIFIED 2026-09-18]** derived from `GET https://anitaku.io/series/one-piece/` — the `.bixbox.epcheck` block with the `.eplister` grid; li entries carry `a[href]` (absolute episode URL), `.epl-num`, `.epl-title`, newest-first (live: newest ~84 of One Piece; the site exposes no older-episode ajax). First three entries verbatim, list trimmed. |
| `anidub_search.html` | **[LIVE-VERIFIED 2026-09-13]** derived from `GET https://online.anidub.com/?do=search&subaction=search&story=naruto` (plain curl, desktop UA) — anidub's DLE POST search still renders results server-side (13 cards live; junk query → zero cards, HTTP 200). Cards reuse the catalog `.th-item` template (`.sect-content.sect-items` scope, `a.th-in[href]`, `.th-title`, `.th-img img[src]`); three cards verbatim, page chrome trimmed. |
| `anidub_anime.html` | **[LIVE-VERIFIED 2026-09-13]** the `.fplayer` block verbatim from `GET https://online.anidub.com/12254-blich-…-zakljuchitelnaja-chast.html` — tab 1 "Основной плеер" is ONE full-title playlist span (`ПЛЕЕР #1` → external ladonyvesna host, skipped client-side); tab 2 "Запасной плеер" carries per-episode `Серия N` spans with `video.sibnet.ru/shell.php` embeds (live: 8 of 13 aired). |
| `gogoanime_episode.html` | **[LIVE-VERIFIED 2026-09-18]** derived from `GET https://anitaku.io/one-piece-episode-1178-english-subbed/` — the `select.mirror` server list where each option stores one server as **base64-encoded iframe HTML** in its value (the theme's `loadMi` does `atob(value)` into `#pembed`); the leading `Select Video Server` placeholder has an empty value and is skipped. Option 1 is the real One Piece 1178 capture (blogger.com embed); option 2 carries the real mirror value captured the same day from the Dandadan S2 ep12 page (megacloud.bloggy.click), index renumbered. |
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
