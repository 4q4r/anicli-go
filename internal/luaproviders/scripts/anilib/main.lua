-- AniLib (api.cdnlibs.org/api) — the PR122 Lua migration of the
-- compiled Go provider (itself a port of anicli-py anicli/providers/
-- anilib.py). The mangalib-family JSON API is header-gated (a plain
-- client 403s) and rate-limits by answering 403 on overuse, so every
-- request carries the browser-mimicking header set verbatim and the
-- search preflight runs SEQUENTIAL — the gentlest shape against the
-- overuse wall (the Go version fanned out ≤8-wide; the SDK's
-- get_batch cannot ride custom headers, so the parallel shape is not
-- available to scripts).
--
--   - search: GET /anime?limit=20&q=…&site_id=5 [LIVE-VERIFIED
--     2026-09-13 — the legacy site_id[]/fields[] forms are rejected
--     with 422]. HTTP and decode failures surface as an empty result
--     set, verbatim from the Python `except Exception: return []`
--     (documented quirk). The PR53 contentless preflight probes each
--     result's episode chain and drops releases whose first episode's
--     players list is positively empty; a failed probe keeps the
--     result (fail-open — absence of evidence is not contentlessness).
--   - episodes: GET /episodes?anime_id=N (the numeric id split off
--     the "ID--slug" URL). Python quirks preserved: a JSON-null number
--     renders as "None" and sorts with key 0; the sort is the stable
--     float-key order (index tiebreak = Python sorted parity). The
--     PR44 tier-1 fetch hydrates the sorted-first episode's players
--     and distributes the release's dub keys onto every episode as
--     keys with EMPTY lists — fail-soft, like the Go tier-1.
--   - streams: GET /episodes/{raw_id} — the fresh-sandbox contract
--     (streams receives strings only) turns the Go self-hydration
--     into an always-fresh refetch (+1 request per resolve, the
--     animedia precedent; resolve stays the truth-teller). AnimeLib
--     players resolve to video1.cdnlibs.org CDN links (the literal
--     "/.%D0%B0s/" prefix quirk, Referer v3.animelib.org); Kodik srcs
--     run through anicli.extract with the only-when-nothing-resolved
--     error rule; an unknown dub key resolves to an empty stream.

local base_url = "https://api.cdnlibs.org/api"

local CDN_BASE = "https://video1.cdnlibs.org/.%D0%B0s/"
local CDN_REFERER = "https://v3.animelib.org"

-- The load-bearing header set (anilib.py:27-41, anilib.go verbatim):
-- the API 403s without the browser-mimicking shape.
local HEADERS = {
	["Authority"] = "api.cdnlibs.org",
	["Accept"] = "application/json, text/plain, */*",
	["Accept-Language"] = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7",
	["Origin"] = "https://animelib.me",
	["Referer"] = "https://animelib.me/",
	["User-Agent"] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	["Sec-Ch-Ua"] = '"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"',
	["Sec-Ch-Ua-Mobile"] = "?0",
	["Sec-Ch-Ua-Platform"] = '"Windows"',
	["Sec-Fetch-Dest"] = "empty",
	["Sec-Fetch-Mode"] = "cors",
	["Sec-Fetch-Site"] = "cross-site",
}

