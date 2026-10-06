-- KickassAnime (kaa.lt) — the PR129 Lua migration of the compiled Go
-- provider (internal/providers/kickassanime.go, PR58, written against
-- the live site and live-verified 2026-09-18; re-verified live
-- 2026-10-06 through the configured proxy: fsearch, the show/episodes
-- API and the per-episode server lists all answer the same shapes the
-- fixtures pin). The site is a plain JSON API — anonymous on every
-- leg, no Cloudflare challenge, no gate cookie (the 2026-10-06 probe
-- answered all four endpoints 200 with zero Set-Cookie; the gate
-- the controller's plan note hypothesized does not exist on the wire).
--
-- Shapes:
--   - search: POST {base}/api/fsearch with JSON {"page":1,"query":…}
--     (the Accept header is NOT load-bearing — the live probe answers
--     byte-identically without it, so the header-less http.post is
--     honest parity). Entries loosely matched by the fuzzy index omit
--     title_en; the native title is the fallback. Result URLs are the
--     bare show slugs — the episode API keys on them directly.
--   - episodes: GET /api/show/{slug} routes tv vs movie. TV walks the
--     paginated /api/show/{slug}/episodes?ep=N&lang=ja-JP — the first
--     page rides ep=1, every follow-up page the page's FIRST episode
--     number (the pg.eps[0] hop), fanned out bounded-parallel and
--     concatenated in wire (ascending) order; a page descriptor with
--     no eps or an out-of-range number is skipped. Movies skip the
--     episode API entirely: the single showing comes from the show's
--     watch_uri tail (ep-0-…), rendered as episode 1 — the wire number
--     is meaningless for movies and must not leak. Entries with a
--     non-numeric or sub-1 episode_number are skipped (the
--     Number.isFinite guard of the Anivexa recipe).
--   - dubs: the Lua provider contract has no DubsHydrator capability
--     (the session resolves only from the listing's raw_embeds), so
--     every episode's server list is hydrated EAGERLY in one
--     bounded-parallel batch — the anikoto/animedia/yummy precedent.
--     GET /api/show/{slug}/episode/{epSlug} answers {servers:[…]};
--     each server whose src carries an ?id= query becomes a
--     constructed krussdomi master-manifest embed under the server's
--     name (shortName, then "Unknown", as fallback); dash-typed srcs
--     are skipped (their ids answer 502 on the manifest path — two
--     independent ids probed live) and id-less srcs have nothing to
--     build a manifest from. One dead hydration leg leaves that
--     episode's embeds empty — the resolve re-fetch fails loudly for
--     the episode a user actually opens (the anikoto rule).
--   - streams: GET /api/show/{slug}/episode/{epSlug} again (the fresh
--     sandbox re-derives — +1 fetch per resolve), the chosen dub's
--     first manifest returned as the single "auto" source: the
--     krussdomi master carries every video variant AND the switchable
--     audio renditions (Japanese + English dub among them), so
--     splitting variants into per-quality links would strip the audio
--     tracks. The krussdomi Referer rides on the link (the edge pins
--     playback to its origin). An unknown dub or a malformed raw_id is
--     the typed invalid_input caller-bug wall.
--
-- Parallelism: the compiled provider bounded its episode-page fan-out
-- with config network.max_parallel; the script cannot read the config
-- and get_batch clamps ≤0 to 1, so the bound (the config default, 4)
-- is pinned locally — the animevib review-F3 precedent. get_batch
-- cannot carry custom headers; the live probe answers every GET leg
-- without Accept, so the batch legs ride header-less (only the show
-- fetch, which never batches, keeps the header).
--
-- Route: the kaa.lt search answers direct, but the episode-page
-- fan-out TARPITS the direct route on the characterization network
-- (live 2026-10-06: search 2 hits, episodes legs stall to the 10s
-- budget) — the honest route is network.proxy_url (the anikoto
-- class). The per-provider netclient wiring carries the proxy; the
-- script is route-agnostic.

local base_url = "https://kaa.lt"

-- The krussdomi HLS edge: every usable server src carries the media id
-- in its ?id= query, and the id resolves onto
-- {base}/{id}/master.m3u8 [LIVE-VERIFIED 2026-09-18: the master
-- manifest answers 200 with switchable audio renditions].
local HLS_BASE = "https://hls.krussdomi.com/manifest"
local PLAYBACK_REFERER = "https://krussdomi.com/"

-- The episode API's fixed language (the dub audio rides inside the
-- master manifest; the parameter only documents the request locale).
local EPISODE_LANG = "ja-JP"

-- The fan-out bounds: the config default for network.max_parallel,
-- out of script reach — pinned, never read (see the header).
local MAX_PAGE_PARALLEL = 4
local MAX_HYDRATE_PARALLEL = 4

-- Go-regexp patterns the shared SDK regexp bridge evaluates (RE2
-- syntax — the compiled provider's patterns verbatim). The movie
-- watch_uri tail: group 2 is the full episode slug used as RawID.
local MOVIE_URI_RE = "(?i)(ep-(\\d+)-([0-9a-f]+))$"

local function trim(s)
	return (string.match(s or "", "^%s*(.-)%s*$"))
end

-- trim_slashes is the compiled strings.Trim(animeURL, "/") semantics:
-- slash characters off BOTH ends, whitespace preserved.
local function trim_slashes(s)
	s = string.gsub(s or "", "^/+", "")
	s = string.gsub(s, "/+$", "")
	return s
end

-- kaa_get rides the JSON accept header on the single-fetch legs (the
-- compiled client's base headers; the batch legs go header-less — the
-- live probe answers both shapes identically).
local function kaa_get(url)
	return anicli.http.get(url, { headers = { Accept = "application/json" } })
end

-- episode_page_url builds one page request (?ep=<start>&lang=ja-JP).
local function episode_page_url(show_slug, start_ep)
	return base_url .. "/api/show/" .. show_slug ..
		"/episodes?ep=" .. start_ep .. "&lang=" .. EPISODE_LANG
end

-- num_str renders an episode number the way the compiled
-- strconv.FormatFloat(num, 'f', -1, 64) did: whole values stay plain
-- integers ("1", "12"), fractional values print their digits.
local function num_str(num)
	if num % 1 == 0 then
		return string.format("%d", num)
	end
	return tostring(num)
end

-- page_episodes converts one wire page into episode rows. Entries
-- with a non-numeric or sub-1 episode_number, or an empty slug, are
-- skipped; a missing title decodes to the empty string (the real
-- captures carry titleless entries).
local function page_episodes(show_slug, page)
	local episodes = {}
	for _, item in ipairs(page.result or {}) do
		local num = tonumber(item.episode_number)
		local slug = item.slug or ""
		if num and num >= 1 and slug ~= "" then
			local ns = num_str(num)
			episodes[#episodes + 1] = {
				num = ns,
				title = item.title or "",
				raw_id = show_slug .. "/ep-" .. ns .. "-" .. slug,
			}
		end
	end
	return episodes
end

-- parse_servers folds a server list into the raw_embeds map: one
-- constructed master manifest per id-bearing, non-dash server. The id
-- lifts out of the ?id= / &id= query; the compiled url.Parse decoded
-- it, but the live ids are bare hex/base64url with no percent-escapes,
-- so the plain match is faithful. Same-named servers append.
local function parse_servers(data)
	local embeds = {}
	for _, server in ipairs(data.servers or {}) do
		local src = server.src or ""
		if not string.find(src, "type=dash", 1, true) then
			local id = string.match(src, "[?&]id=([^&]+)")
			if id and id ~= "" then
				local name = server.name
				if name == nil or name == "" then
					name = server.shortName
				end
				if name == nil or name == "" then
					name = "Unknown"
				end
				local list = embeds[name]
				if list == nil then
					list = {}
					embeds[name] = list
				end
				list[#list + 1] = HLS_BASE .. "/" .. id .. "/master.m3u8"
			end
		end
	end
	return embeds
end

-- hydrate_embeds fills every episode's raw_embeds eagerly (the
-- DubsHydrator delta): one bounded-parallel batch over the per-episode
-- server lists. A dead leg — transport error or a body that refuses to
-- decode — leaves that episode's embeds empty; the resolve re-fetch
-- fails loudly for the episode a user actually opens.
local function hydrate_embeds(show_slug, episodes)
	if #episodes == 0 then
		return
	end
	local urls = {}
	for _, ep in ipairs(episodes) do
		local ep_slug = string.match(ep.raw_id, "^[^/]+/(.+)$")
		urls[#urls + 1] = base_url .. "/api/show/" .. show_slug .. "/episode/" .. (ep_slug or "")
	end
	local batch = anicli.http.get_batch(urls, MAX_HYDRATE_PARALLEL)
	for i, res in ipairs(batch) do
		local embeds = {}
		if res.error == nil then
			local ok, data = pcall(anicli.json.decode, res.body)
			if ok and type(data) == "table" then
				embeds = parse_servers(data)
			end
		end
		episodes[i].raw_embeds = embeds
	end
end

return {
	id = "kickassanime",
	name = "KickassAnime",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ja",
	name_preference = "latin",
	smoke_query = "dandadan",

	search = function(query)
		local body = anicli.json.encode({ page = 1, query = query })
		local resp = anicli.http.post(base_url .. "/api/fsearch", body, "application/json")
		local data = anicli.json.decode(resp.body)

		local results = {}
		for _, item in ipairs(data.result or {}) do
			local slug = item.slug or ""
			if slug ~= "" then
				local title = item.title_en
				if title == nil or title == "" then
					title = item.title
				end
				results[#results + 1] = { title = title or "", url = slug }
			end
		end
		return results
	end,

	episodes = function(anime_url)
		local show_slug = trim_slashes(trim(anime_url))
		if show_slug == "" then
			anicli.fail("invalid_input", "anime url \"" .. (anime_url or "") .. "\" trims to an empty show slug")
		end

		local show = anicli.json.decode(kaa_get(base_url .. "/api/show/" .. show_slug).body)

		if show.type == "movie" then
			-- Movies resolve from the watch_uri tail alone; the episode
			-- API is never hit. A tail-less watch_uri is the typed
			-- not_found wall (an empty list would fake a healthy title
			-- with no episodes — the roster doctrine).
			local tail = anicli.regexp.match(MOVIE_URI_RE, show.watch_uri or "")
			if tail == nil then
				anicli.fail("not_found", "movie watch_uri \"" .. (show.watch_uri or "") ..
					"\" carries no ep tail")
			end
			local episodes = {
				{ num = "1", title = "", raw_id = show_slug .. "/" .. tail[2] },
			}
			hydrate_embeds(show_slug, episodes)
			return episodes
		end

		local first = anicli.json.decode(kaa_get(episode_page_url(show_slug, 1)).body)
		local pages = first.pages or {}

		-- batches[i] holds page i's rows; slot 1 is the already-fetched
		-- first page. Follow-up pages ride one bounded-parallel batch,
		-- each fetched with the page's first episode number; a
		-- descriptor with no eps or an out-of-range number is skipped
		-- (the compiled rules). A failed page fetch fails loud — the
		-- compiled fan-out errored the whole walk, never a silent
		-- partial list.
		local batches = { [1] = page_episodes(show_slug, first) }
		if #pages > 1 then
			local urls, slots = {}, {}
			for _, pg in ipairs(pages) do
				local number = tonumber(pg.number)
				if number and number >= 2 and number <= #pages and type(pg.eps) == "table" and #pg.eps > 0 then
					slots[number] = #urls + 1
					urls[#urls + 1] = episode_page_url(show_slug, pg.eps[1])
				end
			end
			if #urls > 0 then
				local batch = anicli.http.get_batch(urls, MAX_PAGE_PARALLEL)
				for number, idx in pairs(slots) do
					local res = batch[idx]
					if res.error then
						anicli.fail("extract_failed", "episode page " .. number .. " failed: " .. res.error)
					end
					batches[number] = page_episodes(show_slug, anicli.json.decode(res.body))
				end
			end
		end

		local episodes = {}
		for i = 1, math.max(#pages, 1) do
			for _, ep in ipairs(batches[i] or {}) do
				episodes[#episodes + 1] = ep
			end
		end

		hydrate_embeds(show_slug, episodes)
		return episodes
	end,

	streams = function(raw_id, dub)
		local show_slug, ep_slug = string.match(raw_id or "", "^([^/]+)/(.+)$")
		if show_slug == nil or show_slug == "" or ep_slug == "" then
			anicli.fail("invalid_input", "episode raw_id \"" .. (raw_id or "") ..
				"\" is not {showSlug}/{epSlug}")
		end

		local ok, data = pcall(function()
			return anicli.json.decode(
				kaa_get(base_url .. "/api/show/" .. show_slug .. "/episode/" .. ep_slug).body)
		end)
		if not ok or type(data) ~= "table" then
			anicli.fail("extract_failed", "server list of \"" .. raw_id .. "\" failed to decode")
		end

		local embeds = parse_servers(data)
		local links = embeds[dub]
		if links == nil or #links == 0 then
			anicli.fail("invalid_input", "dub \"" .. dub .. "\" carries no mirrors to resolve")
		end

		-- One server slot carries exactly one master manifest by
		-- construction; the single "auto" source keeps the switchable
		-- audio renditions (splitting per-quality would strip them).
		return {
			dub_name = dub,
			links = {
				auto = {
					url = links[1],
					quality = "auto",
					type = "m3u8",
					headers = { Referer = PLAYBACK_REFERER },
				},
			},
		}
	end,
}
