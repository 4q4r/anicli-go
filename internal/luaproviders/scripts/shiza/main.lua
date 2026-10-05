-- SHIZA Project (shizaproject.com) — the PR125 Lua migration of the
-- compiled Go provider (internal/providers/shiza.go, PR57), itself
-- written from the live API (no frozen Python original). The site is
-- a Nuxt SPA whose runtime config exposes the real API: a GraphQL
-- endpoint on the site host itself, path /graphql (Apollo Server; the
-- REST paths /api/release answer 404, introspection is disabled in
-- production). The two query documents below are minimal projections
-- of the site bundle's compiled fetchReleases / fetchRelease
-- operations, captured verbatim with their fixtures
-- (testdata/shiza_*.json, 2026-09-18). Anonymous access is enough:
-- search, release detail, episodes and embeds all answer without
-- cookies; the service answers direct from RU networks (no
-- Cloudflare challenge observed; re-verified live 2026-10-05 —
-- «черная лагуна» still surfaces the two Black Lagoon releases).
--
-- Shapes:
--   - search: POST /graphql with the fetchReleases document and
--     {first, query} variables. The page size is 20 (the TUI
--     search fan-out renders the top hits; the catalog ordering puts
--     the best match first). The API does the matching — both the
--     Russian names and the romaji originals hit — so results pass
--     through unfiltered. Each result keeps the site's release-page
--     URL: the slug IS the API handle (release(slug:)) and the
--     episodes() leg round-trips it through that URL. An empty query
--     is a caller bug: typed invalid_input before any network I/O
--     (the Go guard).
--   - episodes: POST /graphql with the fetchRelease document. A null
--     release is the typed not_found miss [LIVE-VERIFIED 2026-09-18:
--     unknown slug answers {"data":{"release":null}}];
--     viewerInBlockedCountry is the API's own geo flag — the embeds
--     of a flagged release do not resolve — and fails typed
--     geo_blocked. Every episode video is an embed of SHIZA's
--     hosting: KODIK (kodikplayer.com, the team's dub) or SIBNET
--     (video.sibnet.ru mirror), both covered by the shared extractor
--     factory — so the embeds hydrate eagerly under the single
--     "SHIZA Project" dub key (a release is one team's work;
--     embedSource is the HOST, not a dub name). Null-number entries
--     (announcement stubs) are skipped, blank embed URLs are
--     dropped, and the API order is preserved (kodik dub first,
--     sibnet mirror second).
--   - streams: streams(raw_id, dub) resolves the embed list carried
--     IN the raw_id state JSON ({e = [...]}) through anicli.extract
--     (the shared Go extractor factory: the kodik extractor resolves
--     the team's player pages into quality-keyed sources, the sibnet
--     extractor the mirrors; the dict.update merge lets later links
--     overwrite earlier quality keys) — with NO release re-fetch.
--     This is the faithful fresh-sandbox translation of the compiled
--     provider's data flow: its ResolveStream read the episode's
--     in-memory RawEmbeds and made ZERO shiza requests per resolve.
--     The re-fetch pattern of the sibling migrations (anilib,
--     anitokyo, yummy) would translate a data flow shiza's Go
--     original never had — and operationally it is not viable here:
--     the site's front tarpits the netclient's fingerprint after the
--     3rd GraphQL POST inside about a minute (probe 2026-10-05: three
--     rapid POSTs answer in ~0.3s each, the 4th — even paced 15s
--     apart — hangs silent until the watchdog kills it; a plain curl
--     burst of six parallel POSTs never trips it), so every consumed
--     chain must fit its GraphQL budget: search + one episodes POST
--     per opened release, nothing more. The carried embeds stay
--     fresh in practice: the TUI's PR111 flows re-hydrate episodes
--     (fresh RawIDs) immediately before every resolve. An unknown
--     dub key resolves to an empty stream without any network I/O
--     (the Go RawEmbeds[dub] nil parity); a state JSON without the
--     embed list is the typed caller bug.
--
-- Torrents: the Release type also carries torrent entries (magnet
-- with the tr.shiza-project.com announces + an anonymous .torrent
-- file on cdn.shizaproject.com), but the swarm is dead — the API
-- reported 0 seeders on every sampled torrent (2026-09-18, 100
-- newest + 50 oldest of 1881; Jackett's indexer request reached the
-- same conclusion). Per the kodik-parity rule (never register a
-- provider that cannot run) no torrent sibling exists.

local base_url = "https://shizaproject.com"

local DUB = "SHIZA Project"
local RELEASE_PREFIX = "/releases/"

-- The two GraphQL documents verbatim: the fixtures were captured
-- with these exact texts (2026-09-18) and the shape tests pin the
-- same literals — a projection change that silently drifts from the
-- captured shape must fail there.
local SEARCH_QUERY = "query fetchReleases($first: Int, $query: String) { releases(first: $first, query: $query) { edges { node { slug name posters { preview: resize(width: 360, height: 500) { url } } } } } }"
local RELEASE_QUERY = "query fetchRelease($slug: String!) { release(slug: $slug) { viewerInBlockedCountry episodes { number name videos { embedUrl } } } }"

-- trim ports the Go strings.TrimSpace over the embed URLs (ASCII
-- space set; URLs never carry unicode spaces).
local function trim(s)
	return string.match(s, "^%s*(.-)%s*$")
end

-- episode_embeds collects one episode's embed list: blank embed URLs
-- are dropped, API order preserved (kodik dub first, sibnet mirror
-- second).
local function episode_embeds(ep)
	local embeds = {}
	for _, video in ipairs(ep.videos or {}) do
		local u = type(video.embedUrl) == "string" and trim(video.embedUrl) or ""
		if u ~= "" then
			embeds[#embeds + 1] = u
		end
	end
	return embeds
end

-- graphql runs one GraphQL operation: POST the {query, variables}
-- envelope with the JSON content type and return the decoded data
-- field. Transport failures raise from the SDK (the typed markers —
-- provider_403, timeout — re-attach their sentinels in the adapter);
-- an application-level errors array fails with its first message
-- (the Go parity, checked BEFORE the data presence); a body that
-- will not decode fails loud.
local function graphql(query, variables)
	local payload = anicli.json.encode({ query = query, variables = variables })
	local resp = anicli.http.post(base_url .. "/graphql", payload, "application/json")
	local ok, envelope = pcall(anicli.json.decode, resp.body)
	if not ok or type(envelope) ~= "table" then
		error("decode graphql response: invalid json", 0)
	end
	local errs = envelope.errors
	if type(errs) == "table" and #errs > 0 and type(errs[1].message) == "string" then
		error("graphql: " .. errs[1].message, 0)
	end
	if type(envelope.data) ~= "table" then
		error("graphql response carries no data", 0)
	end
	return envelope.data
end

return {
	id = "shiza",
	name = DUB,
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "черная лагуна",

	search = function(query)
		-- Empty queries are a caller bug: reject before any network
		-- I/O (the Go guard).
		if string.match(query, "%S") == nil then
			anicli.fail("invalid_input", "пустой поисковый запрос")
		end
		local data = graphql(SEARCH_QUERY, { first = 20, query = query })
		local edges = ((data.releases or {}).edges) or {}

		local results = {}
		for _, edge in ipairs(edges) do
			local node = edge.node or {}
			local slug = node.slug
			if type(slug) == "string" and slug ~= "" then
				-- not a navigable release: the slug IS the API handle
				local poster = ""
				local posters = node.posters
				if type(posters) == "table" and type(posters[1]) == "table" then
					local preview = posters[1].preview
					if type(preview) == "table" and type(preview.url) == "string" then
						poster = preview.url
					end
				end
				results[#results + 1] = {
					title = node.name or "",
					url = base_url .. RELEASE_PREFIX .. slug,
					poster = poster,
				}
			end
		end
		return results
	end,

	episodes = function(anime_url)
		-- The slug is separated from the release-page URL; a URL that
		-- does not carry the prefix (a foreign page) is the typed
		-- caller bug.
		local prefix = base_url .. RELEASE_PREFIX
		local slug = ""
		if string.sub(anime_url, 1, #prefix) == prefix then
			slug = string.sub(anime_url, #prefix + 1)
		end
		if slug == "" then
			anicli.fail("invalid_input", "URL не содержит слаг релиза: " .. anime_url)
		end

		local data = graphql(RELEASE_QUERY, { slug = slug })
		local release = data.release
		if release == nil then
			anicli.fail("not_found", "релиз \"" .. slug .. "\"")
		end
		if release.viewerInBlockedCountry == true then
			anicli.fail("geo_blocked", "контент SHIZA закрыт для этого региона")
		end

		local episodes = {}
		for _, ep in ipairs(release.episodes or {}) do
			if ep.number ~= nil then
				local embeds = episode_embeds(ep)
				episodes[#episodes + 1] = {
					num = tostring(ep.number),
					title = ep.name or "",
					-- raw_id carries the {e} embed list the fresh-sandbox
					-- streams() call resolves from — the sandbox's only
					-- channel for the data the compiled provider read
					-- from its in-memory RawEmbeds (zero shiza requests
					-- per resolve; the site tarpits the 4th GraphQL POST
					-- inside a minute window).
					raw_id = anicli.json.encode({ e = embeds }),
					raw_embeds = { [DUB] = embeds },
				}
			end
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- An unknown dub key carries no embeds: an empty stream
		-- without any network I/O (the Go RawEmbeds[dub] nil parity).
		if dub ~= DUB then
			return { dub_name = dub, links = {} }
		end

		-- The state JSON carries the episode's embed list (see
		-- episodes): no release re-fetch, the extraction is the only
		-- network this leg makes (kodik/sibnet player pages, never
		-- the shiza GraphQL budget).
		local ok, state = pcall(anicli.json.decode, raw_id)
		if not ok or type(state) ~= "table" or type(state.e) ~= "table" then
			anicli.fail("invalid_input", "raw_id не несёт список эмбедов эпизода: " .. tostring(raw_id))
		end
		local embeds = {}
		for _, u in ipairs(state.e) do
			if type(u) == "string" and u ~= "" then
				embeds[#embeds + 1] = u
			end
		end
		if #embeds == 0 then
			-- An empty embed list hydrates nothing: an empty stream,
			-- never an error (the Go resolveEmbeds empty-list parity).
			return { dub_name = dub, links = {} }
		end

		local links = anicli.extract(embeds)
		return { dub_name = dub, links = links }
	end,
}
