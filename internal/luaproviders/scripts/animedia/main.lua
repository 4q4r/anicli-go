-- AniMedia (amd.online) — the RU DLE catalog behind DDoS-Guard, the
-- PR116 Lua migration of the compiled Go provider. The historical
-- animedia.online JSON v3 API is dead; the site lives on amd.online
-- as a DataLife Engine install (plain client requests pass). The
-- observed DLE shapes:
--
--   - search: the DLE search form POST (do=search&subaction=search&
--     story=…). A GET with the same params returns the site chrome
--     with a recommendation feed instead of results — the form method
--     is load-bearing. Result cards are the .poster cards inside
--     #dle-content (the header recommendation widgets rendered before
--     #dle-content reuse the same template and drop out on that
--     scoping).
--   - episodes + dubs: the anime page carries the player twice (PWA
--     and desktop blocks). Every dub — including the is-active
--     default — is an amd-kodik-voice button carrying a per-dub kodik
--     embed URL; episode numbers come from the nav_video_links
--     anchors (data-vid, repeated across blocks — deduped). A kodik
--     embed switches episodes purely by its episode query parameter,
--     so the one page fetch yields every (episode, dub) embed pair by
--     substituting the anchor's data-vid into each dub's src.
--   - streams: the kodik embeds resolve through anicli.extract (the
--     shared extractor factory; kodik first in the Python
--     registration order — live-probed 2026-09-18: 360/480/720 mp4
--     for a Steins;Gate embed).
--
-- Known walls, typed per the no-silent-failure policy:
--   - Titles served through the frame_video_mod player on other hosts
--     (rutube for id 704, aser.pro for the «Врата Штейна 0» archive
--     page) carry no kodik data at all → extract_failed naming the
--     serving host.
--   - The desktop <video-player data-aggregator="mali"> element is
--     hydrated client-side from the amedia.so app platform: empty
--     server-side, not part of the anonymous surface.
--   - Airing titles list anchors for episodes kodik has no material
--     for yet; such embeds fail at resolve time with the extractor's
--     typed error — the site's own player shows the same episode as
--     unplayable.
--
-- State passing: streams(raw_id, dub) receives the {n, u} JSON (the
-- fresh-sandbox contract) and re-fetches the release page to rebuild
-- the dub table before the substitution.

local base_url = "https://amd.online"

-- SERVICE_DUB is the dub name for titles with exactly one unnamed
-- kodik source (movies: a single /video/ iframe, no voice buttons).
local SERVICE_DUB = "AniMedia"

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

local function trim(s)
	return (string.match(s, "^%s*(.-)%s*$"))
end

-- host extracts the host of a player embed src for diagnostics
-- ("unknown" when unparseable).
local function host(src)
	local u = src
	if string.sub(u, 1, 2) == "//" then
		u = "https:" .. u
	end
	return string.match(u, "^%a+://([^/?:#]+)") or "unknown"
end

