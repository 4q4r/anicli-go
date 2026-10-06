-- AniMiku (beta.animiku.tokyo) — the RU DLE catalog riding the kodik
-- player stack, the PR134 Lua migration of the compiled Go provider
-- (itself written from the live site, no frozen Python original). The
-- observed shapes:
--
--   - search: the DLE full-search GET (/index.php?do=search&
--     subaction=search&story=… — the site header form is method=get,
--     unlike the sameband POST variant) renders
--     article.news-container-chapter rows server-side; the first
--     newsid anchor of a row carries the release URL, p.title-text the
--     title, img.xfieldimage the poster (relative /uploads/ paths
--     absolutized; absolute URLs pass through). Rows without a newsid
--     link are site service pages and drop out. Anonymous, no extra
--     headers (LIVE-VERIFIED 2026-09-25, re-verified 2026-10-06:
--     HTTP 200, no challenge, junk query answers 0 rows). DLE caps
--     the form answer at one page; no pagination is attempted (the
--     yummy/anilib/anistar/animemobi single-page precedent).
--   - episodes: the mrdeath/aaparser AJAX bridge
--     (/engine/ajax/controller.php?mod=anime_grabber&module=
--     kodik_playlist_ajax) answers POST news_id=<id>&action=
--     load_player with the player HTML fragment (the same URL asked
--     with GET answers HTTP 200 and an EMPTY body — the POST form is
--     load-bearing). One answer carries the whole episode graph:
--     li.b-translator__item[data-this_translator] is the dub list
--     (kodik translation id to voice-over name), serials add the
--     li.b-simple_episode__item grid (data-this_episode times
--     data-this_translator times data-this_link protocol-relative
--     kodikplayer.com refs), movies carry the link on the translator
--     item itself (the kodik_translates_alt shape) and collapse to
--     one episode keyed "1". The dub-to-episode matrix is sparse —
--     each episode keeps only the dubs that list it; episode order is
--     first-seen; a repeated (episode, dub) pair overwrites.
--   - streams: the refs resolve through anicli.extract (the shared Go
--     extractor factory; the kodik extractor Matches the
--     kodikplayer.com host). Kodik ladders are .m3u8 HLS manifests,
--     everything else plays progressive.
--
-- State passing (the fresh-sandbox contract): streams(raw_id, dub)
-- receives strings only, so raw_id encodes {n = num, id = news-id} as
-- JSON and the bridge is re-POSTed and re-parsed at resolve time
-- (+1 network fetch, network-bound — the anitokyo {n,u} precedent).
--
-- Quality tiers (archaeology note, kept from the Go port): the
-- player switcher also lists the site's 4K/FHD tiers, but both are
-- RUNTIME resolvers (a site-hosted libplayer iframe that searches the
-- anilibria.top API by title client-side); no deterministic embed URL
-- exists, so the kodik player — the site's default and per-episode
-- source — is the stream contract here; the tiers stay documented,
-- not wired. No NamePreference or SmokeQuery declaration (PR42/PR51
-- semantics): the DLE index matches Cyrillic word prefixes with
-- е/ё equivalence, the shared RU smoke probe hits.

local base_url = "https://beta.animiku.tokyo"

-- The mrdeath/aaparser AJAX endpoint that renders the kodik player
-- for a release.
local PLAYER_BRIDGE =
	"/engine/ajax/controller.php?mod=anime_grabber&module=kodik_playlist_ajax"

-- abs absolutizes the root-relative asset URLs the markup mixes
-- freely (the /uploads/ posters): protocol-relative gains https,
-- site-relative gains the base, everything else passes through
-- (animemobiAbsURL semantics).
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

-- trim strips the surrounding whitespace the markup leaves around
-- translator labels and anchors (the Go strings.TrimSpace ports).
local function trim(s)
	return (string.gsub(s or "", "^%s*(.-)%s*$", "%1"))
end

-- fetch_player POSTs the aaparser bridge for a release id and parses
-- the player fragment. No Referer, cookies or X-Requested-With header
-- are required (live-verified 2026-09-25).
local function fetch_player(news_id)
	local resp = anicli.http.post(
		base_url .. PLAYER_BRIDGE,
		"news_id=" .. news_id .. "&action=load_player",
		"application/x-www-form-urlencoded"
	)
	return anicli.html.parse(resp.body)
end

