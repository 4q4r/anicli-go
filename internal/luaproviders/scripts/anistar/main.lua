-- AniStar (anistar.org) — the PR138 Lua migration of the compiled Go
-- provider (internal/providers/anistar.go, PR77, written from the live
-- site — no frozen Python original; re-verified live through the
-- configured proxy 2026-10-06: the «боруто» search POST answers 10 raw
-- cards in ~0.34s, the release page serves the p2p iframe, the player
-- page serves the playlst array with the sf2/sfhd ladders). The site
-- is a DataLife Engine catalog and the roster's ONLY Windows-1251
-- wire: the encoding rides every leg, each with its own direction —
--
--   - search: the DLE full-search form POST to the site root
--     (do=search&subaction=search&story=<query>) — the story value is
--     the query ENCODED to cp1251 bytes (the same query percent-encoded
--     as UTF-8 answers zero hits, live-verified 2026-09-20). The SDK's
--     anicli.iconv decodes only (legacy bytes → UTF-8), so the script
--     carries the reverse map itself: the 0x80–0xFF byte table below is
--     walked once into a UTF-8-sequence → byte map, and a query
--     character outside the table fails loud invalid_input — the
--     compiled provider's encoder error, kept typed. The encoded bytes
--     percent-quote Python-style (spaces ride %20, never the form +).
--   - search/release/legacy-playlist pages: cp1251 wire → decoded
--     through anicli.iconv BEFORE pattern matching (the fixtures keep
--     their raw bytes; the script decodes — the production shape).
--   - the p2p player page (videoas_p2p_new.php): UTF-8 wire —
--     deliberately NOT iconv-decoded (decoding UTF-8 through a cp1251
--     decoder would mojibake «Серия» and break the dub split; the
--     compiled provider parsed the raw body too).
--
-- Observed shapes (fixtures are 2026-09-20 captures, re-probed live
-- 2026-10-06):
--
--   - results: div.news cards; ONLY cards carrying a .tags a[href*=
--     '/anime/'] category link are releases — the answer mixes
--     site-news posts and manga-reader pages into the same .news/.html
--     shape (a news pick would wall at episodes with a typed miss).
--     Title .title_left a, poster img.main-img; DLE caps the answer at
--     one page (~10 cards), no pagination (yummy/anilib precedent).
--   - episodes: the release page (#movie_video) iframes ONE of two
--     player generations. The p2p player (videoas_p2p_new.php?id=N&
--     hash=H) serves `var playlst=[{title, media_id, files[], files_mp4[]},
--     …]` — JavaScript with comments and trailing commas, sliced by
--     bounded regex scans, never evaluated. Entries group by the
--     «Серия N»/«Фильм» number (first-seen order — the array
--     interleaves the dub teams episode by episode); the title suffix
--     («Многоголосая озвучка», «(Zendos)») keys the dub, suffix-less
--     entries are the release's own voice-over under "AniStar".
--     Entries outside the two title shapes (an "OVA 2" entry and a
--     Latin-C «Cерия» typo observed live 2026-10-06) are skipped —
--     the compiled regex matched the same two shapes only, parity
--     kept. The legacy player (playlist_anistar2.php?link={slug}.html)
--     serves #PlayList spans (one per episode, embed URLs inside
--     playX('…') onclick handlers, HTML-entity-escaped query strings —
--     unescaped before they surface, the PR82 nit).
--   - streams: the p2p resolve re-fetches the player page (the
--     embedded token expiry moves) and resolves the dub's media_id
--     entry: files[] (the HLS config) wins a quality key, files_mp4[]
--     (the progressive config) fills the gaps; sources label by URL
--     shape (.m3u8 → m3u8, else mp4) and carry the site-root Referer —
--     the load-bearing header set (the an-media edge answers 403 on
--     manifest requests without it, verified live 2026-09-20). The
--     player fetch itself rides the release-page Referer at listing
--     time (the iframe context) and the site root at resolve time —
--     the compiled split, kept. Legacy embeds resolve through the
--     shared extractor factory; hosts it does not cover (vk.com,
--     myvi.ru — the bulk of the legacy playlists) surface as the typed
--     extract wall naming the host, never a silent zero.
--
-- Fresh-sandbox state (the raw_id channel, the anikado/anifilm
-- precedent): streams(raw_id, dub) receives no RawEmbeds map, so the
-- resolve state rides the RawID JSON — {n = num, p = player URL,
-- d = {dub = {media_id,…}}} for the p2p generation, {n = num,
-- e = {dub = {embed URL,…}}} for the legacy one. RawEmbeds keeps
-- carrying the player#media_id / embed references for consumers.
--
-- Typed walls (the compiled provider's, kept): a release page with
-- neither player generation, an empty playlist page and an empty
-- playlst array are not_found; an unknown dub, a media_id absent from
-- the re-fetched playlst are not_found; an empty resolve and an
-- uncovered embed host are extract_failed; a query character outside
-- cp1251 is invalid_input.

local base_url = "https://anistar.org"

-- DUB_FALLBACK is the dub key for the playlst entries whose title
-- carries no suffix — the release's own AniStar voice-over (the site
-- leaves it unnamed; «Серия 1» vs «Серия 1 Многоголосая озвучка»).
local DUB_FALLBACK = "AniStar"