-- substitute_episode rewrites the episode query parameter of a kodik
-- embed to num (amdSubstituteEpisode semantics): a non-numeric num, a
-- src without a query, a src without an episode param, and a src
-- already carrying exactly the requested episode are all returned
-- verbatim; the substitution only ever performs a real change, never
-- a re-serialization, and preserves the remaining params in order.
local function substitute_episode(src, num)
	if not string.match(num, "^%d+$") then
		return src
	end
	local base, query = string.match(src, "^([^?]*)%?(.*)$")
	if not query then
		return src
	end
	local out, found = {}, false
	for pair in string.gmatch(query, "[^&]+") do
		local k, v = string.match(pair, "^([^=]*)=(.*)$")
		if k == "episode" then
			if v == num then
				return src
			end
			found = true
			out[#out + 1] = "episode=" .. num
		else
			out[#out + 1] = pair
		end
	end
	if not found then
		return src
	end
	return base .. "?" .. table.concat(out, "&")
end

-- all_digits/sort: the nav anchors render in ascending episode order;
-- the values order numerically when every value is numeric.
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

-- parse_dubs rebuilds the dub table from the one document: the voice
-- buttons in document order, deduped by the data-voice id (the PWA
-- and desktop blocks repeat the same set); titles with a single
-- unnamed kodik source (movies) render one iframe and no buttons —
-- the service dub carries it. Returns nil when no kodik data exists
-- anywhere (the caller types the unsupported-player wall).
local function parse_dubs(doc)
	local dubs, seen = {}, {}
	doc:find("button.amd-kodik-voice[data-voice][data-kodik-src]"):each(function(_, btn)
		local voice = btn:attr("data-voice")
		if seen[voice] then
			return
		end
		seen[voice] = true
		local name = trim(btn:text())
		local src = btn:attr("data-kodik-src")
		if name == "" or src == "" then
			return
		end
		dubs[#dubs + 1] = { name = name, src = abs(src) }
	end)

	if #dubs == 0 then
		doc:find("iframe.amd-kodik-iframe[data-src]"):each(function(_, frame)
			if #dubs > 0 then
				return
			end
			local src = frame:attr("data-src")
			if trim(src) == "" then
				return -- an aired-out episode shell; keep looking
			end
			dubs[#dubs + 1] = { name = SERVICE_DUB, src = abs(src) }
		end)
	end

	if #dubs == 0 then
		return nil, host(doc:find("iframe.frame_video_mod[src]"):attr("src"))
	end
	return dubs
end

-- unsupported_player types the no-kodik wall: the title is served
-- through a player this provider cannot resolve anonymously — an
-- empty list would fake a healthy title with no episodes.
local function unsupported_player(player)
	anicli.fail("extract_failed", "amd.online serves this title through the unsupported \"" ..
		player .. "\" player; no anonymous kodik data on the page")
end

-- episode_nums collects the nav anchor data-vid values, deduped
-- across the repeated blocks; no anchors at all means a
-- single-embed title (movie): one episode numbered 1.
local function episode_nums(doc)
	local nums, seen = {}, {}
	doc:find("a.nav_video_links[data-vid]"):each(function(_, anchor)
		local vid = anchor:attr("data-vid")
		if vid ~= "" and not seen[vid] then
			seen[vid] = true
			nums[#nums + 1] = vid
		end
	end)
	if #nums == 0 then
		nums = { "1" }
	end
	sort_nums(nums)
	return nums
end

return {
	id = "animedia",
	name = "AniMedia",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "врата штейна",

	search = function(query)
		local form = "do=search&subaction=search&story=" .. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. "/", form, "application/x-www-form-urlencoded")

		local doc = anicli.html.parse(resp.body)
		local results = {}
		-- Result cards are the .poster cards INSIDE #dle-content —
		-- never the header recommendation widgets rendered before it.
		doc:find("#dle-content"):find(".poster"):each(function(_, card)
			local link = card:find("a.poster__link[href]")
			local title = card:find("h3.poster__title"):text()
			if link:len() == 0 or title == "" then
				return
			end
			local href = link:attr("href")
			if href == "" then
				return
			end
			local poster = ""
			local img = card:find(".poster__img img[src]")
			if img:len() > 0 then
				poster = abs(img:attr("src"))
			end
			results[#results + 1] = { title = title, url = href, poster = poster }
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local doc = anicli.html.parse(resp.body)

		local dubs, player = parse_dubs(doc)
		if not dubs then
			unsupported_player(player)
		end

		local episodes = {}
		for _, num in ipairs(episode_nums(doc)) do
			local embeds = {}
			for _, dub in ipairs(dubs) do
				embeds[dub.name] = { substitute_episode(dub.src, num) }
			end
			episodes[#episodes + 1] = {
				num = num,
				raw_id = anicli.json.encode({ n = num, u = anime_url }),
				raw_embeds = embeds,
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local state = anicli.json.decode(raw_id)
		local num, page = tostring(state.n), state.u

		local resp = anicli.http.get(page)
		local doc = anicli.html.parse(resp.body)
		local dubs, player = parse_dubs(doc)
		if not dubs then
			unsupported_player(player)
		end

		local embed
		for _, d in ipairs(dubs) do
			if d.name == dub then
				embed = substitute_episode(d.src, num)
				break
			end
		end
		if not embed then
			-- a dub the episode does not carry is a caller bug (the
			-- typed ErrInvalidInput semantics)
			anicli.fail("invalid_input", "episode " .. num .. " carries no dub \"" .. dub .. "\"")
		end

		local ok, links = pcall(anicli.extract, { embed })
		if not ok then
			anicli.fail("extract_failed", tostring(links))
		end
		return { dub_name = dub, links = links }
	end,
}
