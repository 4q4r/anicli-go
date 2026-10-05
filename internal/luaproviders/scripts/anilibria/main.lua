-- AniLibria (aniliberty.top) — the PR120 Lua migration of the
-- compiled Go provider (itself the PR37 rebase of the anicli-py
-- anilibria.py port onto the third domain generation's Laravel API;
-- the old anilibria.top /api/v3 routes are dead). The API host is the
-- site host here: every request path hangs off the /api/v1 prefix of
-- the ONE base_url literal below.
--
--   - search: GET /api/v1/app/search/releases?query=<url-encoded>
--     (the query encoding is the Go port's task ruling; the Python
--     original interpolated it raw). A JSON answer that is not an
--     array (an error-object body) is the typed extract wall — the
--     compiled provider's slice decode rejected the same shapes.
--   - episodes: GET /api/v1/anime/releases/<alias>; episodes carry
--     UUID ids and numeric ordinals; quality links hang off the
--     hls_480/hls_720/hls_1080 fields. The API tiers content per
--     requester IP (and per auth): null/blank tiers are dropped, the
--     ceiling is 1080p BY DESIGN (the v1 OpenAPI schema defines only
--     480/720/1080; 4K exists as torrent releases served by the
--     separate anilibria-torrent provider).
--   - streams: the fresh-sandbox contract passes strings only, so the
--     per-episode quality payload rides raw_id as JSON (the anitokyo
--     {n,u} state-carrier precedent; the compiled provider stashed
--     the same payload behind the hls_json: embed convention — the
--     convention survives on raw_embeds, where the dub catalog lives).
--     Non-http URLs gain an "https:" prefix (the Python string
--     concatenation, bare-path quirk included), and every source
--     carries the site Referer the CDN gates playback on.
--
-- Known walls (typed via anicli.fail / the transport markers):
--   - a non-array search body (error envelope) → extract_failed;
--   - HTML instead of JSON → the http.get_json decode raise;
--   - 403 / timeouts → the netclient sentinels, re-attached by the
--     adapter exactly like the compiled providers'.
--
-- Documented divergences from the compiled provider (the Lua engine's
-- stricter semantics): a missing alias skips the search row (the
-- compiled provider appended an empty-URL row); a null ordinal is the
-- adapter's typed validation error (the compiled provider emitted an
-- empty-string number); a JSON null inside the episodes array stops
-- the listing there (Lua flattens nulls, ipairs halts — the compiled
-- provider emitted a zero-value entry).

local base_url = "https://aniliberty.top"
local api = base_url .. "/api/v1"

local DUB = "AniLibria"
local QUALITIES = { "1080", "720", "480" }

-- quality_links folds one release-detail episode into its
-- quality→url table, dropping the null/blank tiers (the
-- per-requester tiering; the compiled provider's empty-check).
local function quality_links(ep)
	local links = {}
	for _, q in ipairs(QUALITIES) do
		local u = ep["hls_" .. q]
		if type(u) == "string" and string.match(u, "%S") then
			links[q] = u
		end
	end
	return links
end

return {
	id = "anilibria",
	name = "AniLibria",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",

	search = function(query)
		local items = anicli.http.get_json(api .. "/app/search/releases?query=" .. anicli.http.query_escape(query))

		-- The search endpoint answers with an ARRAY of releases; an
		-- error envelope decodes to a keyed table with no [1] — the
		-- typed wall the compiled slice-decode raised instead.
		if type(items) ~= "table" or (items[1] == nil and next(items) ~= nil) then
			anicli.fail("extract_failed", "search response is not a release array")
		end

		local results = {}
		for _, item in ipairs(items) do
			if type(item) == "table" and type(item.alias) == "string" and item.alias ~= "" then
				local title = ""
				if type(item.name) == "table" and type(item.name.main) == "string" then
					title = item.name.main
				end
				if title == "" then
					-- Python: item.get("name", {}).get("main", "Unknown").
					title = "Unknown"
				end
				results[#results + 1] = {
					title = title,
					url = item.alias,
					meta = { id = item.id },
				}
			end
		end
		return results
	end,

	episodes = function(anime_url)
		local release = anicli.http.get_json(api .. "/anime/releases/" .. anime_url)
		local eps = {}
		if type(release) == "table" and type(release.episodes) == "table" then
			eps = release.episodes
		end

		local episodes = {}
		for _, ep in ipairs(eps) do
			if type(ep) == "table" then
				local links = quality_links(ep)
				local payload = anicli.json.encode(links)
				episodes[#episodes + 1] = {
					num = ep.ordinal,
					-- the fresh-sandbox streams() state carrier
					raw_id = payload,
					raw_embeds = { [DUB] = { "hls_json:" .. payload } },
				}
			end
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- No payload routes under a foreign dub id: the compiled
		-- provider looked the dub up in the (single-key) embed map and
		-- answered an empty stream.
		if dub ~= DUB then
			return { dub_name = dub, links = {} }
		end

		local links = anicli.json.decode(raw_id)
		local out = {}
		for q, u in pairs(links) do
			if type(u) == "string" then
				if string.sub(u, 1, 4) ~= "http" then
					u = "https:" .. u
				end
				out[q] = {
					url = u,
					quality = q,
					headers = { Referer = base_url },
				}
			end
		end
		return { dub_name = dub, links = out }
	end,
}