-- The Go regex shapes, byte-for-byte (anicli.regexp is Go RE2):
-- the player iframe probes, the playlst entry/table/array-item scans,
-- the legacy span scan and the title split.
local PLAYER_RE = [[videoas_p2p_new\.php\?id=(\d+)&(?:amp;)?hash=([0-9a-fA-F]{8,64})]]
local PLAYLIST_RE = [[playlist_anistar2\.php\?link=([A-Za-z0-9._\-]+\.html)]]
local SPAN_RE = [[onclick="[a-zA-Z0-9_]+\('([^']+)'\s*,\s*this\)"[^>]*>([^<]+)<]]
local PLAYLST_RE = [[title:"((?:Серия|Фильм)[^"]*)",\s*media_id:"(\d+)"]]
local FILES_RE = [=[files:\[([^\]]*)\]]=]
local FILES_MP4_RE = [=[files_mp4:\[([^\]]*)\]]=]
local FILE_RE = [[title:"(\d+)",\s*file:"([^"]+)"]]
local SERIES_RE = [[^Серия\s+(\d+)\s*(.*)$]]
local MOVIE_RE = [[^Фильм\s*(.*)$]]

-- CP1251_HIGH lists the 0x80–0xFF byte map in byte order (0x98 is
-- unmapped in the code page and carries no entry; the builder skips
-- the hole). One character per byte: the reverse map keys are the
-- characters' UTF-8 byte sequences. The two invisible entries are
-- built through string.char (the VM is Lua 5.1 — \xHH escapes do not
-- exist, and invisible literals would rot in review): U+00A0 (the
-- 0xA0 NBSP) and U+00AD (the 0xAD soft hyphen).
local CP1251_HIGH =
	"ЂЃ‚ѓ„…†‡€‰Љ‹ЊЌЋЏ" ..
	"ђ‘’“”•–—™љ›њќћџ" ..
	string.char(0xC2, 0xA0) .. "ЎўЈ¤Ґ¦§Ё©Є«¬" .. string.char(0xC2, 0xAD) .. "®Ї" ..
	"°±Ііґµ¶·ё№є»јЅѕї" ..
	"АБВГДЕЖЗИЙКЛМНОПРСТУФХЦЧШЩЪЫЬЭЮЯ" ..
	"абвгдежзийклмнопрстуфхцчшщъыьэюя"

local cp1251_map

-- build_cp1251_map walks CP1251_HIGH once: character i lands at byte
-- 0x80+i-1, hopping the 0x98 hole after the 0x80–0x97 run.
local function build_cp1251_map()
	local map = {}
	local pos, byte = 1, 0x80
	while pos <= #CP1251_HIGH do
		local c = string.byte(CP1251_HIGH, pos)
		local len = 1
		if c >= 0xF0 then
			len = 4
		elseif c >= 0xE0 then
			len = 3
		elseif c >= 0xC0 then
			len = 2
		end
		map[string.sub(CP1251_HIGH, pos, pos + len - 1)] = string.char(byte)
		pos = pos + len
		byte = byte + 1
		if byte == 0x98 then
			byte = 0x99
		end
	end
	return map
end