-- dub_names maps the kodik translation ids onto voice-over names.
local function dub_names(doc)
	local dubs = {}
	doc:find("li.b-translator__item[data-this_translator]"):each(function(_, li)
		local id = trim(li:attr("data-this_translator"))
		local name = trim(li:text())
		if id ~= "" and name ~= "" then
			dubs[id] = name
		end
	end)
	return dubs
end

-- episode_groups folds a player answer onto the (episode number to
-- dub name to embed ref) graph. Serial rows group by data-this_episode
-- in first-seen order; a release without the grid collapses to one
-- episode keyed "1" carrying every per-dub translator link. Returns
-- the num order and the groups; an answer with neither shape (a news
-- page or a stripped release) is the typed not-found.
local function episode_groups(doc)
	local dubs = dub_names(doc)
	local nums, groups, grid = {}, {}, false
	doc:find("li.b-simple_episode__item[data-this_episode]"):each(function(_, li)
		grid = true
		local num = trim(li:attr("data-this_episode"))
		local ref = li:attr("data-this_link")
		if num == "" or ref == "" then
			return
		end
		local id = trim(li:attr("data-this_translator"))
		local dub = dubs[id]
		if dub == nil or dub == "" then
			-- Unknown translation id: key by the id itself — never
			-- silently drop an embed.
			if id == "" then
				id = "?"
			end
			dub = id
		end
		local g = groups[num]
		if not g then
			g = { title = trim(li:text()), refs = {} }
			groups[num] = g
			nums[#nums + 1] = num
		end
		g.refs[dub] = ref
	end)

	if not grid then
		doc:find("li.b-translator__item[data-this_link]"):each(function(_, li)
			local ref = li:attr("data-this_link")
			local name = trim(li:text())
			if ref ~= "" and name ~= "" then
				local g = groups["1"]
				if not g then
					g = { title = "", refs = {} }
					groups["1"] = g
					nums[#nums + 1] = "1"
				end
				g.refs[name] = ref
			end
		end)
	end

	if #nums == 0 then
		anicli.fail("not_found", "no episodes or dub links in the player answer")
	end
	return nums, groups
end

return {
	id = "animiku",
	name = "AniMiku",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",

	search = function(query)
		local resp = anicli.http.get(base_url ..
			"/index.php?do=search&subaction=search&story=" ..
			anicli.http.query_escape(query))

		local doc = anicli.html.parse(resp.body)
		local results, seen = {}, {}
		doc:find("article.news-container-chapter"):each(function(_, row)
			local link = row:find("a[href*='newsid=']")
			if link:len() == 0 then
				return
			end
			local href = link:attr("href")
			if href == "" or seen[href] then
				return
			end
			seen[href] = true
			local poster = ""
			local img = row:find("img.xfieldimage")
			if img:len() > 0 then
				poster = abs(img:attr("src"))
			end
			results[#results + 1] = {
				title = row:find("p.title-text"):text(),
				url = href,
				poster = poster,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		-- The release id is the newsid query param — the only release
		-- URL shape this catalog issues (search rows and category cards
		-- both link index.php?newsid=N) — so a URL without it is the
		-- typed invalid-input, and the release page itself never needs
		-- fetching.
		local news_id = string.match(anime_url, "[?&]newsid=([^&]+)")
		if news_id == nil or news_id == "" then
			anicli.fail("invalid_input", "release url carries no newsid: " .. anime_url)
		end

		local nums, groups = episode_groups(fetch_player(news_id))
		local episodes = {}
		for _, num in ipairs(nums) do
			local embeds = {}
			for dub, ref in pairs(groups[num].refs) do
				embeds[dub] = { ref }
			end
			episodes[#episodes + 1] = {
				num = num,
				title = groups[num].title,
				raw_id = anicli.json.encode({ n = num, id = news_id }),
				raw_embeds = embeds,
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local state = anicli.json.decode(raw_id)
		local num, news_id = tostring(state.n), state.id

		local nums, groups = episode_groups(fetch_player(news_id))
		local g = nil
		for _, n in ipairs(nums) do
			if n == num then
				g = groups[n]
				break
			end
		end
		local ref = g and g.refs[dub] or nil
		if ref == nil then
			anicli.fail("not_found",
				"dub \"" .. dub .. "\" has no embed references on episode " .. num)
		end

		local links = anicli.extract({ ref })
		-- The URL-shape labeling (animikuStreamType port): kodik
		-- ladders end in .m3u8 HLS manifests, everything else plays
		-- as mp4.
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
