-- YummyAnime (site.yummyani.me / api.yani.tv) — the PR123 Lua
-- migration of the compiled Go provider (internal/providers/yummy.go,
-- PR68), itself a port of anicli-api's source/yummy_anime.py. The
-- REST API lives on api.yani.tv; the declared base_url stays
-- site.yummyani.me (the catalog root the roster renders — the domain
-- 301s to old.yummyani.me since 2026-04 but is never fetched). The
-- historical SSR mirror yummyanime.in is dead (HTTP 410,
-- live-reverified 2026-10-05): the API surface is the only live one,
-- exactly as the Go provider documented. The {"response": ...}
-- envelope and the HTTP-200 error envelope come from the upstream
-- parser module (yummy_anime_me_parser.py).
--
-- Shapes:
--   - search: GET /anime?limit=20&offset=0&q=… — a miss answers
--     HTTP 200 {"response":[]}; an error envelope (which may ride
--     HTTP 200) is the typed not_found miss. SearchResult.URL is the
--     anime_id (the videos endpoint keys on it — the id-as-URL
--     animevost contract); protocol-relative poster URLs are
--     absolutized onto https for the TUI's poster downloads.
--   - episodes: GET /anime/<id>/videos — EVERY dub's embed URL for
--     every episode arrives in ONE call, so dubs hydrate eagerly (no
--     lazy DubsHydrator). The wire mixes zero-padded and bare number
--     strings ("01".."12" then "1".."9"); the canonical int identity
--     of upstream's ordinal=int(num) merges them, first-seen order
--     kept, links append. Non-numeric numbers ("2.5", "OVA") pass
--     through verbatim. Alloha iframes are NOT skipped (the shared
--     extractor factory resolves them — the documented Go delta).
--   - streams: the fresh-sandbox state contract — streams(raw_id,
--     dub) receives only RawID, so raw_id carries the {a, n} state
--     JSON and the leg re-fetches /anime/<id>/videos and re-groups
--     before picking the dub's links (the animedia precedent). Each
--     link resolves independently: /iframeCVH.html? iframes take the
--     dedicated CDNVideoHub chain, everything else walks
--     anicli.extract (the shared Go extractor factory). Results merge
--     dict.update-style (later links overwrite); a failing link is
--     remembered and only surfaces when NOTHING resolved.
--
-- The CDNVideoHub chain (upstream Source.get_videos special case +
-- player/cdnvideohub.py): fetch the iframe page and take the module
-- script's src; fetch that JS chunk and scrape the data-publisher-id
-- / data-aggregator constants; GET the playlist with pub/aggr/anime_id
-- and pick the item whose episode and voiceStudio match the iframe's
-- query (the + in dubbing_code decodes to a space); GET video/<vkId>
-- and map the mpeg* ladder with hls/dash appended AT the max quality
-- (dash appended last — it wins the key). A studio with no playlist
-- video is the legitimate empty outcome, not an error. Every source
-- echoes the playback User-Agent: the okcdn edge ties a playback
-- session to the UA that fetched the links. The UA is pinned to the
-- netclient default (the animevost precedent — the sandbox cannot
-- read the app config); a user-overridden network.user_agent diverges
-- the echo, the one known limitation of the migration.

local site_base = "https://site.yummyani.me"
local api_base = "https://api.yani.tv"
-- cvh_base is the CDNVideoHub player API the iframeCVH iframes resolve
-- through (upstream player/parsers/cdnvideohub_parser.py).
local cvh_base = "https://plapi.cdnvideohub.com"

-- playback_ua mirrors the netclient default UA (internal/config
-- Default().Network.UserAgent) — okcdn ties playback to the
-- extraction UA, so the echo must equal what the client sends.
local playback_ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " ..
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

-- cvh_quality ports _RESOLUTION_MAPPING (anicli-api player/cdnvideohub.py)
-- — the mapping the video/<vkId> sources use.
local cvh_quality = {
	mpegTinyUrl = 144,
	mpegLowestUrl = 240,
	mpegLowUrl = 360,
	mpegMediumUrl = 480,
	mpegHighUrl = 720,
	mpegFullHdUrl = 1080,
	mpegQhdUrl = 1440,
	mpeg2kUrl = 2048,
	mpeg4kUrl = 4096,
}

-- abs absolutizes the protocol-relative URLs the API emits (posters,
-- iframe URLs: "//static.yani.tv/...") onto https. Relative paths are
-- passed through verbatim — the API never emits them (the Go
-- yummyAbsURL semantics; the TUI downloads posters and hands streams
-- to mpv, both of which need an absolute URL).
local function abs(u)
	if u == "" then
		return ""
	end
	if string.sub(u, 1, 2) == "//" then
		return "https:" .. u
	end
	return u
end