-- cp1251_encode renders the query in the site's wire encoding: ASCII
-- passes through, every mapped character rides its byte, anything else
-- is the typed invalid_input the compiled encoder errored with.
local function cp1251_encode(s)
	if cp1251_map == nil then
		cp1251_map = build_cp1251_map()
	end
	local out = {}
	local i, n = 1, #s
	while i <= n do
		local c = string.byte(s, i)
		if c < 0x80 then
			out[#out + 1] = string.char(c)
			i = i + 1
		else
			local len = 1
			if c >= 0xF0 then
				len = 4
			elseif c >= 0xE0 then
				len = 3
			elseif c >= 0xC0 then
				len = 2
			end
			local b = cp1251_map[string.sub(s, i, i + len - 1)]
			if b == nil then
				anicli.fail("invalid_input", "query is not representable in cp1251: " .. s)
			end
			out[#out + 1] = b
			i = i + len
		end
	end
	return table.concat(out)
end

-- py_quote ports urllib.parse.quote with its default safe="/" set:
-- every byte outside the URL-unreserved set (and "/") percent-quotes
-- uppercase — spaces ride %20, never the form "+" (the compiled
-- pyQuote, re-derived on the byte string).
local function py_quote(s)
	return (string.gsub(s, "[^%w%-_%.~%/]", function(c)
		return string.format("%%%02X", string.byte(c))
	end))
end

-- trim strips the surrounding whitespace (the Go strings.TrimSpace
-- call sites: the split suffix and the legacy span titles).
local function trim(s)
	return (string.match(s or "", "^%s*(.-)%s*$"))
end

-- abs absolutizes the URLs the markup mixes freely (absolute https,
-- scheme-relative, root-relative).
local function abs(u)
	if u == nil or u == "" then
		return ""
	end
	if string.sub(u, 1, 4) == "http" then
		return u
	end
	if string.sub(u, 1, 2) == "//" then
		return "https:" .. u
	end
	if string.sub(u, 1, 1) == "/" then
		return base_url .. u
	end
	return u
end

-- regexp_all finds every match of the Go pattern in s, left to right,
-- non-overlapping: each row carries the capture table plus the full-
-- match span (the playlst parser bounds each entry's block by the next
-- match's start). Matches never shrink to empty here — every pattern
-- above requires content — and the plain-text relocation keeps the
-- scan honest when the captures contain magic characters.
local function regexp_all(pattern, s)
	local out = {}
	local pos = 1
	while pos <= #s do
		local m = anicli.regexp.match(pattern, string.sub(s, pos))
		if m == nil or m[1] == "" then
			break
		end
		local b, e = string.find(s, m[1], pos, true)
		if b == nil then
			break
		end
		out[#out + 1] = { caps = m, start = b, stop = e }
		pos = e + 1
	end
	return out
end

-- unescape_url decodes the entities the legacy spans carry inside
-- their onclick attributes (&amp; on vk query strings — the PR82
-- review-nit shape). The URL-relevant named set, single pass; plain &
-- passes through untouched.
local function unescape_url(u)
	u = string.gsub(u, "&amp;", "&")
	u = string.gsub(u, "&quot;", '"')
	u = string.gsub(u, "&lt;", "<")
	u = string.gsub(u, "&gt;", ">")
	u = string.gsub(u, "&apos;", "'")
	u = string.gsub(u, "&#39;", "'")
	return u
end

-- url_host names the embed URL's host for the uncovered-host wall (the
-- whole URL names the failure when no authority parses).
local function url_host(u)
	local host = string.match(u, "^%a[%w%+%.%-]*://([^/%s]+)")
		or string.match(u, "^//([^/%s]+)")
	if host == nil then
		return u
	end
	return host
end

-- stream_type labels a link by its URL shape: the sf2 ladders end in
-- index.m3u8 manifests, everything else (sfhd progressive, the keyed
-- sfv edge) plays as mp4.
local function stream_type(u)
	if string.find(u, ".m3u8", 1, true) then
		return "m3u8"
	end
	return "mp4"
end

-- parse_files extracts the (quality, url) pairs of one ladder array
-- body (no array → no pairs).
local function parse_files(body)
	if body == nil then
		return {}
	end
	local out = {}
	for _, loc in ipairs(regexp_all(FILE_RE, body)) do
		out[#out + 1] = { quality = loc.caps[2], url = loc.caps[3] }
	end
	return out
end

-- parse_playlst slices the player page's var playlst JS array into
-- entries. Entry bodies are bounded by the next entry's title — the
-- array's formatting (tabs, //0 comments, trailing commas) never
-- crosses a title boundary in the observed pages.
local function parse_playlst(js)
	local locs = regexp_all(PLAYLST_RE, js)
	local entries = {}
	for i, loc in ipairs(locs) do
		local block_end = #js
		if locs[i + 1] then
			block_end = locs[i + 1].start - 1
		end
		local block = string.sub(js, loc.start, block_end)
		local files_m = anicli.regexp.match(FILES_RE, block)
		local files_mp4_m = anicli.regexp.match(FILES_MP4_RE, block)
		entries[#entries + 1] = {
			title = loc.caps[2],
			media_id = loc.caps[3],
			files = parse_files(files_m and files_m[2] or nil),
			files_mp4 = parse_files(files_mp4_m and files_mp4_m[2] or nil),
		}
	end
	return entries
end

-- split_title splits a playlst title into the episode number and the
-- dub suffix (empty for the release's own voice-over). Movies count as
-- episode 1 (the movie numbering convention); anything outside both
-- shapes rides the title verbatim as the group key — unreachable for
-- entries the playlst regex surfaced, kept for parity.
local function split_title(title)
	local m = anicli.regexp.match(SERIES_RE, title)
	if m then
		return m[2], trim(m[3])
	end
	m = anicli.regexp.match(MOVIE_RE, title)
	if m then
		return "1", trim(m[2])
	end
	return title, ""
end

-- fetch_decoded GETs one URL with optional headers and decodes the
-- cp1251 body into UTF-8. The p2p player pages are UTF-8 wire and
-- must NOT ride this helper (decoding UTF-8 through the cp1251 decoder
-- would mojibake the «Серия» titles the dub split keys on).
local function fetch_decoded(url, headers)
	local resp = anicli.http.get(url, { headers = headers })
	return anicli.iconv(resp.body, "cp1251")
end

return {
	id = "anistar",
	name = "AniStar",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	-- The declared live probe: the shared RU probe («черная лагуна»)
	-- surfaces only the legacy back-catalog generation, whose vk/myvi
	-- embeds no extractor covers; «боруто» lands on the current p2p
	-- generation and exercises the supported chain (live 2026-09-20,
	-- re-verified through the proxy 2026-10-06).
	smoke_query = "боруто",

	search = function(query)
		local form = "do=search&subaction=search&story=" .. py_quote(cp1251_encode(query))
		local resp = anicli.http.post(base_url .. "/", form, "application/x-www-form-urlencoded")
		local body = anicli.iconv(resp.body, "cp1251")

		local doc = anicli.html.parse(body)
		local results = {}
		local seen = {}
		doc:find("div.news"):each(function(_, card)
			-- The /anime/ category link is the release marker; news posts
			-- and manga pages carry /news/ and /m/ sections instead.
			if card:find(".tags a[href*='/anime/']"):len() == 0 then
				return
			end
			local link = card:find(".title_left a[href]")
			if link:len() == 0 then
				return
			end
			local href = link:attr("href")
			if href == "" then
				return
			end
			local url = abs(href)
			if seen[url] then
				return
			end
			seen[url] = true
			local poster = ""
			local img = card:find("img.main-img")
			if img:len() > 0 then
				poster = abs(img:attr("src"))
			end
			results[#results + 1] = {
				title = link:text(),
				url = url,
				poster = poster,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		local page = fetch_decoded(anime_url)

		-- The player probe: the p2p iframe's id+hash query first, the
		-- legacy playlist link second, neither is the typed miss (site
		-- news share the .html URL shape).
		local player_url, legacy
		local m = anicli.regexp.match(PLAYER_RE, page)
		if m then
			player_url = base_url .. "/test/player2/videoas_p2p_new.php?id=" .. m[2] .. "&hash=" .. m[3]
			legacy = false
		else
			m = anicli.regexp.match(PLAYLIST_RE, page)
			if m then
				player_url = base_url .. "/playlist_anistar2.php?link=" .. m[2]
				legacy = true
			else
				anicli.fail("not_found", "no anistar player on page " .. anime_url)
			end
		end

		-- The per-(episode, dub) references, one row per playlst entry or
		-- legacy span. The player fetch rides the release-page Referer
		-- (the iframe context the browser sends).
		local pairs_list = {}
		if legacy then
			local playlist = fetch_decoded(player_url, { Referer = anime_url })
			local spans = regexp_all(SPAN_RE, playlist)
			if #spans == 0 then
				anicli.fail("not_found", "empty playlist for " .. anime_url)
			end
			for _, span in ipairs(spans) do
				pairs_list[#pairs_list + 1] = { title = trim(span.caps[3]), ref = unescape_url(span.caps[2]) }
			end
		else
			local player = anicli.http.get(player_url, { headers = { Referer = anime_url } })
			local entries = parse_playlst(player.body)
			if #entries == 0 then
				anicli.fail("not_found", "empty playlst for " .. anime_url)
			end
			for _, entry in ipairs(entries) do
				pairs_list[#pairs_list + 1] = { title = entry.title, ref = player_url .. "#" .. entry.media_id }
			end
		end

		-- Group by the episode number in first-seen order (the array
		-- interleaves the dub teams episode by episode); the title
		-- suffix keys the dub, suffix-less entries ride the release's
		-- own voice-over name.
		local groups = {}
		local order = {}
		for _, pair in ipairs(pairs_list) do
			local num, suffix = split_title(pair.title)
			local dub = suffix
			if dub == "" then
				dub = DUB_FALLBACK
			end
			if groups[num] == nil then
				groups[num] = { dubs = {}, media = {} }
				order[#order + 1] = num
			end
			local g = groups[num]
			if g.dubs[dub] == nil then
				g.dubs[dub] = {}
				g.media[dub] = {}
			end
			g.dubs[dub][#g.dubs[dub] + 1] = pair.ref
			g.media[dub][#g.media[dub] + 1] = string.match(pair.ref, "#(%d+)$") or pair.ref
		end

		local episodes = {}
		for _, num in ipairs(order) do
			local g = groups[num]
			local state
			if legacy then
				state = { n = num, e = g.dubs }
			else
				state = { n = num, p = player_url, d = g.media }
			end
			episodes[#episodes + 1] = {
				num = num,
				raw_id = anicli.json.encode(state),
				raw_embeds = g.dubs,
			}
		end
		return episodes
	end,

-- DUB-MISS SEMANTICS (the #159 doctrine port — the animevib
-- fix-round-3 semantics, one format across the state-carrying
-- scripts): streams() NEVER substitutes another dub. When the
-- requested dub is absent from the state's dub table, the resolve
-- walls typed not_found whose message LISTS the dubs the episode
-- ACTUALLY carries — the e/d table keys, byte-sorted (a Lua map has
-- no order; the sort is the deterministic canonical form the TUI dub
-- menu and headless re-asks consume). The scan is FETCH-FREE: the
-- state IS the carrier table. The stable marker is
-- `carries no dub "X" (episode dubs: A, B, …)` — the tui
-- dubNotCarriedFailure predicate keys on the class + marker pair, so
-- every sibling's miss opens the ask-first flow. A state carrying no
-- dub table at all → not_found zero-dubs wall (the same marker, no
-- list). A malformed raw_id — the merged "prov:{json}" bytes a
-- caller composing the convention without decomposing it hands over
-- (#157), a stale history record, a state carrying neither the e nor
-- the d table, a non-table leg — is the typed invalid_input wall,
-- never the raw json.decode VM error.

	streams = function(raw_id, dub)
		-- The state guard: raw_id is ALWAYS this script's own
		-- {n, e} (legacy) or {n, p, d} (p2p) json (the fresh-sandbox
		-- contract); any other byte sequence — the merged prov:id
		-- convention composed without decomposing it (#157), a stale
		-- history record — surfaces typed invalid_input, never the
		-- raw json.decode VM error (the animevib guard).
		local ok, state = pcall(anicli.json.decode, raw_id)
		if not ok or type(state) ~= "table"
			or (type(state.e) ~= "table" and type(state.d) ~= "table")
			or (state.e ~= nil and type(state.e) ~= "table")
			or (state.d ~= nil and type(state.d) ~= "table") then
			anicli.fail("invalid_input",
				"episode raw_id is not the {n, e} or {n, p, d} state json: \"" ..
				string.sub(tostring(raw_id), 1, 64) .. "\"")
		end
		local num = tostring(state.n)

		-- The dub-miss wall: a dub the episode does not carry is the
		-- typed not_found miss (the #159 doctrine, see the header) —
		-- never the caller-mistake class, never a substitution. The
		-- scan rides the state's own dub table, no extra fetches.
		local dub_table
		if type(state.e) == "table" then
			dub_table = state.e
		elseif type(state.d) == "table" then
			dub_table = state.d
		end
		local refs = (dub_table and dub_table[dub]) or {}
		if #refs == 0 then
			local carriers = {}
			if dub_table then
				for name in pairs(dub_table) do
					carriers[#carriers + 1] = name
				end
				table.sort(carriers)
			end
			if #carriers == 0 then
				-- the ONLY dub wall with no ask behind it: the state
				-- carries no dub at all — a data-shape fact, not a
				-- caller mistake.
				anicli.fail("not_found", "episode " .. num .. " carries no dub in its player references")
			end
			anicli.fail("not_found", "episode " .. num .. ' carries no dub "' .. dub ..
				'" (episode dubs: ' .. table.concat(carriers, ", ") .. ')')
		end

		local links = {}
		-- The first-error carrier is a TABLE CELL, not a plain local:
		-- the sandbox VM (gopher-lua) drops plain-local upvalue writes
		-- made by a closure after a raising pcall in the same scope
		-- (isolated 2026-10-06: a local first_err mutated via record()
		-- stays nil at the wall, a { err = … } cell survives it) — the
		-- recorded wall would silently degrade to "no playable links".
		local err_state = { err = nil, kind = nil }
		local function record(kind, msg)
			if err_state.err == nil then
				err_state.err = msg
				err_state.kind = kind
			end
		end

		if state.e ~= nil then
			-- The legacy shape: the RawEmbeds value IS the embed URL —
			-- resolve through the shared extractor factory. The factory
			-- settles hosts it does not cover (vk.com, myvi.ru) with the
			-- no-extractor raise, surfaced here as the loud typed failure
			-- naming the host, never a silent zero.
			for _, ref in ipairs(refs) do
				local ok, sources = pcall(anicli.extract, ref)
				if not ok then
					local msg = tostring(sources)
					if string.find(msg, "no extractor yielded links", 1, true) then
						record("extract_failed", "no extractor for embed host " .. url_host(ref))
					else
						record("extract_failed", msg)
					end
				else
					for quality, src in pairs(sources) do
						links[quality] = src -- dict.update: later links overwrite
					end
				end
			end
		else
			-- The p2p shape: re-fetch the player page (the embedded token
			-- expiry moves) and resolve the dub's media_id entry. The
			-- resolve-time fetch rides the site root Referer (the
			-- compiled fetchPlaylst split).
			for _, media_id in ipairs(refs) do
				local ok, entries = pcall(function()
					local player = anicli.http.get(state.p, { headers = { Referer = base_url .. "/" } })
					local parsed = parse_playlst(player.body)
					if #parsed == 0 then
						anicli.fail("extract_failed", "empty playlst at " .. state.p)
					end
					return parsed
				end)
				if not ok then
					record("extract_failed", tostring(entries))
				else
					local entry
					for _, e in ipairs(entries) do
						if e.media_id == media_id then
							entry = e
							break
						end
					end
					if entry == nil then
						record("not_found", "media_id " .. media_id .. " absent from the playlst")
					else
						-- The HLS ladder wins the quality key, the
						-- progressive ladder fills the gaps; every source
						-- carries the load-bearing site-root Referer (the
						-- an-media edge answers 403 without it).
						local headers = { Referer = base_url .. "/" }
						for _, f in ipairs(entry.files) do
							links[f.quality] = {
								url = f.url,
								quality = f.quality,
								type = stream_type(f.url),
								headers = headers,
							}
						end
						for _, f in ipairs(entry.files_mp4) do
							if links[f.quality] == nil then
								links[f.quality] = {
									url = f.url,
									quality = f.quality,
									type = stream_type(f.url),
									headers = headers,
								}
							end
						end
					end
				end
			end
		end

		if next(links) == nil then
			if err_state.err ~= nil then
				anicli.fail(err_state.kind, err_state.err)
			end
			anicli.fail("extract_failed", string.format('no playable links for dub "%s" episode %s', dub, num))
		end
		return { dub_name = dub, links = links }
	end,
}
