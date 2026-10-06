-- AniFilm (anifilm.pro) — the PR135 Lua migration of the compiled Go
-- provider (PR91; written from the live site, no frozen Python
-- original). The site runs a custom "AniFilm System" engine (Yii CSRF
-- on the account forms, Vue components for the player) — NOT DLE: the
-- sameband/animedia DLE search-form recipe does not apply. The
-- observed shapes (characterized 2026-09-23, re-verified live through
-- the configured proxy 2026-10-06):
--
--   - search: GET /releases?title=<query> — the site's own header form
--     is method=get (the engine also exposes a fast-search
--     /releases/api:search fragment, but the full page carries the
--     complete result set). Results render server-side as
--     .releases__item cards; the RU title link is
--     a.releases__title-russian[href], the poster img.releases__image
--     [src]. A miss is HTTP 200 with zero cards (an empty result,
--     never an error). The query rides the percent-encoded UTF-8 the
--     form itself submits.
--   - episodes: the release page hydrates <player-component> from its
--     :releases_id and :services_props attributes; both scrape off the
--     RAW page — the Vue render emits the attributes unquoted, and an
--     HTML re-serialization would escape the JSON's double quotes into
--     entities. The episode playlist is GET
--     /releases/api:online:<id>:<service> — a JSON array of {id, rid,
--     episode, title, iframe, imageUrl, from} rows, every field a JSON
--     string. Active services only, kodik first when present (the
--     extractor-covered default), every other active service in
--     document order; the trailer "service" is never polled (a promo
--     source, not an episode surface). The playlist's raw iframe field
--     points at a kodik mirror host (anivod.com) that the extractor
--     gate does not cover and that fails TLS on some networks; the
--     embeds instead carry the release's own
--     /releases/api:video:<row id> page, which wraps the canonical
--     kodikplayer.com URL.
--   - streams: the api:video page carries one iframe.iframe-player
--     pointing at kodikplayer.com; it resolves through the shared
--     extractor factory (anicli.extract — the 2026-09 vInfo player
--     shape with the /ftor default API path, live-probed 2026-09-23:
--     360/480/720 mp4 for a Devil May Cry episode).
--
-- Dub naming: the release page's credited voices
-- (.release__work-item--voice a[href*="/releases/voice/"]; the voice-
-- TYPE links on /releases/voice-type/ do not credit a voice). Exactly
-- one credited voice names the single translation inside the embeds;
-- zero or several credits stay ambiguous and fall back to the service
-- dub "AniFilm" (the Дорохедоро: Спешл live case, 5 credited voices
-- over single-translation embeds).
--
-- Typed walls (the no-silent-failure ruling, kept from the Go port):
-- a release page without a parsable releases_id, unparsable player
-- props, a release whose active services serve no decodable playlist
-- and an iframe-less video page are all extract_failed naming the
-- serving shape; a deleted-but-indexed release (the search index lists
-- pages that answer 404, observed live 2026-09-23) rides the
-- transport layer's typed not-found. Contract shift forced by the
-- fresh-sandbox Lua adapter (the sameband/animego precedent),
-- documented here rather than hidden: streams(raw_id, dub) receives
-- no dub set, so the Go provider's unknown-dub caller-bug wall
-- (ErrInvalidInput) has no surface — the resolve re-derives everything
-- from the playlist row id that rides RawID.
--
-- Torrents: the release pages list per-release .torrent downloads
-- (GET /releases/download-torrent/<tid> answers a real torrent file
-- anonymously) — a TorrentBase extension candidate, deliberately out
-- of scope for the stream provider.

local base_url = "https://anifilm.pro"

-- SERVICE_DUB is the dub name for titles whose release page credits
-- zero or several voices while the playlist carries exactly one
-- unnamed translation per episode (the amd.online service-dub
-- precedent).
local SERVICE_DUB = "AniFilm"

-- abs absolutizes the site's relative URLs against the base:
-- protocol-relative gains https, absolute URLs pass through, anything
-- else gains the site root.
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
	return base_url .. u
end

-- The services_props entry shape: document-order "key":{from,active}
-- pairs (the balanced JSON is one level of nesting; the prop value
-- carries the same name as its key).
local SERVICES_RE = [["([^"]+)"\s*:\s*\{\s*"from"\s*:\s*"([^"]*)"\s*,\s*"active"\s*:\s*(true|false)]]

