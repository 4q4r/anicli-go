# Provider test fixtures

Every fixture is hand-modeled from the parsing logic of the frozen Python
original (`/home/whoami/projects/anicli-py`, branch `snapshot/broken-2026-09-12`):
each file reproduces the minimal real-world response shape the referenced
Python code consumes. No fixture is a byte-for-byte network capture; fields
irrelevant to the parsing logic are trimmed. If the live API drifts, the
provenance reference below points at the Python parsing code that defines
the expected input.

| Fixture | Consumed by (Python provenance) |
| --- | --- |
| `anilibria_search.json` | `anicli/providers/anilibria.py:21-34` — search response is a JSON array; per item reads `name.main` (default `Unknown`), `alias`, `id`. |
| `anilibria_release.json` | `anicli/providers/anilibria.py:36-58` — release object; reads `episodes[]` with truthy-checked `hls_1080`/`hls_720`/`hls_480`, `ordinal`, `id`. Empty `hls_480` models the falsy field skip. |
| `animevost_search.json` | `anicli/providers/animevost.py:20-37` — search response object with `data[]`; per item reads `title`, `id`, `urlImagePreview`. |
| `animevost_playlist.json` | `anicli/providers/animevost.py:39-64` — playlist is a JSON array (or an `{"error": ...}` object, modeled inline in tests); per item reads `name`, `hd`, `std` (truthy-checked). |
| `anilib_search.json` | `anicli/providers/anilib.py:43-84` — search response object with `data[]`; per item reads `rus_name`/`name`/`eng_name` (None-coalesce chain), `slug_url`, `cover.default`. |
| `anilib_episodes.json` | `anicli/providers/anilib.py:86-114` — episodes response object with `data[]`; per item reads `id`, `name` (or `Episode`), `number` (may be `null` → Python `str(None)` = `"None"`). |
| `anilib_episode_players.json` | `anicli/providers/anilib.py:116-139` — episode object with `data.players[]`; per player reads `team.name`, `player`, `src` (Kodik) and `video.quality[]` with `href`/`quality` (AnimeLib internal). |
| `animego_search.html` | `anicli/providers/animego.py:27-50` — search page; selectolax selectors `.row > .col-ul-2`, `.text-truncate a[title]`, `.lazy[data-original]`. |
| `animego_anime.html` | `anicli/providers/animego.py:52-59` — anime page; selector `.br-2 .my-list-anime` with `id="my-list-<id>"`. |
| `animego_player_series.json` | `anicli/providers/animego.py:61-81` — player API JSON with `content` HTML; selector `#video-carousel .mb-0` with `data-episode`, `data-id`, `data-episode-title`. |
| `animego_player_film.json` | `anicli/providers/animego.py:82-105` — player API JSON with `content` HTML lacking `#video-carousel`; film path parses `#video-dubbing .mb-1` (`data-dubbing`) and `#video-players > span` children WITHOUT `.mb-1` so the Python fallback selector branch (`#video-players > span`, animego.py:115) is exercised (`data-player`, `data-provide-dubbing`). |
| `animego_series.json` | `anicli/providers/animego.py:93-105` — `/anime/series` API JSON with `content` HTML carrying the same dubbing/players markup. |
| `sovetromantica_search.html` | `anicli/providers/sovetromantica.py:29-53` — search page; selectors `.anime--block`, `.anime--block__name`, first `a` descendant. |
| `sovetromantica_anime.html` | `anicli/providers/sovetromantica.py:55-82` — anime page; selectors `.episodes-slick .episode` (fallback `.episodes-list .episode`), first `a`, first `span` text `Эпизод N`. |
| `sovetromantica_episode.html` | `anicli/providers/sovetromantica.py:84-98` — episode page embedding `file: "<url>.m3u8"` in inline JS (regex `file\s*:\s*"([^"]+\.m3u8[^"]*)"`). |
| `gogoanime_search.html` | `anicli/providers/gogoanime.py:21-42` — search page; selectors `.last_episodes li`, first `a` (title/href attrs), first `img` (src attr). The fourth li models the anchor-less skip. |
| `gogoanime_anime.html` | `anicli/providers/gogoanime.py:44-71` — anime page with the hidden `#movie_id`/`#alias_anime` inputs feeding the ajax episode list. |
| `gogoanime_episodes.html` | `anicli/providers/gogoanime.py:71-95` — ajax.gogo-load.com load-list-episode body; `li` items with first `a` href and `.name` text `EP N` (nameless entry models the "0" fallback; order models the verbatim reversal). |
| `gogoanime_episode.html` | `anicli/providers/gogoanime.py:97-120` — episode page; `.anime_muti_link a` with `data-video` (protocol-relative absolutized, "Choose this server" label stripped, empty attrs skipped). |
| `animepahe_search.json` | `anicli/providers/animepahe.py:26-46` — `/api?m=search` response object with `data[]`; per item reads `title`, `session`, `poster`. |
| `animepahe_episodes_p1.json` | `anicli/providers/animepahe.py:48-91` — `/api?m=release` first page carrying `total`/`per_page`/`last_page`=2 and `data[]` (`episode`, `session`). |
| `animepahe_episodes_p2.json` | same endpoint, page 2; a fractional `episode: 2.5` pins the wire-form number rendering (`str(2.5)` → `"2.5"`). |
| `dreamcast_anime.html` | `anicli/providers/dreamcast.py:50-88` — anime page with an inline `new Playerjs("<encoded>")` script and a `/js/playerjs/...` script src. **Oracle-generated**: the encoded blob and the key inside the playerjs decode through the frozen Python `_decode_playlist`, verified 2026-09-12. |
| `dreamcast_player.js` | `anicli/providers/dreamcast.py:101-258` — unpacked playerjs carrying the `u: '#0<key>=\\'` crypt marker. Same oracle run as above. |
| `sameband_search.html` | `anicli/providers/sameband.py:23-46` — DLE search result page; selectors `.col-auto`, `.image[href]`, `.poster[title]`, `img.swiper-lazy`; models the always-prefix poster quirk. |
| `sameband_anime.html` | `anicli/providers/sameband.py:48-59` — anime page; selector `.player > .player-content > iframe[src]`. |
| `sameband_playlist.json` | `anicli/providers/sameband.py:67-81` — player JSON playlist array; per item reads `title` (optional) and `file` (`[NNNp]url` comma-joined). |
| `kodik_search.json` | `anicli/providers/kodik.py:42-95` — `/search` response with `results[]`; per item reads `title`/`title_orig` (fallback chain), `link` (protocol-relative, may be null → skip), `year`, `type`, `material_data.poster_url`/`anime_poster_url`. |
| `kodik_serial.html` | `anicli/providers/kodik.py:97-170, 185-247` — serial player page: `.serial-series-box select` options (episode numbers) + `.serial-translations-box select` options (`data-media-id`/`data-media-hash`; an option missing the id models the skip). |
| `kodik_movie.html` | `anicli/providers/kodik.py:153-168` — movie page: `.movie-translations-box select` only → single film episode (`Фильм`). |
| `kodik_default.html` | `anicli/providers/kodik.py:219-247` — no translations box; inline script with `.media_id`/`.media_hash` globals → the "Default" translation fallback. |
