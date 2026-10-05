-- AnimeGo (animego.me) — the PR124 Lua migration of the compiled Go
-- provider. The animego.org/.one original is dead; animego.me is the
-- live continuation (same branding, same /anime/{slug}-{id} scheme,
-- kodik+aniboom player ecosystem). The site is a Turbo/Stimulus
-- frontend whose data paths are HTML scraping of /search/anime, a
-- JSON-wrapped HTML fragment at /player/{animeID} (the episode
-- carousel plus the first episode's provider buttons) and the same
-- envelope at /player/videos/{episodeID} (one episode's provider
-- buttons):
--
--   - search: GET /search/anime?q=… (the legacy path still serves the
--     full results page). Items are .ani-grid__item blocks; the title
--     link is .ani-grid__item-title a[title], the poster the
--     .ani-grid__item-picture img[src]. The site emits RELATIVE hrefs;
--     they are absolutized against the base so episodes() receives a
--     fetchable URL. Items without a title link are skipped.
--   - episodes: the anime page's data-anime-player-loader-url-value
--     names the /player/{id} fragment. Series pages yield one episode
--     per carousel item ([data-episode-number] carrying data-episode);
--     the same fragment's provider buttons describe episode one's
--     streams, whose dub keys the PR44 tier-1 distributes release-wide
--     (episode one keeps its real links, the rest carry the keys with
--     EMPTY lists — streams resolve on demand). Pages without a
--     carousel are films: one episode whose embeds are parsed inline
--     from the same fragment.
--   - streams: the always-fresh /player/videos/{raw_id} hydration —
--     the fresh-sandbox state contract turns the Go self-hydration
--     into an unconditional refetch (the anilib/animedia precedent).
--     A dub the hydration does not name resolves to an empty stream,
--     never an error; the named dub's embed URLs run through
--     anicli.extract (the shared Go extractor factory) and a resolve
--     that yields nothing anywhere raises the typed extract failure.

local base_url = "https://animego.me"

-- The site-wide request headers (the compiled provider's set): the
-- XHR marker and the RU language tag ride every request, the Referer
-- tracks the base (the harness-rewritten literal — no second copy of
-- the production domain may appear in this file).
local HEADERS = {
	["Referer"] = base_url,
	["X-Requested-With"] = "XMLHttpRequest",
	["Accept-Language"] = "ru-RU",
}

-- site_get rides the header set on every request (per-request headers
-- win over the netclient's own defaults).
local function site_get(url)
	return anicli.http.get(url, { headers = HEADERS })
end

-- abs absolutizes a site-relative href against the base
-- (absoluteURL parity: protocol-relative gains https, absolute URLs
-- pass through, hrefs without a leading slash stay as-is,
-- site-relative gain the base).
local function abs(u)
	if u == nil or u == "" then
		return ""
	end
	if string.sub(u, 1, 2) == "//" then
		return "https:" .. u
	end
	if string.sub(u, 1, 7) == "http://" or string.sub(u, 1, 8) == "https://" then
		return u
	end
	if string.sub(u, 1, 1) ~= "/" then
		return u
	end
	return base_url .. u
end

-- path_id extracts the trailing numeric id of a site-relative path
-- ("/player/2115" → "2115"); a non-numeric or absent tail is "".
local function path_id(path)
	return string.match(path, "/(%d*)$") or ""
end

-- fetch_player_fragment GETs a /player endpoint and returns its
-- data.content HTML. Transport failures raise (the SDK's typed
-- markers re-attach their sentinels); a body that will not decode is
-- the typed failure the Go decode error pinned. A decodable envelope
-- without a content payload yields the empty fragment (the Go zero
-- value), never an error.
local function fetch_player_fragment(url)
	local resp = site_get(url)
	local ok, data = pcall(anicli.json.decode, resp.body)
	if not ok or type(data) ~= "table" then
		anicli.fail("extract_failed", "decode player response: invalid json")
	end
	local payload = data.data
	if type(payload) ~= "table" or payload.content == nil then
		return ""
	end
	return payload.content
end

-- parse_embeds builds the raw_embeds map from a player fragment:
-- provider buttons (button[data-anime-player-target="provider"])
-- carry the embed URL in data-player and the dubbing studio in
-- data-translation-title (missing titles fall back to "Unknown").
local function parse_embeds(fragment)
	local embeds = {}
	fragment:find('button[data-anime-player-target="provider"]'):each(function(_, item)
		local player_url = item:attr("data-player")
		if player_url == "" then
			return
		end
		if string.sub(player_url, 1, 2) == "//" then
			player_url = "https:" .. player_url
		end
		local dub = item:attr("data-translation-title")
		if dub == "" then
			dub = "Unknown"
		end
		local list = embeds[dub]
		if list == nil then
			list = {}
			embeds[dub] = list
		end
		list[#list + 1] = player_url
	end)
	return embeds
end

return {
	id = "animego",
	name = "AnimeGo",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "черная лагуна",

	search = function(query)
		local resp = site_get(base_url .. "/search/anime?q=" .. anicli.http.query_escape(query))
		local doc = anicli.html.parse(resp.body)

		local results = {}
		doc:find(".ani-grid__item"):each(function(_, item)
			local title_node = item:find(".ani-grid__item-title a[title]")
			if title_node:len() == 0 then
				return
			end
			local poster = ""
			local thumb = item:find(".ani-grid__item-picture img[src]")
			if thumb:len() > 0 then
				poster = thumb:attr("src")
			end
			results[#results + 1] = {
				title = title_node:attr("title"),
				url = abs(title_node:attr("href")),
				poster = poster,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = site_get(anime_url)
		local doc = anicli.html.parse(resp.body)

		local loader = doc:find("[data-anime-player-loader-url-value]")
		if loader:len() == 0 then
			return {}
		end
		local loader_path = loader:attr("data-anime-player-loader-url-value")
		local anime_id = path_id(loader_path)
		if anime_id == "" then
			return {}
		end

		local fragment = anicli.html.parse(fetch_player_fragment(base_url .. loader_path))

		local episodes = {}
		fragment:find("[data-episode-number][data-episode]"):each(function(_, item)
			local num = item:attr("data-episode-number")
			local ep_id = item:attr("data-episode")
			if num == "" or ep_id == "" then
				return
			end
			episodes[#episodes + 1] = {
				num = num,
				raw_id = ep_id,
				raw_embeds = {},
			}
		end)

		if #episodes > 0 then
			-- Episode one's provider buttons ride the SAME fragment
			-- (the PR44 tier-1 fetch is free here); the remaining
			-- episodes carry the dub keys with EMPTY lists.
			episodes[1].raw_embeds = parse_embeds(fragment)
			for i = 2, #episodes do
				for dub in pairs(episodes[1].raw_embeds) do
					if episodes[i].raw_embeds[dub] == nil then
						episodes[i].raw_embeds[dub] = {}
					end
				end
			end
			return episodes
		end

		-- Film path: no carousel — the fragment's provider buttons ARE
		-- the film's embeds.
		return { {
			num = "1",
			title = "Фильм",
			raw_id = anime_id,
			raw_embeds = parse_embeds(fragment),
		} }
	end,

	streams = function(raw_id, dub)
		-- The always-fresh hydration: the episode's provider buttons
		-- are re-fetched from the bare raw_id (cached embeds are never
		-- trusted — the fresh-sandbox contract makes the Go
		-- self-hydration structural).
		local fragment = anicli.html.parse(
			fetch_player_fragment(base_url .. "/player/videos/" .. tostring(raw_id)))
		local embeds = parse_embeds(fragment)

		local links = embeds[dub]
		if links == nil or #links == 0 then
			-- A dub the hydration does not name resolves to an empty
			-- stream, never an error (ResolveStream parity).
			return { dub_name = dub, links = {} }
		end

		-- The extractor merge rule: a link whose extraction fails
		-- surfaces only while nothing resolved anywhere (the factory's
		-- total-failure rule), retyped through the provider taxonomy.
		local ok, sources = pcall(anicli.extract, links)
		if not ok then
			anicli.fail("extract_failed", tostring(sources))
		end
		return { dub_name = dub, links = sources }
	end,
}
