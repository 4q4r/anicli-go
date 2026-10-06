-- AnimeMobi (animemobi.com) — the PR137 Lua migration of the compiled
-- Go provider (internal/providers/animemobi.go, PR92, itself written
-- from the live site: no frozen Python original — the anidub/anistar
-- precedent). The RU mobile-oriented DLE catalog on UTF-8 (unlike
-- anistar's cp1251), anonymous plain-HTTPS surface. The observed
-- shapes:
--
--   - search: the DLE full-search form POST to the site root
--     (do=search&subaction=search&story=…, UTF-8). The operator's
--     User-Agent decides which DLE skin renders, and the skins mark
--     result rows differently (both captured live 2026-09-23, same
--     query, same answers): the smartphone skin (mobile UA) marks
--     div.shortstory rows with h2.title anchors, the desktop skin
--     (desktop UA) marks div.base rows with div.bheading h1.heading
--     anchors. The row's heading anchor carries the release URL and
--     title; the poster is the row's first /uploads/ img (the row
--     container differs per skin, the poster img inside it does not).
--     The DLE full-search mixes site news, manga reader pages, AMVs,
--     covers and top-lists into the answer, so every anchor outside
--     the /anime-rus/ and /anime-sub/ sections drops (the anistar
--     news/manga filter precedent). DLE caps the form answer at one
--     page (~10 rows); no pagination is attempted (the yummy/anilib/
--     anistar/animiku single-page precedent).
--   - episodes: the release page's a.onlinevideo anchors map onto
--     episodes — each anchor is one (episode, embed) pair: «Серия NN»
--     labels the per-episode kodikplayer.com /seria/ links, «Фильм NN»
--     the movies, and a label-free «Смотреть» anchor is the
--     whole-season /season/ link counted as episode 1. Zero-padded
--     numbers normalize to canonical decimals ("01" → "1"). A page
--     without anchors (site news share the .html URL shape) is the
--     typed not-found. One voice-over per release: the page's single
--     «Озвучка:» credit (entity-encoded: &#91;…]<br>) keys the dub;
--     releases without the credit (the /anime-sub/ section) fall back
--     to the catalog's own name — the compiled provider's
--     animemobiDubFallback. The per-release torrent downloads
--     (div.yadivk → do=download → the animemobi.top mirror) are a
--     download pipeline out of the stream contract — documented in the
--     PR92 Go source's git history, not wired.
--   - streams: the embed refs resolve through anicli.extract (the
--     shared Go extractor factory; the kodik extractor Matches both
--     the kodikplayer.com and aniqit.com hosts). The returned sources
--     type by URL shape: kodik ladders end in .m3u8 HLS manifests,
--     everything else plays progressive.
--
-- State passing (the fresh-sandbox contract): streams(raw_id, dub)
-- receives strings only, so raw_id encodes {n = num, d = dub,
-- r = embed-ref} as JSON — the embed is fully determined at listing
-- time (one dub, one ref per episode), so the resolve stays a
-- zero-fetch extract (the anikado movie-state precedent). The credited
-- dub rides the state, so a dub the episode does not carry keeps the
-- compiled provider's typed RawEmbeds-miss wall (not_found) without
-- touching the network.
--
-- Route (LIVE DRIFT, 2026-10-06): since the PR116 verification matrix
-- the site tarpits the netclient's TLS fingerprint — the search POST
-- times out on both routes (silent connection drop direct, EOF through
-- the proxy), while a plain browser-UA client still answers 200 with
-- result rows. The compiled PR92 provider failed the smoke the same
-- way on the same day (identical live behavior — the
-- migration-success rule); the fixtures pin the parsing contract.

local base_url = "https://animemobi.com"

-- SERVICE_DUB is the dub name for releases whose page credits no
-- «Озвучка:» field (the site's own subtitled /anime-sub/ section among
-- them) — the catalog's own name, mirroring anistar's fallback.
local SERVICE_DUB = "AnimeMobi"

-- abs absolutizes the root-relative asset URLs the markup mixes
-- freely (the /uploads/ posters): protocol-relative gains https,
-- site-relative gains the base, everything else passes through
-- (the compiled animemobiAbsURL semantics).
local function abs(u)
	if u == nil or u == "" then
		return ""
	end
	if string.sub(u, 1, 2) == "//" then
		return "https:" .. u
	end
	if string.sub(u, 1, 1) == "/" then
		return base_url .. u
	end
	return u
end

local function trim(s)
	return (string.match(s or "", "^%s*(.-)%s*$"))
end

-- utf8_encode encodes one codepoint as UTF-8 (arithmetic only — the
-- Lua 5.1 VM has no bitwise operators).
local function utf8_encode(cp)
	local bytes
	if cp < 128 then
		bytes = { cp }
	elseif cp < 2048 then
		bytes = { 192 + math.floor(cp / 64), 128 + cp % 64 }
	elseif cp < 65536 then
		bytes = {
			224 + math.floor(cp / 4096),
			128 + math.floor(cp / 64) % 64,
			128 + cp % 64,
		}
	else
		bytes = {
			240 + math.floor(cp / 262144),
			128 + math.floor(cp / 4096) % 64,
			128 + math.floor(cp / 64) % 64,
			128 + cp % 64,
		}
	end
	return string.char(unpack(bytes))
end

-- NAMED_ENTITIES is the core named-entity set the info fields carry;
-- unknown names keep their source text verbatim (the compiled Go
-- html.UnescapeString semantics for unknown entities).
local NAMED_ENTITIES = {
	amp = "&",
	lt = "<",
	gt = ">",
	quot = "\"",
	apos = "'",
	nbsp = "\194\160",
}

-- unescape decodes the HTML entities the release info fields carry
-- (the Go html.UnescapeString port over the realistic surface: the
-- numeric decimal/hex refs — UTF-8 encoded; surrogate and out-of-range
-- codepoints keep their source text) plus the core named set.
local function unescape(s)
	s = string.gsub(s, "&#([%d]+);", function(digits)
		local cp = tonumber(digits)
		if cp == nil or cp < 0 or cp > 0x10FFFF or (cp >= 0xD800 and cp <= 0xDFFF) then
			return nil -- keep the source text verbatim
		end
		return utf8_encode(cp)
	end)
	s = string.gsub(s, "&#[xX]([%x]+);", function(digits)
		local cp = tonumber(digits, 16)
		if cp == nil or cp > 0x10FFFF or (cp >= 0xD800 and cp <= 0xDFFF) then
			return nil
		end
		return utf8_encode(cp)
	end)
	s = string.gsub(s, "&(%a+);", function(name)
		return NAMED_ENTITIES[name] -- nil keeps the source text
	end)
	return s
end

-- dub_name extracts the release's voice-over credit: the «Озвучка:»
-- field regex runs on the RAW page (the compiled animemobiDubRe
-- verbatim — the leading «Команда Озвучки:» field never matches, its
-- text is «Озвучки»), the captured value is entity-decoded, trimmed
-- and stripped of the literal [ ] brackets; no credit or an empty
-- value falls back to SERVICE_DUB.
local function dub_name(page)
	local m = anicli.regexp.match("Озвучка:</b>\\s*(?:<[^>]+>)*\\s*([^<]+)", page)
	if m == nil then
		return SERVICE_DUB
	end
	local dub = trim(unescape(m[2]))
	dub = string.gsub(dub, "^%[", "")
	dub = string.gsub(dub, "%]$", "")
	if dub == "" then
		return SERVICE_DUB
	end
	return dub
end

-- episode_num maps an anchor label onto the episode number (the
-- compiled animemobiEpisodeNum semantics): «Серия NN»/«Фильм NN» carry
-- zero-padded numbers normalized to canonical decimals, any other
-- label («Смотреть» — the whole-season anchor) counts as episode 1.
local function episode_num(label)
	local n = string.match(label, "^Серия%s+(%d+)") or string.match(label, "^Фильм%s+(%d+)")
	if n == nil then
		return "1"
	end
	return tostring(tonumber(n))
end

-- anime_section reports whether a release URL belongs to the two
-- playable anime sections; everything else (site news, manga reader
-- pages, AMVs, covers, top-lists) drops.
local function anime_section(u)
	return string.find(u, "/anime-rus/", 1, true) ~= nil
		or string.find(u, "/anime-sub/", 1, true) ~= nil
end

return {
	id = "animemobi",
	name = "AnimeMobi",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	-- smoke_query: the shared RU probe («черная лагуна») MISSES the
	-- catalog — the site titles inflect («Пираты «Черной лагуны»») and
	-- DLE's word-prefix search never matches «лагуна» against «лагуны»
	-- (live-verified 2026-09-23: 24 menu cards, zero shortstory rows).
	-- The declared probe surfaces the current Boruto release first.
	smoke_query = "боруто",
	-- No name_preference declaration (PR42 semantics): the DLE index
	-- matches the Cyrillic fragments of the composite site titles
	-- («наруто» verified live 2026-09-23), so the provider stays in
	-- the RU group (anilibria-torrent precedent).

	search = function(query)
		local form = "do=search&subaction=search&story=" .. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. "/", form, "application/x-www-form-urlencoded")

		local doc = anicli.html.parse(resp.body)
		local results, seen = {}, {}
		-- Both skins' row containers, one traversal: a row without a
		-- result heading anchor (the desktop skin's form div.base)
		-- contributes nothing on its own; the anchor union matches the
		-- compiled selector verbatim.
		doc:find("div.shortstory, div.base"):each(function(_, row)
			row:find("h2.title a[href], div.bheading h1.heading a[href]"):each(function(_, link)
				local href = link:attr("href")
				if href == "" or not anime_section(href) or seen[href] then
					return
				end
				seen[href] = true
				local poster = ""
				local img = row:find("img[src*='/uploads/']")
				if img:len() > 0 then
					poster = abs(img:attr("src"))
				end
				results[#results + 1] = {
					title = trim(link:text()),
					url = href,
					poster = poster,
				}
			end)
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		-- The dub credit rides the RAW page, not the parsed tree (the
		-- compiled provider regexed the response body verbatim).
		local dub = dub_name(resp.body)

		local doc = anicli.html.parse(resp.body)
		local order, by_num = {}, {}
		doc:find("a.onlinevideo[href]"):each(function(_, anchor)
			local ref = anchor:attr("href")
			if ref == "" then
				return
			end
			local label = trim(anchor:text())
			local num = episode_num(label)
			local g = by_num[num]
			if not g then
				g = { title = label, ref = ref }
				by_num[num] = g
				order[#order + 1] = num
			end
			-- A repeated episode number keeps its first label and
			-- takes the last anchor's embed (the compiled overwrite
			-- rule).
			g.ref = ref
		end)

		if #order == 0 then
			anicli.fail("not_found", "no onlinevideo anchors on page " .. tostring(anime_url))
		end

		local episodes = {}
		for _, num in ipairs(order) do
			local g = by_num[num]
			episodes[#episodes + 1] = {
				num = num,
				title = g.title,
				raw_id = anicli.json.encode({ n = num, d = dub, r = g.ref }),
				raw_embeds = { [dub] = { g.ref } },
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local state = anicli.json.decode(raw_id)
		local num = tostring(state.n)

		-- The credited dub rides the state; a dub the episode does not
		-- carry is the compiled RawEmbeds miss (a caller bug or a stale
		-- dub id), typed without touching the network.
		if state.d ~= dub or state.r == nil or state.r == "" then
			anicli.fail("not_found",
				"dub \"" .. dub .. "\" has no embed references on episode " .. num)
		end

		local ok, links = pcall(anicli.extract, { state.r })
		if not ok then
			anicli.fail("extract_failed", tostring(links))
		end
		-- The URL-shape labeling (animemobiStreamType port): the kodik
		-- ladders end in .m3u8 HLS manifests, everything else plays as
		-- mp4.
		for _, src in pairs(links) do
			if string.find(src.url, ".m3u8", 1, true) then
				src.type = "m3u8"
			else
				src.type = "mp4"
			end
		end
		return { dub_name = dub, links = links }
	end,
}