-- balanced_json returns the first balanced {...} block of s (s starts
-- at the opening brace): a byte scan honoring JSON strings and
-- escapes, the Go port's shape (the value nests one level).
local function balanced_json(s)
	local depth = 0
	local in_string = false
	local escaped = false
	for i = 1, #s do
		local c = string.byte(s, i)
		if escaped then
			escaped = false
		elseif c == 92 and in_string then -- backslash
			escaped = true
		elseif c == 34 then -- double quote
			in_string = not in_string
		elseif not in_string then
			if c == 123 then -- {
				depth = depth + 1
			elseif c == 125 then -- }
				depth = depth - 1
				if depth == 0 then
					return string.sub(s, 1, i)
				end
			end
		end
	end
	return nil
end

-- service_pairs extracts the (from, active) pairs off the balanced
-- services_props JSON in DOCUMENT ORDER — the kodik-first preference
-- rides this order, and a decoded object table would scramble it (Go
-- map semantics; pairs() has none either). An empty from value falls
-- back to its key (the prop value carries the same name live).
local function service_pairs(json)
	local out = {}
	local pos = 1
	while pos <= #json do
		local m = anicli.regexp.match(SERVICES_RE, string.sub(json, pos))
		if m == nil then
			break
		end
		local from = m[3]
		if from == "" then
			from = m[2]
		end
		out[#out + 1] = { from = from, active = m[4] == "true" }
		local s, e = string.find(json, m[1], pos, true)
		if s == nil then
			break
		end
		pos = e + 1
	end
	return out
end

-- parse_services_props locates and parses the player-component's
-- :services_props attribute off the raw page; every breakage is the
-- typed extract wall (no anonymous episode surface without it).
local function parse_services_props(page)
	local idx = string.find(page, ":services_props=", nil, true)
	if idx == nil then
		anicli.fail("extract_failed", "release page player props unparsable: no services_props attribute")
	end
	local rest = string.sub(page, idx + #":services_props=")
	local start = string.find(rest, "{", nil, true)
	if start == nil then
		anicli.fail("extract_failed", "release page player props unparsable: services_props carries no JSON object")
	end
	local raw = balanced_json(string.sub(rest, start))
	if raw == nil then
		anicli.fail("extract_failed", "release page player props unparsable: unbalanced JSON object")
	end
	local services = service_pairs(raw)
	if #services == 0 then
		anicli.fail("extract_failed", "release page player props unparsable: services_props is empty")
	end
	return services
end

-- service_names renders the parsed service list for wall messages
-- ("none" when the attribute parsed to nothing usable).
local function service_names(services)
	local names = {}
	for _, s in ipairs(services) do
		names[#names + 1] = s.from
	end
	if #names == 0 then
		return "none"
	end
	return table.concat(names, ", ")
end

-- servable_services orders the services for the playlist walk: ACTIVE
-- services only (the site's own default surface), kodik first when
-- present, every other active service in document order. The trailer
-- service is never servable (a promo source, not episodes).
local function servable_services(services)
	local out = {}
	for _, s in ipairs(services) do
		if s.from ~= "trailer" and s.active then
			out[#out + 1] = s
		end
	end
	for i = #out, 1, -1 do
		if out[i].from == "kodik" then
			local kodik = table.remove(out, i)
			table.insert(out, 1, kodik)
			break
		end
	end
	local names = {}
	for _, s in ipairs(out) do
		names[#names + 1] = s.from
	end
	return names
end

-- fetch_playlist walks the servable services in order and returns the
-- first playlist that decodes to at least one row. A dead service
-- (transport failure) and a 200-with-empty (or junk) answer are not
-- yet walls — the walk continues; the typed wall at the end names the
-- full service set and what was polled.
local function fetch_playlist(releases_id, services)
	local tried = {}
	for _, name in ipairs(servable_services(services)) do
		tried[#tried + 1] = name
		local ok, resp = pcall(anicli.http.get,
			base_url .. "/releases/api:online:" .. releases_id .. ":" .. name)
		if ok then
			local decoded, rows = pcall(anicli.json.decode, resp.body)
			if decoded and type(rows) == "table" and #rows > 0 then
				return rows
			end
		end
	end
	anicli.fail("extract_failed", "release " .. releases_id
		.. " serves episodes through an unsupported service set (active: "
		.. service_names(services) .. "; polled: " .. table.concat(tried, ", ") .. ")")
end

-- dub_name derives the dub label from the release page's credited
-- voices: exactly one credited voice names it, anything else falls
-- back to the service dub. The voice block also carries the voice-TYPE
-- links — only anchors into /releases/voice/ credit a voice.
local function dub_name(doc)
	local voices, seen = {}, {}
	doc:find('.release__work-item--voice a[href*="/releases/voice/"]'):each(function(_, sel)
		local name = sel:text()
		if name == "" or seen[name] then
			return
		end
		seen[name] = true
		voices[#voices + 1] = name
	end)
	if #voices == 1 then
		return voices[1]
	end
	return SERVICE_DUB
end

-- all_digits reports whether every value is a non-empty digit run
-- (the amd.online numeric-order gate; an empty list never sorts).
local function all_digits(values)
	if #values == 0 then
		return false
	end
	for _, v in ipairs(values) do
		if string.match(v, "^%d+$") == nil then
			return false
		end
	end
	return true
end

return {
	id = "anifilm",
	name = "AniFilm",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "дьявол",

	search = function(query)
		local resp = anicli.http.get(base_url .. "/releases?title=" .. anicli.http.query_escape(query))
		local doc = anicli.html.parse(resp.body)

		local results = {}
		doc:find(".releases__item"):each(function(_, card)
			local title_node = card:find("a.releases__title-russian[href]")
			if title_node:len() == 0 then
				return
			end
			local title = title_node:text()
			local href = title_node:attr("href")
			if title == "" or href == "" then
				return
			end
			local poster = ""
			local img = card:find("img.releases__image[src]")
			if img:len() > 0 then
				poster = abs(img:attr("src"))
			end
			results[#results + 1] = {
				title = title,
				url = abs(href),
				poster = poster,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local page = resp.body

		-- The player-component's releases_id rides the RAW page
		-- unquoted (`:releases_id=1200`); a release page without it
		-- carries no anonymous episode surface.
		local releases_id = string.match(page, ":releases_id=(%d+)")
		if releases_id == nil or releases_id == "" then
			anicli.fail("extract_failed",
				"release page carries no player-component releases_id; no anonymous episode surface")
		end

		local services = parse_services_props(page)
		local dub = dub_name(anicli.html.parse(page))
		local rows = fetch_playlist(releases_id, services)

		local episodes = {}
		local nums = {}
		for _, row in ipairs(rows) do
			local num = row.episode
			if num == nil or num == "" then
				num = row.id -- the row id still orders deterministically
			end
			episodes[#episodes + 1] = {
				num = num,
				title = row.title,
				raw_id = row.id,
				raw_embeds = { [dub] = { abs("/releases/api:video:" .. row.id) } },
			}
			nums[#nums + 1] = num
		end

		-- Numeric ascending order when every episode number is digits
		-- (the live playlist is ascending already; the sort must not
		-- disturb it).
		if all_digits(nums) then
			table.sort(nums, function(a, b)
				return tonumber(a) < tonumber(b)
			end)
			local pos = {}
			for i, n in ipairs(nums) do
				pos[n] = i
			end
			local ordered = {}
			for _, ep in ipairs(episodes) do
				ordered[pos[ep.num]] = ep
			end
			episodes = ordered
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- The episode's api:video page re-derives from the playlist
		-- row id (the fresh-sandbox state channel): it wraps the
		-- canonical kodikplayer.com iframe, which resolves through
		-- the shared extractor factory. A transport failure rides the
		-- SDK's typed markers; an iframe-less page is the extract
		-- wall.
		local resp = anicli.http.get(base_url .. "/releases/api:video:" .. tostring(raw_id))
		local frame = anicli.html.parse(resp.body):find("iframe.iframe-player[src]")
		if frame:len() == 0 then
			anicli.fail("extract_failed",
				"video page for episode " .. tostring(raw_id) .. " carries no iframe-player embed")
		end

		-- The extractor merge rule: a link whose extraction fails
		-- surfaces only while nothing resolved anywhere (the factory's
		-- total-failure rule), retyped through the provider taxonomy.
		local ok, sources = pcall(anicli.extract, frame:attr("src"))
		if not ok then
			anicli.fail("extract_failed", tostring(sources))
		end
		return { dub_name = dub, links = sources }
	end,
}