-- fetch_json GETs url and unwraps the {"response": ...} envelope. The
-- error envelope may arrive ON HTTP 200 (upstream
-- YummyAnimeApiErr200Error): it surfaces as the typed not_found miss.
local function fetch_json(url, accept)
	local resp = anicli.http.get(url, { headers = { Accept = accept } })
	local env = anicli.json.decode(resp.body)
	if type(env) ~= "table" then
		error("yummy: non-object envelope from " .. url, 0)
	end
	if env.error ~= nil and env.error ~= "" then
		anicli.fail("not_found", tostring(env.error))
	end
	return env.response
end

local function fetch_videos(anime_id)
	local response = fetch_json(api_base .. "/anime/" .. tostring(anime_id) .. "/videos", "application/json")
	local videos = type(response) == "table" and response or {}
	if #videos == 0 then
		-- A nonexistent id answers HTTP 200 {"response":[]} (live
		-- 2026-09-19): the typed miss replaces the Python silent [].
		anicli.fail("not_found", "no videos for anime " .. tostring(anime_id))
	end
	return videos
end

-- canonical_num ports the Go grouping: numeric number strings
-- canonicalize through int (Atoi→Itoa merges "01" with "1"), anything
-- Atoi would reject passes through verbatim.
local function canonical_num(num)
	if string.match(num, "^[-+]?%d+$") then
		return tostring(tonumber(num))
	end
	return num
end

