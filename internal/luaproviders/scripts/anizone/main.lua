-- AniZone (anizone.to) — the PR130 Lua migration of the compiled Go
-- provider (internal/providers/anizone.go, PR59). The catalog is a
-- foreign sub-only stream source with no frozen anicli-py original:
-- the extraction recipe came from the Anivexa aggregator's AniZone
-- provider (github.com/walterwhite-69/Anivexa-API providers/anizone.js)
-- and was re-verified live 2026-09-18. It is a Laravel Livewire site —
-- every list surface is a server-rendered page whose inline script
-- carries an `items: JSON.parse('…')` payload, and long catalogs walk
-- through POST /livewire/update continuations.
--
-- Shapes:
--   - search: GET /anime?search={q} renders the Anime Index page; the
--     items payload carries slug, cover, main_title, title_list, type
--     and start_year rows. The site indexes romaji/English titles
--     only, so a Cyrillic query is guaranteed-zero — and the legit
--     no-results answer is the FULL index page with the Livewire
--     search input rendered but NO items payload script at all (the
--     PR78 clean-miss rule: that page settles as zero results, never
--     a typed failure).
--   - episodes: GET /anime/{slug} carries the episode rows (slug IS
--     the episode number, or "s1" for specials), the entity-encoded
--     wire:snapshot attribute naming pages.anime-detail, the csrf
--     meta and the nextCursor/hasMore pagination state. While
--     hasMore holds, POST /livewire/update continuations fetch the
--     next page: the decoded snapshot + a loadPage(cursor) call in,
--     the next items-loaded dispatch (items, snapshot, nextCursor,
--     hasMore) out. Session cookies rotate every response — the
--     per-provider netclient jar replays them; the page csrf token
--     stays valid across pages (live-verified in PR59). The walk is
--     bounded (200 pages, the largest live catalog needs 50) and the
--     bound failure is typed, never a silent truncation.
--   - streams: GET /anime/{slug}/{number} carries the vidstackPlayer
--     payload whose src is the HLS master playlist (360/720/1080
--     variants over selectable ja/en audio groups, ja default); the
--     playlist splits into per-height variant links with the site
--     root as their Referer. Subtitle sidecars and storyboard/chapter
--     tracks have no slot in the stream contract — documented, out of
--     scope.
--
-- State: the Livewire csrf/snapshot round-trip is state, but the
-- fresh-sandbox contract means every invocation re-derives it —
-- episodes() fetches the series page and walks continuations inside
-- one invocation; nothing crosses invocation boundaries. The watch
-- URL rides raw_id as the {"n": number, "u": url} JSON (the
-- animevost/anilib state-carrier precedent), so streams() re-derives
-- its target from raw_id alone.
--
-- The site's single fixed dub label (every episode serves the
-- Japanese-audio HLS with subtitle sidecars; no dubs exist).
local AZ_DUB = "Original (AniZone)"

local base_url = "https://anizone.to"

-- The Livewire episode walk bound (page size 24).
local MAX_SERIES_PAGES = 200