-- api_get rides the header set on every request (the per-request
-- headers win over the netclient's own defaults, anilib.go parity).
local function api_get(path)
	return anicli.http.get(base_url .. path, { headers = HEADERS })
end

-- decode_json returns the decoded table or nil (never raises).
local function decode_json(text)
	local ok, data = pcall(anicli.json.decode, text)
	if not ok or type(data) ~= "table" then
		return nil
	end
	return data
end

-- fetch_json_strict: transport failures RAISE (the SDK's typed
-- markers — provider_403, timeout — re-attach their sentinels in the
-- adapter); a body that will not decode raises too. streams() uses
-- this shape: its fetch IS the hydration, and a hydration failure is
-- a loud resolve failure (anilib.go FetchDubs fails loud).
local function fetch_json_strict(path)
	local resp = api_get(path)
	local data = decode_json(resp.body)
	if data == nil then
		error("invalid json from " .. path, 0)
	end
	return data
end

-- fetch_json: the swallow variant — any failure becomes nil. This is
-- the Python `except Exception` quirk the search and episode listing
-- preserve verbatim (HTTP/decode failures surface as empty results).
local function fetch_json(path)
	local ok, data = pcall(fetch_json_strict, path)
	if not ok then
		return nil
	end
	return data
end

-- unquote ports Python unquote / Go url.PathUnescape (the query is
-- percent-DECODED before being re-encoded by query_escape). A literal
-- "+" stays "+"; ANY invalid escape falls back to the raw string —
-- the Go provider's whole-query fallback (PathUnescape errors),
-- stricter than Python's per-escape pass-through and the shape the
-- compiled provider pinned.
local function unquote(s)
	local pos = string.find(s, "%", 1, true)
	while pos do
		if string.match(string.sub(s, pos + 1, pos + 2), "^%x%x$") == nil then
			return s
		end
		pos = string.find(s, "%", pos + 3, true)
	end
	return (string.gsub(s, "%%(%x%x)", function(hex)
		return string.char(tonumber(hex, 16))
	end))
end

-- slug_id splits the numeric id off the "ID--slug-name" URL
-- (anilib.py:91); a bare id passes through.
local function slug_id(u)
	local pos = string.find(u, "--", 1, true)
	if pos then
		return string.sub(u, 1, pos - 1)
	end
	return u
end

-- num_str ports pythonStr over a decoded JSON value: the wire literal
-- is preserved ("1" stays "1", 0.5 stays "0.5") and a JSON null
-- renders as "None" — str(None) — which sorts with key 0.
local function num_str(v)
	if v == nil then
		return "None"
	end
	if type(v) == "string" then
		return v
	end
	return tostring(v)
end

-- float_key ports the Python episode sort key
-- float(x.num) if x.num.replace('.', '', 1).isdigit() else 0
-- (anilib.py:114): "None" keys 0, "0.5" keys 0.5.
local function float_key(num)
	local stripped = string.gsub(num, "%.", "", 1)
	if stripped == "" or string.match(stripped, "^%d+$") == nil then
		return 0
	end
	return tonumber(num) or 0
end

-- first_non_empty is the Python or-chain with Go firstNonEmpty
-- semantics: an EMPTY STRING falls through to the next candidate
-- (Lua's `or` alone would keep "" — falsy only for nil/false).
local function first_non_empty(a, b, c)
	if a ~= nil and a ~= "" then
		return a
	end
	if b ~= nil and b ~= "" then
		return b
	end
	if c ~= nil and c ~= "" then
		return c
	end
	return ""
end

-- embeds_from_players builds the raw_embeds map from an episode's
-- players list (anilib.py:124-136): the dub key is "Team (Player)"
-- with the Unknown fallback; Kodik stashes its embed src, AnimeLib an
-- internal: JSON payload of the video object.
local function embeds_from_players(players)
	local embeds = {}
	for _, player in ipairs(players or {}) do
		local team = (player.team or {}).name
		if team == nil or team == "" then
			team = "Unknown"
		end
		local key = team .. " (" .. tostring(player.player) .. ")"
		if player.player == "Kodik" then
			local src = player.src
			if src == nil then
				src = ""
			end
			embeds[key] = { src }
		elseif player.player == "AnimeLib" then
			-- The payload encodes a plain JSON-decoded table — an encode
			-- failure would mean a broken SDK, so it fails loud (the Go
			-- branch wraps the "impossible" marshal error the same way).
			local ok, payload = pcall(anicli.json.encode, player.video or {})
			if not ok then
				error("encode internal video payload: " .. tostring(payload), 0)
			end
			embeds[key] = { "internal:" .. payload }
		end
	end
	return embeds
end

-- resolve_internal decodes an internal: payload into CDN links
-- (anilib.py:146-159): per quality entry, the href gains the literal
-- "/.%D0%B0s/" CDN prefix and the v3.animelib.org Referer; a missing
-- quality defaults to 1080 (Python q.get) and later entries overwrite
-- earlier ones (dict parity — the default-1080 entry replaces a real
-- 1080). Malformed payloads are skipped, matching the Python bare
-- `except: pass`.
local function resolve_internal(links, payload)
	local ok, video = pcall(anicli.json.decode, payload)
	if not ok or type(video) ~= "table" then
		return
	end
	for _, q in ipairs(video.quality or {}) do
		local href = q.href
		if href ~= nil and href ~= "" then
			local quality = "1080"
			if q.quality ~= nil then
				quality = tostring(q.quality)
			end
			links[quality] = {
				url = CDN_BASE .. href,
				quality = quality,
				headers = { Referer = CDN_REFERER },
			}
		end
	end
end

-- probe_contentless reports whether the release behind a search-result
-- slug carries no dub content: the episode list is empty, or the
-- sorted-first episode's players list is empty. Transport/decode
-- failures RAISE (the caller fail-opens).
local function probe_contentless(slug_url)
	local anime_id = slug_id(slug_url)
	local list = fetch_json("/episodes?anime_id=" .. anicli.http.query_escape(anime_id))
	if list == nil then
		error("probe: episodes list of " .. anime_id .. " failed", 0)
	end
	local episodes = list.data
	if episodes == nil or #episodes == 0 then
		return true
	end
	local first = episodes[1]
	for i = 2, #episodes do
		if float_key(num_str(episodes[i].number)) < float_key(num_str(first.number)) then
			first = episodes[i]
		end
	end
	local detail = fetch_json("/episodes/" .. tostring(first.id))
	if detail == nil then
		error("probe: episode " .. tostring(first.id) .. " detail failed", 0)
	end
	local players = (detail.data or {}).players or {}
	return #players == 0
end

-- filter_contentless drops positively-contentless releases, feed order
-- preserved. The probes ride the SAME endpoints the tier-1 hydration
-- uses (episode list, then the sorted-first episode's detail) and run
-- SEQUENTIALLY: the API rate-limits with 403 on overuse, the SDK's
-- get_batch cannot carry the header set, and a failed probe fails
-- open — a flaky or throttling API must never empty the search.
local function filter_contentless(results)
	local kept = {}
	for _, r in ipairs(results) do
		local ok, contentless = pcall(probe_contentless, r.url)
		if ok and contentless then
			anicli.log.info("anilib: dropped contentless release release="
				.. tostring(r.url) .. " title=" .. tostring(r.title))
		else
			kept[#kept + 1] = r
		end
	end
	return kept
end

-- hydrate_first is the PR44 tier-1: the sorted-first episode's players
-- name every team voicing the release; the key set rides every other
-- episode as keys with EMPTY lists (episode one keeps its real links).
-- Fail-soft: a failed fetch leaves the dub lists unknown until an
-- episode is opened (the Go `if err == nil` gate).
local function hydrate_first(episodes)
	local data = fetch_json("/episodes/" .. episodes[1].raw_id)
	if data == nil then
		return
	end
	episodes[1].raw_embeds = embeds_from_players((data.data or {}).players)
	for i = 2, #episodes do
		local ep = episodes[i]
		for dub in pairs(episodes[1].raw_embeds) do
			if ep.raw_embeds[dub] == nil then
				ep.raw_embeds[dub] = {}
			end
		end
	end
end

return {
	id = "anilib",
	name = "AnimeLib",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "black lagoon",

	search = function(query)
		local q = unquote(query)
		-- Parameter order mirrors Go's url.Values.Encode (sorted keys);
		-- site_id=5 is the LIVE-VERIFIED 2026-09-13 form (the legacy
		-- site_id[]/fields[] selectors are rejected with 422).
		local data = fetch_json("/anime?limit=20&q=" .. anicli.http.query_escape(q) .. "&site_id=5")
		if data == nil then
			return {}
		end

		local results = {}
		for _, item in ipairs(data.data or {}) do
			results[#results + 1] = {
				title = first_non_empty(item.rus_name, item.name, item.eng_name),
				url = item.slug_url or "",
				poster = item.cover and item.cover.default or nil,
			}
		end
		return filter_contentless(results)
	end,

	episodes = function(anime_url)
		local anime_id = slug_id(anime_url)
		local data = fetch_json("/episodes?anime_id=" .. anicli.http.query_escape(anime_id))
		if data == nil then
			return {}
		end

		-- Decorate-sort-undecorate with the arrival index as tiebreak:
		-- Python sorted()/Go SliceStable parity (Lua's table.sort is
		-- not stable).
		local sorted = {}
		for i, item in ipairs(data.data or {}) do
			local title = item.name
			if title == nil or title == "" then
				title = "Episode"
			end
			sorted[#sorted + 1] = {
				num = num_str(item.number),
				title = title,
				raw_id = tostring(item.id),
				raw_embeds = {},
				idx = i,
			}
		end
		table.sort(sorted, function(a, b)
			local ka, kb = float_key(a.num), float_key(b.num)
			if ka ~= kb then
				return ka < kb
			end
			return a.idx < b.idx
		end)

		local episodes = {}
		for _, ep in ipairs(sorted) do
			ep.idx = nil
			episodes[#episodes + 1] = ep
		end
		if #episodes > 0 then
			pcall(hydrate_first, episodes)
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- The always-fresh hydration: the episode detail is re-fetched
		-- from the bare raw_id (cached embeds are never trusted — the
		-- PR111 rule the fresh-sandbox contract makes structural).
		local data = fetch_json_strict("/episodes/" .. tostring(raw_id))
		local embeds = embeds_from_players((data.data or {}).players)

		local links = embeds[dub]
		if links == nil then
			-- An unknown dub hydrates nothing: an empty stream, never an
			-- error (anilib.go parity).
			return { dub_name = dub, links = {} }
		end

		local out = {}
		for _, link in ipairs(links) do
			if string.sub(link, 1, #"internal:") == "internal:" then
				resolve_internal(out, string.sub(link, #"internal:" + 1))
			else
				if string.sub(link, 1, 2) == "//" then
					link = "https:" .. link
				end
				-- The extractor merge rule (anilib.py:161-162): a link
				-- whose extraction fails surfaces its error only while no
				-- source has resolved yet, never shadowing resolved ones.
				local ok, sources = pcall(anicli.extract, link)
				if not ok then
					if next(out) == nil then
						error(sources, 0)
					end
				else
					for quality, src in pairs(sources) do
						out[quality] = src
					end
				end
			end
		end
		return { dub_name = dub, links = out }
	end,
}
