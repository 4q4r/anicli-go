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