-- Go-regexp patterns the shared SDK regexp bridge evaluates (RE2
-- syntax — the compiled provider's patterns verbatim).
local ITEMS_RE = "(?i)items\\s*:\\s*JSON\\.parse\\('((?:[^'\\\\]|\\\\.)*)'\\)"
local PLAYER_RE = "(?i)vidstackPlayer\\s*\\(\\s*JSON\\.parse\\('((?:[^'\\\\]|\\\\.)*)'\\)\\s*\\)"
local SNAPSHOT_RE = "wire:snapshot=\"([^\"]*)\""
local CURSOR_RE = "nextCursor:\\s*'([^']+)'"
local HAS_MORE_RE = "(?i)hasMore:\\s*true"
local CSRF_RE = "csrf-token\"\\s+content=\"([^\"]+)\""
local SEARCH_INPUT_RE = "wire:model[\\w.-]*=\"search\""
local SLUG_RE = "(?i)^[a-z0-9-]+$"
local TRAILING_NUMBER_RE = "/(\\d+)/?$"
local DOUBLE_UNICODE_RE = "\\\\u(%x%x%x%x)"
local RESOLUTION_RE = "RESOLUTION=(\\d+)[xX](\\d+)"

-- decode passes for the inline payloads: the argument is a
-- single-quoted JS string literal, so JS escapes must come off before
-- JSON parsing. The server double-escapes: "\\u0022" is a JS escape
-- for a quote (structure quotes in the captured payload), while
-- "\\\\u041F" is a JS-escaped backslash followed by u041F — a literal
-- JSON "\\u041F" escape the JSON parser must see verbatim. Pass one
-- protects the double-escaped form behind a marker, pass two decodes
-- the remaining JS unicode escapes (astral chars arrive as surrogate
-- PAIRS — combined like String.fromCharCode), pass three restores the
-- protected escapes and hands the result to the JSON parser. Any
-- failure is typed, never a silent null (the JS original returned
-- null; the port fails loud — the house rule).
local MARKER = "\1U\1"

-- utf8_encode renders one code point the way Go's rune write did;
-- U+FFFD marks an unpaired surrogate (no representable character —
-- the rest of the payload survives).
local function utf8_encode(cp)
	if cp < 0x80 then
		return string.char(cp)
	end
	if cp < 0x800 then
		return string.char(0xC0 + math.floor(cp / 0x40), 0x80 + cp % 0x40)
	end
	if cp < 0x10000 then
		return string.char(0xE0 + math.floor(cp / 0x1000),
			0x80 + math.floor(cp / 0x40) % 0x40, 0x80 + cp % 0x40)
	end
	return string.char(0xF0 + math.floor(cp / 0x40000),
		0x80 + math.floor(cp / 0x1000) % 0x40,
		0x80 + math.floor(cp / 0x40) % 0x40, 0x80 + cp % 0x40)
end

local function is_hex_digit(c)
	return (c >= 48 and c <= 57) or (c >= 97 and c <= 102) or (c >= 65 and c <= 70)
end

-- The sandbox's registry bounds table.concat inputs (a 90k-element
-- table overflows the VM stack), so pass two accumulates through a
-- chunked builder: small concat blocks flush into a parts list, and
-- plain-byte runs ride single substrings.
local BUILDER_BLOCK = 2048

local function new_builder()
	return { inner = {}, parts = {}, n = 0 }
end

