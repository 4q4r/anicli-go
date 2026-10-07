-- AniTokyo (anitokyo.tv) — the RU DLE catalog with its RalodePlayer
-- module, the PR116 Lua migration of the compiled Go provider. The
-- site is a DataLife Engine install whose player embeds one JSON blob
-- describing EVERY (dub, episode) pair on the release page, so
-- episodes and dubs hydrate from a single fetch:
--
--   - search: the DLE search form POST (do=search&subaction=search&
--     story=…) renders result cards server-side as
--     article.story.shortstory; the dedicated /hentai/ section and
--     non-release links are dropped.
--   - episodes + dubs: RalodePlayer.init(<dubs>,<meta>) — <dubs> maps
--     dub id → {name, items}; each item is one episode of that dub
--     (aname label, lssort number) whose scode iframe points at the
--     site's own /video.php?id=N&cat=K wrapper. Episodes group across
--     dubs by lssort.
--   - streams: the video.php wrapper carries exactly one player
--     iframe — kodik or sibnet — resolved through anicli.extract
--     (the shared Go extractor factory).
--
-- Known walls (typed via anicli.fail, the Go sentinels' equivalents):
--   - Announcement («Анонс») pages carry no RalodePlayer data at all
--     → not_found (the site renders those releases playerless).
--   - A wrapper page without its player iframe → extract_failed.
--
-- DUB-MISS SEMANTICS (the #159 doctrine port — the animevib
-- fix-round-3 semantics, one format across the state-carrying
-- scripts): streams() NEVER substitutes another dub. When the
-- requested dub is absent from the episode's row (dropped from the
-- blob, or its per-episode coverage ends before this episode), the
-- resolve walls typed not_found whose message LISTS the dubs the
-- episode ACTUALLY carries — the row.refs keys, byte-sorted (a Lua
-- map has no order; the sort is the deterministic canonical form the
-- TUI dub menu and headless re-asks consume). The stable marker is
-- `carries no dub "X" (episode dubs: A, B, …)` — the tui
-- dubNotCarriedFailure predicate keys on the class + marker pair, so
-- every sibling's miss opens the ask-first flow. An episode the page
-- does not list at all → not_found zero-dubs wall (the same marker,
-- no list). A malformed raw_id — the merged "prov:{json}" bytes a
-- caller composing the convention without decomposing it hands over
-- (#157), a stale history record, any non-{n,u} shape — is the typed
-- invalid_input wall, never the raw json.decode VM error.
--
-- State passing (the fresh-sandbox contract): streams(raw_id, dub)
-- receives strings only, so raw_id encodes {n = num, u = page-url}
-- as JSON and the release page is re-fetched and re-parsed at resolve
-- time (+1 network fetch, network-bound — the mangal
-- output-feeds-next-stage pattern with the URL as the carrier).

local base_url = "https://anitokyo.tv"

local PLAYABLE_SECTIONS = { "/anime/", "/ongoing/", "/ova/", "/movie/" }

local EMBED_RE = '<iframe[^>]+src="([^"]+)"'
local SCODE_RE = 'src="([^"]+)"'

-- abs absolutizes protocol-relative and site-relative URLs
-- (animemobiAbsURL semantics: https: for "//", pass http through,
-- site-relative gains the base).
local function abs(u)
	if u == nil or u == "" then
		return ""
	end
	if string.sub(u, 1, 2) == "//" then
		return "https:" .. u
	end
	if string.sub(u, 1, 4) == "http" then
		return u
	end
	return base_url .. u
end

-- playable_section reports whether a release URL belongs to one of
-- the catalog's playable sections.
local function playable_section(u)
	for _, sec in ipairs(PLAYABLE_SECTIONS) do
		if string.find(u, sec, 1, true) then
			return true
		end
	end
	return false
end

-- json_from extracts the FIRST balanced JSON value starting at/after
-- pos (a depth-tracking scanner, string-aware: Lua's json.decode
-- cannot stop at the end of one value the way Go's json.Decoder
-- does). Returns the JSON text or nil.
local function json_from(s, pos)
	local b = string.find(s, "{", pos, true)
	if not b then
		return nil
	end
	local depth = 0
	local in_str = false
	local esc = false
	for i = b, #s do
		local c = string.sub(s, i, i)
		if in_str then
			if esc then
				esc = false
			elseif c == "\\" then
				esc = true
			elseif c == '"' then
				in_str = false
			end
		else
			if c == '"' then
				in_str = true
			elseif c == "{" then
				depth = depth + 1
			elseif c == "}" then
				depth = depth - 1
				if depth == 0 then
					return string.sub(s, b, i)
				end
			end
		end
	end
	return nil
end

-- all_digits/numeric sort: the episode numbers order numerically when
-- every value is numeric (the site numbers its items ascending),
-- lexicographically otherwise (amdAllDigits/amdSortNumeric parity).
local function all_digits(t)
	if #t == 0 then
		return false
	end
	for _, v in ipairs(t) do
		if not string.match(v, "^%d+$") then
			return false
		end
	end
	return true
end

local function sort_nums(nums)
	if all_digits(nums) then
		table.sort(nums, function(a, b)
			return tonumber(a) < tonumber(b)
		end)
	else
		table.sort(nums)
	end
end

-- build_rows parses the RalodePlayer.init(<dubs>,…) first argument
-- and groups the items across dubs by lssort. Returns rows (num →
-- {title, refs = {dub = {wrapper-url}}}) and the num list. A page
-- without the init call returns nil (the caller types it).
local function build_rows(body)
	local idx = string.find(body, "RalodePlayer.init(", 1, true)
	if not idx then
		return nil
	end
	local jsontext = json_from(body, idx + #"RalodePlayer.init(")
	if not jsontext then
		anicli.fail("extract_failed", "decode RalodePlayer blob: value not terminated")
	end
	local ok, dubmap = pcall(anicli.json.decode, jsontext)
	if not ok or type(dubmap) ~= "table" then
		anicli.fail("extract_failed", "decode RalodePlayer blob: invalid json")
	end

	local rows, nums = {}, {}
	for _, dub in pairs(dubmap) do
		-- a registered-but-empty dub slot contributes nothing (the
		-- Go %S trim check)
		if type(dub) == "table" and dub.name and string.match(dub.name, "%S") and dub.items then
			for _, item in pairs(dub.items) do
				if type(item) == "table" then
					local num = item.lssort and tostring(item.lssort)
					local scode = item.scode
					local m = scode and string.match(scode, SCODE_RE)
					if num ~= "" and m and m ~= "" then
						local row = rows[num]
						if not row then
							row = { title = item.aname or "", refs = {} }
							rows[num] = row
							nums[#nums + 1] = num
						end
						row.refs[dub.name] = { abs(m) }
					end
				end
			end
		end
	end
	return rows, nums
end

-- require_rows parses and types the empty case: an episode table the
-- page does not carry is the not-found wall, never a silent empty.
local function require_rows(body, page_url)
	local rows, nums = build_rows(body)
	if not rows then
		anicli.fail("not_found", "no RalodePlayer player data on page")
	end
	if #nums == 0 then
		anicli.fail("not_found", "RalodePlayer blob carries no episodes on page " .. page_url)
	end
	sort_nums(nums)
	return rows, nums
end

return {
	id = "anitokyo",
	name = "AniTokyo",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "дандадан",

	search = function(query)
		local form = "do=search&subaction=search&story=" .. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. "/", form, "application/x-www-form-urlencoded")

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find("article.story.shortstory"):each(function(_, card)
			local link = card:find("h2.story-title a[href]")
			if link:len() == 0 then
				return
			end
			local href = link:attr("href")
			if not playable_section(href) then
				return
			end
			local poster = ""
			local img = card:find(".story-poster img[src]")
			if img:len() > 0 then
				poster = abs(img:attr("src"))
			end
			results[#results + 1] = {
				title = link:text(),
				url = href,
				poster = poster,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local rows, nums = require_rows(resp.body, anime_url)

		-- raw_id carries the (episode, page) state the fresh-sandbox
		-- streams() call re-derives from.
		local episodes = {}
		for _, num in ipairs(nums) do
			local row = rows[num]
			local embeds = {}
			for dub, urls in pairs(row.refs) do
				embeds[dub] = urls
			end
			episodes[#episodes + 1] = {
				num = num,
				title = row.title,
				raw_id = anicli.json.encode({ n = num, u = anime_url }),
				raw_embeds = embeds,
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- The state guard: raw_id is ALWAYS this script's own {n, u}
		-- json (the fresh-sandbox contract); any other byte sequence —
		-- the merged prov:id convention composed without decomposing
		-- it (#157), a stale history record — surfaces typed
		-- invalid_input, never the raw json.decode VM error (the
		-- animevib guard).
		local ok, state = pcall(anicli.json.decode, raw_id)
		if not ok or type(state) ~= "table" or type(state.u) ~= "string" or state.u == "" then
			anicli.fail("invalid_input",
				"episode raw_id is not the {n, u} state json: \"" ..
				string.sub(tostring(raw_id), 1, 64) .. "\"")
		end
		local num, page = tostring(state.n), state.u

		local resp = anicli.http.get(page)
		local rows = require_rows(resp.body, page)
		local row = rows[num]
		if not row or next(row.refs) == nil then
			-- the ONLY dub wall with no ask behind it: the page does
			-- not list this episode under any dub — a data-shape fact,
			-- not a caller mistake.
			anicli.fail("not_found", "episode " .. num .. " carries no dub on the release page")
		end
		if not row.refs[dub] then
			-- DUB MISS (the #159 doctrine, see the header): never
			-- substitute another dub silently — list what the episode
			-- ACTUALLY carries (the row's own dub table, no extra
			-- fetches) in the byte-sorted order.
			local carriers = {}
			for name in pairs(row.refs) do
				carriers[#carriers + 1] = name
			end
			table.sort(carriers)
			anicli.fail("not_found", "episode " .. num .. ' carries no dub "' .. dub ..
				'" (episode dubs: ' .. table.concat(carriers, ", ") .. ')')
		end

		local embeds = {}
		for _, ref in ipairs(row.refs[dub]) do
			local wrapper = anicli.http.get(ref)
			local m = anicli.regexp.match(EMBED_RE, wrapper.body)
			if not m then
				anicli.fail("extract_failed", "video.php wrapper has no player iframe (" .. ref .. ")")
			end
			embeds[#embeds + 1] = abs(m[2])
		end

		local links = anicli.extract(embeds)
		-- the URL-shape labeling (HLS manifests play as m3u8, the
		-- kodik/sibnet mp4 files as mp4)
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