-- group_videos folds the wire rows into first-seen-order groups of
-- {num, dubs}: one row is one (episode, dub, player) triple and the
-- merge appends each absolutized iframe in wire order.
local function group_videos(videos)
	local order, by_num = {}, {}
	for _, video in ipairs(videos) do
		local num = canonical_num(tostring(video.number))
		local group = by_num[num]
		if not group then
			group = { num = num, dubs = {} }
			by_num[num] = group
			order[#order + 1] = group
		end
		local dub = tostring(video.data.dubbing)
		local links = group.dubs[dub]
		if not links then
			links = {}
			group.dubs[dub] = links
		end
		links[#links + 1] = abs(tostring(video.iframe_url))
	end
	return order
end

-- decode_component ports unquote_plus: "+" decodes to a space, then
-- the %XX escapes.
local function decode_component(s)
	s = string.gsub(s, "+", " ")
	s = string.gsub(s, "%%(%x%x)", function(hex)
		return string.char(tonumber(hex, 16))
	end)
	return s
end

local function decode_query(u)
	local query = string.match(u, "%?(.*)$") or ""
	local out = {}
	for pair in string.gmatch(query, "[^&]+") do
		local key, value = string.match(pair, "^([^=]*)=(.*)$")
		if key then
			out[decode_component(key)] = decode_component(value)
		end
	end
	return out
end

-- reraise surfaces the first per-link failure under its original
-- anicli class (a transport marker keeps its sentinel; anything else
-- is the extract_failed wall) — the compiled provider remembered the
-- first error and surfaced it only when NOTHING resolved.
local function reraise(err)
	local kind = string.match(tostring(err), "^anicli:([a-z_]+):") or "extract_failed"
	anicli.fail(kind, tostring(err))
end

-- resolve_cvh ports the upstream Source.get_videos special case for
-- "/iframeCVH.html?" URLs plus video_playlist_from_vk_id. Shape
-- failures raise "extractor:cdnvideohub: ..." messages — the caller's
-- pcall remembers them exactly like the Go firstErr.
local function resolve_cvh(raw_url)
	local u = abs(raw_url)
	local base = string.match(u, "^(%a+://[^/?#]+)")
	if not base then
		error("extractor:cdnvideohub: unparseable iframe url: " .. u, 0)
	end

	-- 1. The iframe page: its single module script carries the player
	-- route chunk (attribute-order independent, same as the goquery
	-- selector the compiled provider used).
	local iframe = anicli.http.get(u)
	local doc = anicli.html.parse(iframe.body)
	local chunk = doc:find('script[type="module"][crossorigin][src]')
	local src = chunk:attr("src")
	if chunk:len() == 0 or src == "" then
		error("extractor:cdnvideohub: iframe page carries no module script", 0)
	end
	local js_url = src
	if string.sub(src, 1, 2) == "//" then
		js_url = "https:" .. src
	elseif string.sub(src, 1, 4) ~= "http" then
		js_url = base .. src
	end

	-- 2. The JS chunk: the player constants (upstream PageJsCVHParams
	-- — patterns verbatim).
	local js = anicli.http.get(js_url)
	local pub = anicli.regexp.match('"data-publisher-id":\\s?(\\d+)', js.body)
	local aggr = anicli.regexp.match('"data-aggregator":\\s?"([^"]+)"', js.body)
	if not pub or not aggr then
		error("extractor:cdnvideohub: player JS missing data-publisher-id/data-aggregator", 0)
	end

	-- 3. The iframe query carries the episode identity; dubbing_code's
	-- "+" decodes to a space so it equals the playlist's voiceStudio
	-- key exactly like upstream's unquote_plus.
	local params = decode_query(u)
	local episode_s, anime_id, dubbing_code = params.episode, params.anime_id, params.dubbing_code
	if episode_s == nil or not string.match(episode_s, "^[-+]?%d+$")
		or anime_id == nil or anime_id == ""
		or dubbing_code == nil or dubbing_code == "" then
		error("extractor:cdnvideohub: iframe query lacks anime_id/episode/dubbing_code", 0)
	end

	local playlist = anicli.http.get(
		cvh_base .. "/api/v1/player/sv/playlist?aggr=" .. anicli.http.query_escape(aggr[2])
			.. "&id=" .. anicli.http.query_escape(anime_id)
			.. "&pub=" .. anicli.http.query_escape(pub[2]),
		{ headers = { Accept = "application/json, text/plain, */*" } })
	local pl = anicli.json.decode(playlist.body)
	local vk_id = nil
	for _, item in ipairs(type(pl) == "table" and pl.items or {}) do
		if tostring(item.episode) == tostring(tonumber(episode_s)) and item.voiceStudio == dubbing_code then
			vk_id = tostring(item.vkId)
			break
		end
	end
	if not vk_id then
		-- Studio listed in the metadata but no video uploaded — the
		-- legitimate empty outcome (upstream returns []).
		return {}
	end

	-- 4. The video sources. hls/dash append AT the max quality after
	-- the mpeg* ladder (dash appended last — it wins the key; the map
	-- collapses upstream's list, the documented divergence).
	local video = anicli.http.get(cvh_base .. "/api/v1/player/sv/video/" .. vk_id,
		{ headers = { Accept = "application/json, text/plain, */*" } })
	local vid = anicli.json.decode(video.body)
	local sources = {}
	if type(vid) == "table" and type(vid.sources) == "table" then
		sources = vid.sources
	end

	local results = {}
	local max_quality = 0
	for key, link in pairs(sources) do
		if link ~= nil and link ~= "" and key ~= "hlsUrl" and key ~= "dashUrl" then
			local quality = cvh_quality[key]
			if quality then
				local qs = tostring(quality)
				results[qs] = { url = link, quality = qs, type = "mp4",
					headers = { ["User-Agent"] = playback_ua } }
				if quality > max_quality then
					max_quality = quality
				end
			end
		end
	end
	if max_quality > 0 then
		local max = tostring(max_quality)
		local hls, dash = sources.hlsUrl, sources.dashUrl
		if hls ~= nil and hls ~= "" then
			results[max] = { url = hls, quality = max, type = "m3u8",
				headers = { ["User-Agent"] = playback_ua } }
		end
		if dash ~= nil and dash ~= "" then
			results[max] = { url = dash, quality = max, type = "mpd",
				headers = { ["User-Agent"] = playback_ua } }
		end
	end
	return results
end

return {
	id = "yummy",
	name = "YummyAnime",
	base_url = site_base,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "лагуна",

	search = function(query)
		local items = fetch_json(api_base .. "/anime?limit=20&offset=0&q=" ..
			anicli.http.query_escape(query), "application/json")
		local results = {}
		for _, item in ipairs(type(items) == "table" and items or {}) do
			local poster = ""
			if type(item.poster) == "table" then
				poster = abs(tostring(item.poster.medium or ""))
			end
			results[#results + 1] = {
				title = tostring(item.title),
				url = tostring(item.anime_id),
				poster = poster,
			}
		end
		return results
	end,

	episodes = function(anime_url)
		local videos = fetch_videos(anime_url)
		local episodes = {}
		for _, group in ipairs(group_videos(videos)) do
			episodes[#episodes + 1] = {
				num = group.num,
				-- The fresh-sandbox resolve state: streams(raw_id, dub)
				-- re-fetches /anime/<a>/videos and re-picks group n.
				raw_id = anicli.json.encode({ a = tostring(anime_url), n = group.num }),
				raw_embeds = group.dubs,
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local state = anicli.json.decode(raw_id)
		if type(state) ~= "table" or state.a == nil or state.n == nil then
			anicli.fail("invalid_input", "raw_id lacks the {a, n} resolve state")
		end

		local links = nil
		for _, group in ipairs(group_videos(fetch_videos(tostring(state.a)))) do
			if group.num == tostring(state.n) then
				links = group.dubs[dub]
				break
			end
		end

		local out, first_err = {}, nil
		for _, link in ipairs(links or {}) do
			local ok, res
			if string.find(link, "/iframeCVH.html?", 1, true) then
				ok, res = pcall(resolve_cvh, link)
			else
				ok, res = pcall(anicli.extract, link)
			end
			if not ok then
				if first_err == nil then
					first_err = res
				end
			else
				for quality, source in pairs(res) do
					out[quality] = source -- dict.update: later links overwrite
				end
			end
		end
		if next(out) == nil and first_err ~= nil then
			reraise(first_err)
		end
		return { dub_name = dub, links = out }
	end,
}