local function builder_add(b, s)
	b.n = b.n + 1
	b.inner[b.n] = s
	if b.n >= BUILDER_BLOCK then
		b.parts[#b.parts + 1] = table.concat(b.inner)
		b.inner = {}
		b.n = 0
	end
end

local function builder_build(b)
	if b.n > 0 then
		b.parts[#b.parts + 1] = table.concat(b.inner)
	end
	return table.concat(b.parts)
end

-- decode_js_unicode escapes: \\uXXXX sequences (four hex digits, no
-- more, no less) decode; everything else passes through verbatim —
-- including the JSON-legal \\/ escapes the recipe's URL normalization
-- collapses AFTER the JSON round-trip.
local function decode_unicode_escapes(raw)
	local out = new_builder()
	local i, n = 1, #raw
	while i <= n do
		local c = string.byte(raw, i)
		if c == 92 and i + 6 <= n and string.byte(raw, i + 1) == 117
			and is_hex_digit(string.byte(raw, i + 2))
			and is_hex_digit(string.byte(raw, i + 3))
			and is_hex_digit(string.byte(raw, i + 4))
			and is_hex_digit(string.byte(raw, i + 5)) then
			local cp = tonumber(string.sub(raw, i + 2, i + 5), 16)
			if cp >= 0xD800 and cp <= 0xDBFF and i + 12 <= n
				and string.byte(raw, i + 6) == 92 and string.byte(raw, i + 7) == 117
				and is_hex_digit(string.byte(raw, i + 8))
				and is_hex_digit(string.byte(raw, i + 9))
				and is_hex_digit(string.byte(raw, i + 10))
				and is_hex_digit(string.byte(raw, i + 11)) then
				local low = tonumber(string.sub(raw, i + 8, i + 11), 16)
				if low >= 0xDC00 and low <= 0xDFFF then
					builder_add(out, utf8_encode(0x10000 + (cp - 0xD800) * 0x400 + (low - 0xDC00)))
					i = i + 12
				else
					builder_add(out, utf8_encode(0xFFFD))
					i = i + 6
				end
			elseif cp >= 0xD800 and cp <= 0xDFFF then
				builder_add(out, utf8_encode(0xFFFD))
				i = i + 6
			else
				builder_add(out, utf8_encode(cp))
				i = i + 6
			end
		else
			-- Plain run: append everything up to the next escape as one
			-- substring instead of byte-wise entries.
			local run = i
			while run <= n do
				local rc = string.byte(raw, run)
				if rc == 92 and run + 6 <= n and string.byte(raw, run + 1) == 117
					and is_hex_digit(string.byte(raw, run + 2))
					and is_hex_digit(string.byte(raw, run + 3))
					and is_hex_digit(string.byte(raw, run + 4))
					and is_hex_digit(string.byte(raw, run + 5)) then
					break
				end
				run = run + 1
			end
			builder_add(out, string.sub(raw, i, run - 1))
			i = run
		end
	end
	return builder_build(out)
end

-- decode_json_argument extracts nothing itself: the caller passes the
-- JS string literal CONTENT and receives the parsed JSON value. A
-- payload that fails the round-trip is a typed extract wall.
local function decode_json_argument(raw, what)
	local protected = string.gsub(raw, DOUBLE_UNICODE_RE, MARKER .. "%1")
	local restored = string.gsub(decode_unicode_escapes(protected), MARKER, "\\u")
	local ok, decoded = pcall(anicli.json.decode, restored)
	if not ok then
		anicli.fail("extract_failed", "decode " .. what
			.. " payload: decoded payload is not valid JSON")
	end
	return decoded
end

-- pick_title ports pickTitle (providers/anizone.js:92-94): the "1"
-- (romanized) title, then "5", then "8", then the first value. The JS
-- first-value tail follows insertion order; Lua hash tables have none,
-- so the fallback walks the keys sorted — deterministic, same
-- first-value spirit (the tail only fires for rows missing all three
-- keys).
local function pick_title(titles)
	if type(titles) ~= "table" then
		return ""
	end
	for _, key in ipairs({ "1", "5", "8" }) do
		local v = titles[key]
		if type(v) == "string" and v ~= "" then
			return v
		end
	end
	local keys = {}
	for k in pairs(titles) do
		keys[#keys + 1] = tostring(k)
	end
	table.sort(keys)
	for _, k in ipairs(keys) do
		local v = titles[k]
		if type(v) == "string" and v ~= "" then
			return v
		end
	end
	return ""
end

-- normalize_url ports normalizeUrl: collapsed escaped slashes (the
-- recipe's /\\+\//g guard for the double-escaped CDN URLs). In a Lua
-- pattern the backslash is a literal, so one-or-more backslashes
-- before the slash is spelled with a bare + quantifier.
local function normalize_url(s)
	return (string.gsub(s or "", "\\+/", "/"))
end

-- html_unescape decodes the entity-encoded wire:snapshot attribute in
-- one left-to-right pass (each entity consumed once, so "&amp;quot;"
-- yields "&quot;" — the Go html.UnescapeString semantics; unknown
-- entities stay verbatim). Lua patterns have no alternation, so the
-- generic &…; form matches and the named/numeric split happens in the
-- replacement function.
local NAMED_ENTITIES = { amp = "&", lt = "<", gt = ">", quot = "\"", apos = "'" }
local function html_unescape(s)
	return (string.gsub(s, "&([%w#]+);", function(ent)
		local named = NAMED_ENTITIES[ent]
		if named then
			return named
		end
		if string.sub(ent, 1, 1) == "#" then
			local cp
			if string.sub(ent, 2, 2) == "x" or string.sub(ent, 2, 2) == "X" then
				cp = tonumber(string.sub(ent, 3), 16)
			else
				cp = tonumber(string.sub(ent, 2))
			end
			if cp and cp >= 0 and cp <= 0x10FFFF then
				return utf8_encode(math.floor(cp))
			end
		end
		return "&" .. ent .. ";"
	end))
end

-- find_detail_snapshot walks every wire:snapshot attribute and takes
-- the pages.anime-detail component's (the recipe snapshot()).
local function find_detail_snapshot(body)
	local rest, offset = body, 0
	while true do
		local m = anicli.regexp.match(SNAPSHOT_RE, rest)
		if not m then
			return ""
		end
		local full, attr = m[1], m[2]
		local at = string.find(rest, full, 1, true)
		if not at then
			return ""
		end
		if string.find(attr, "pages.anime-detail", 1, true) then
			return html_unescape(attr)
		end
		rest = string.sub(rest, at + #full)
		offset = offset + at + #full - 1
	end
end

-- regexp_first is the one-capture convenience: the SDK bridge exposes
-- m[1] = full match, m[2] = first capture; nil when absent.
local function regexp_first(pattern, s)
	local m = anicli.regexp.match(pattern, s)
	if not m then
		return nil
	end
	return m[2]
end

-- The recipe's page headers: document accept over the site root
-- referer; the netclient carries the browser UA.
local function page_headers()
	return {
		["Referer"] = base_url .. "/",
		["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		["Accept-Language"] = "en-US,en;q=0.9",
	}
end

-- extract_items finds and decodes the `items: JSON.parse('…')`
-- payload of a Livewire page; nil when the page carries none.
local function extract_items(body)
	local payload = regexp_first(ITEMS_RE, body)
	if not payload then
		return nil
	end
	local items = decode_json_argument(payload, "items")
	if type(items) ~= "table" then
		return nil
	end
	return items
end

-- episode_number ports episodeNumber: the numeric slug, else the
-- trailing URL digits. Specials ("s1") carry neither — dropped.
local function episode_number(item)
	local slug = tostring(item.slug or "")
	local n = tonumber(slug)
	if n and n > 0 and math.floor(n) == n then
		return n, true
	end
	local m = anicli.regexp.match(TRAILING_NUMBER_RE, tostring(item.url or ""))
	if m then
		n = tonumber(m[2])
		if n and n > 0 then
			return n, true
		end
	end
	return nil, false
end

-- parse_episodes ports parseEpisodes: numeric episodes only, deduped
-- by number (FIRST occurrence wins), sorted ascending, gated on
-- videos_count (the has-sub property; the site has no dubs). The
-- watch URL rides BOTH the raw_embeds dub key and the raw_id {"n",u}
-- state — streams() consumes the raw_id (the fresh-sandbox contract:
-- streams receives raw_id and the dub only).
local function parse_episodes(slug, items)
	local seen, entries = {}, {}
	for _, item in ipairs(items) do
		local number, ok = episode_number(item)
		if ok and not seen[number] then
			seen[number] = true
			local title = pick_title(item.title_list)
			if title == "" then
				title = "Episode " .. string.format("%d", number)
			end
			entries[#entries + 1] = {
				number = number,
				title = title,
				has_sub = (tonumber(item.videos_count) or 0) > 0,
			}
		end
	end
	table.sort(entries, function(a, b)
		return a.number < b.number
	end)

	local episodes = {}
	for _, e in ipairs(entries) do
		if e.has_sub then
			local num = string.format("%d", e.number)
			local watch_url = base_url .. "/anime/" .. slug .. "/" .. num
			episodes[#episodes + 1] = {
				num = num,
				title = e.title,
				raw_id = anicli.json.encode({ n = num, u = watch_url }),
				raw_embeds = { [AZ_DUB] = { watch_url } },
			}
		end
	end
	return episodes
end

-- load_page performs one /livewire/update continuation (recipe
-- loadPage): the current snapshot + loadPage(cursor) in, the next
-- page's rows and walk state out via the items-loaded dispatch. The
-- page csrf token stays valid across pages (live-verified), and the
-- netclient jar replays the rotating session cookies.
local function load_page(slug, state)
	local body = anicli.json.encode({
		components = { {
			snapshot = state.snapshot,
			updates = {},
			calls = { { path = "", method = "loadPage", params = { state.cursor } } },
		} },
	})
	local resp = anicli.http.post(base_url .. "/livewire/update", body, "application/json",
		{ headers = {
			["Referer"] = base_url .. "/anime/" .. slug,
			["Accept"] = "application/json, text/plain, */*",
			["X-Livewire"] = "",
			["X-CSRF-TOKEN"] = state.csrf,
			["X-Requested-With"] = "XMLHttpRequest",
			["Origin"] = base_url,
			["Accept-Language"] = "en-US,en;q=0.9",
		} })

	local ok, payload = pcall(anicli.json.decode, resp.body)
	if not ok or type(payload) ~= "table" or type(payload.components) ~= "table"
		or #payload.components ~= 1 then
		anicli.fail("extract_failed", "decode livewire response: continuation payload not found")
	end
	local component = payload.components[1]
	if type(component) ~= "table" or (component.snapshot or "") == "" then
		anicli.fail("extract_failed", "livewire continuation payload not found")
	end
	for _, dispatch in ipairs(component.effects and component.effects.dispatches or {}) do
		if dispatch.name == "items-loaded" then
			local params = dispatch.params or {}
			if params.items == nil then
				break
			end
			return {
				items = params.items,
				snapshot = component.snapshot,
				cursor = params.nextCursor or "",
				has_more = params.hasMore == true,
				csrf = state.csrf,
			}
		end
	end
	anicli.fail("extract_failed", "livewire items-loaded dispatch not found")
end

-- resolve_reference ports the URL resolution the playlist split
-- needs: absolute refs stay, scheme-relative gain the scheme,
-- root-relative gain the origin, bare paths join the embedding
-- directory.
local function resolve_reference(base, ref)
	if string.match(ref, "^%a+://") then
		return ref
	end
	if string.sub(ref, 1, 2) == "//" then
		return (string.match(base, "^(%a+:)") or "https:") .. ref
	end
	local origin = string.match(base, "^(%a+://[^/?#]+)") or ""
	if string.sub(ref, 1, 1) == "/" then
		return origin .. ref
	end
	local dir = string.match(base, "^(.*/)") or origin .. "/"
	return dir .. ref
end

-- master_links splits the fetched manifest: a master playlist yields
-- one link per RESOLUTION height (the default when a STREAM-INF
-- carries no RESOLUTION is 1080; relative URIs resolve against the
-- manifest URL), anything else means the src IS the stream — the
-- single 1080 entry. Every link keeps the site root as its Referer
-- (the PR51 convention; the live CDN also answers referer-less).
local function master_links(body, manifest_url, origin_referer)
	if not string.find(body, "#EXT-X-STREAM-INF", 1, true) then
		return {
			["1080"] = {
				url = manifest_url, quality = "1080",
				type = "m3u8", headers = { Referer = origin_referer },
			},
		}
	end
	local links = {}
	local pending = ""
	for line in string.gmatch(body .. "\n", "(.-)\n") do
		line = string.gsub(line, "\r$", "")
		if string.sub(line, 1, 18) == "#EXT-X-STREAM-INF:" then
			pending = "1080"
			local res = anicli.regexp.match(RESOLUTION_RE, line)
			if res then
				pending = res[3]
			end
		elseif line == "" or string.sub(line, 1, 1) == "#" then
			-- attributes may continue on the same line only
		elseif pending ~= "" then
			links[pending] = {
				url = resolve_reference(manifest_url, line),
				quality = pending,
				type = "m3u8",
				headers = { Referer = origin_referer },
			}
			pending = ""
		end
	end
	return links
end

return {
	id = "anizone",
	name = "AniZone",
	base_url = base_url,
	capabilities = "both",
	-- The catalog serves the Japanese-audio HLS with subtitle sidecars;
	-- no dubs exist on the site (PR59).
	content_lang = "ja",
	-- The site indexes romaji/English titles only (PR42): a Cyrillic
	-- query is guaranteed-zero.
	name_preference = "latin",

	search = function(query)
		local resp = anicli.http.get(
			base_url .. "/anime?search=" .. anicli.http.query_escape(query),
			{ headers = page_headers() })

		local items = extract_items(resp.body)
		if items == nil then
			-- The legit no-results page (verbatim capture 2026-09-20, a
			-- cyrillic query): the full Anime Index Livewire page with an
			-- empty result block and NO items payload script. A clean
			-- miss, not a shape drift — the fan-out legitimately reaches
			-- this latin-only index with cyrillic-only variant sets, and
			-- a miss must settle as zero results, never an error (PR78).
			if anicli.regexp.match(SEARCH_INPUT_RE, resp.body) then
				return {}
			end
			anicli.fail("extract_failed", "search payload not found")
		end

		-- parseSearchItems: slug-shaped rows with a title only.
		local results = {}
		for _, item in ipairs(items) do
			local slug = tostring(item.slug or "")
			if anicli.regexp.match(SLUG_RE, slug) then
				local title = pick_title(item.title_list)
				if title == "" then
					title = tostring(item.main_title or "")
				end
				if title ~= "" then
					results[#results + 1] = {
						title = title,
						url = slug, -- the hash slug doubles as the anime id
						poster = normalize_url(tostring(item.cover or "")),
						meta = {
							year = item.start_year,
							type = tostring(item.type or ""),
						},
					}
				end
			end
		end
		return results
	end,

	episodes = function(anime_url)
		local slug = tostring(anime_url or "")
		local resp = anicli.http.get(base_url .. "/anime/" .. slug,
			{ headers = page_headers() })

		-- The initial page's rows plus everything the continuation
		-- calls need (recipe initialPage): items, the detail snapshot,
		-- the csrf — all three are required for the walk, their absence
		-- means a challenge page or a shape drift.
		local items = extract_items(resp.body)
		local snapshot = find_detail_snapshot(resp.body)
		local csrf = regexp_first(CSRF_RE, resp.body) or ""
		if items == nil or #items == 0 or snapshot == "" or csrf == "" then
			anicli.fail("extract_failed", "series page payload not found")
		end

		local state = {
			items = items,
			snapshot = snapshot,
			cursor = regexp_first(CURSOR_RE, resp.body) or "",
			has_more = anicli.regexp.match(HAS_MORE_RE, resp.body) ~= nil,
			csrf = csrf,
		}

		-- The walk: continuations while hasMore + nextCursor hold
		-- (recipe scrapeSeries), bounded — hitting the bound is a typed
		-- error, never a silent truncation.
		local all, pages = {}, 1
		for _, item in ipairs(state.items) do
			all[#all + 1] = item
		end
		while state.has_more and state.cursor ~= "" do
			if pages >= MAX_SERIES_PAGES then
				anicli.fail("extract_failed",
					"series listing exceeded " .. MAX_SERIES_PAGES .. " pages")
			end
			state = load_page(slug, state)
			for _, item in ipairs(state.items) do
				all[#all + 1] = item
			end
			pages = pages + 1
		end

		-- The recipe throws on an empty list; the port keeps the typed
		-- class (a series page that decodes but carries no resolvable
		-- episode is a not-found, never a silent empty set).
		local episodes = parse_episodes(slug, all)
		if #episodes == 0 then
			anicli.fail("not_found", "no resolvable episodes for " .. slug)
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- The compiled provider's semantics: no resolvable watch state
		-- resolves to an EMPTY stream without error (the session skips
		-- the row). A non-empty raw_id that does not parse is a caller
		-- bug — typed invalid input (the anikoto raw-shape precedent).
		if (raw_id or "") == "" then
			return { dub_name = AZ_DUB, links = {} }
		end
		local ok, state = pcall(anicli.json.decode, raw_id)
		if not ok or type(state) ~= "table" or (state.u or "") == "" then
			anicli.fail("invalid_input",
				"episode RawID is not the {\"n\",u} watch state")
		end

		local watch_url = state.u
		local page = anicli.http.get(watch_url, { headers = page_headers() })
		local payload = regexp_first(PLAYER_RE, page.body)
		if not payload then
			anicli.fail("extract_failed",
				"player payload not found for " .. watch_url)
		end
		local player = decode_json_argument(payload, "player")
		local src = normalize_url(tostring(type(player) == "table" and player.src or ""))
		if src == "" then
			anicli.fail("extract_failed", "player payload carries no src")
		end

		-- The master playlist is fetched with the site root as its
		-- Referer (the original PR51 convention; the live CDN also
		-- answers referer-less).
		local playlist_headers = { Referer = base_url }
		local playlist = anicli.http.get(src, { headers = playlist_headers })
		return { dub_name = AZ_DUB, links = master_links(playlist.body, src, base_url) }
	end,
}
