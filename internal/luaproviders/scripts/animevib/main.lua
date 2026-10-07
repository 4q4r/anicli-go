-- AnimeVib (www.animevib.ru) — the RU DLE catalog whose every post
-- embeds ONE kodik player, the PR116 Lua migration of the compiled Go
-- provider. The site is DataLife Engine (the DLE search form answers
-- server-side; the ?s= parameter is silently ignored):
--
--   - search: GET /index.php?do=search&subaction=search&story=…
--     (server-side matching; both RU and latin queries surface
--     results — verified live with дандадан and dandadan). Results
--     render as article.movie-item cards — a class pair the homepage
--     catalog cards (.shortstory-in) and the sidebar rail
--     (.side_ongoing) do not carry, so neither can leak in.
--   - episodes: the post embeds one kodik player on
--     iframe.player-shar. /serial/ embeds: the kodik player page
--     lists the translations (data-media-id/data-media-hash) and, per
--     translation, the episodes with per-episode seria data-id/
--     data-hash — the script fetches each translation's serial page
--     (bounded-parallel via anicli.http.get_batch, 4 in flight: the
--     config default for network.max_parallel, out of script reach)
--     and merges the (episode × dub) table. /video/ embeds (movies):
--     one episode under the AnimeVib service dub.
--   - streams: the synthesized /seria/ embeds (and /video/ movie
--     embeds) resolve through anicli.extract — the shared kodik
--     extractor flow.
--
-- The stloadi.live ad player in the post's second tab is never
-- classed player-shar and is never selected.
--
-- State passing: streams(raw_id, dub) receives the {n, u} JSON (the
-- fresh-sandbox contract) and re-derives: post page → embed → the
-- dub's serial page → the episode's seria embed (+3 network fetches
-- per resolve, network-bound — once per chosen dub).
--
-- Seria/serial URL derivation note: both URLs reuse the embed URL's
-- PREFIX up to its /serial/ (or /video/) segment. For live kodik
-- pages that prefix is the bare origin (kodikplayer.com — identical
-- to the Go avSerialPage/avSeriaEmbed), and mirrors hosting the
-- player under a subpath keep their prefix (the substring the
-- extractor gate matches on rides along).
--
-- DUB FALLBACK DOCTRINE (fix-round 2 of #158, 2026-10-07 — owner
-- review requested): the site carries up to 48 dubs via PER-EPISODE
-- translation tables, and dub coverage genuinely varies episode to
-- episode — a dub watched on episode 1 legitimately may not serve
-- episode 3. streams() therefore NEVER walls on a missing dub: when
-- the requested dub is absent from the episode's table (dropped from
-- the translations select, or its per-episode coverage ends before
-- this episode), the resolve falls back to the episode's FIRST
-- available dub in the deterministic episodes() table order — the
-- embed dub first, then the select order (the same order episodes()
-- merges the per-episode tables under) — and attributes the RESOLVED
-- dub in dub_name. The fallback batch rides the same bounded-
-- parallel get_batch as episodes() (MAX_TRANSLATION_PARALLEL). The
-- ONLY remaining walls: a malformed raw_id (invalid_input, the
-- fresh-sandbox contract) and an episode that NO dub carries at all
-- (not_found — a data-shape fact, not a caller mistake). The dub
-- name matched here is the provider's OWN bare name (the kodik
-- select's data-title, what episodes() merges under); the merged-
-- session "[prov] " track tag is stripped at the Go provider
-- boundary (python extract_best_source parity, tui realEpisode /
-- downloadOne / api streams-resolve).

local base_url = "https://www.animevib.ru"

-- SERVICE_DUB is the dub name for /video/ (movie) embeds: a kodik
-- video page carries no translation metadata (amdServiceDub precedent).
local SERVICE_DUB = "AnimeVib"

-- MAX_TRANSLATION_PARALLEL bounds the per-translation serial-page
-- fan-out (get_batch). netclient.Parallel treats ≤0 as unbounded and
-- the script cannot read network.max_parallel — the config default
-- (4) is pinned here; the wave-A review F3 clamp precedent.
local MAX_TRANSLATION_PARALLEL = 4

local KODIK_RE = "/(serial|video)/(\\d+)/([0-9a-f]{32})(?:/\\d+p?)?/?$"
local POSTER_RE = "background-image:\\s*url\\(([^)]+)\\)"

local function trim(s)
	return (string.match(s or "", "^%s*(.-)%s*$"))
end

-- parse_embed absolutizes the protocol-relative iframe src with the
-- post page's scheme and splits the kodik URL shape. A player-shar
-- iframe that is neither a kodik /serial/ nor /video/ embed is the
-- typed extraction failure (the site has no other player family on
-- that class — the stloadi ad tab carries no class at all).
local function parse_embed(src, post_url)
	if src == nil or src == "" then
		anicli.fail("not_found", "post page carries no player iframe; no anonymous episode surface")
	end
	local absolute
	if string.sub(src, 1, 2) == "//" then
		local scheme = string.match(post_url, "^(%a+:)//")
		absolute = (scheme or "https:") .. src
	elseif string.sub(src, 1, 4) == "http" then
		absolute = src
	else
		anicli.fail("extract_failed", "bad player iframe src \"" .. src .. "\"")
	end

	local m = anicli.regexp.match(KODIK_RE, absolute)
	if not m then
		anicli.fail("extract_failed", "unsupported player embed \"" .. absolute ..
			"\" (want a kodik /serial/ or /video/ url)")
	end
	-- The prefix up to the /serial|video/ segment: the bare origin for
	-- live kodik pages, the subpath-inclusive base for mirrors.
	local prefix = string.match(absolute, "^(.-)/serial/") or string.match(absolute, "^(.-)/video/")
	return {
		url = absolute,
		kind = m[2],
		id = m[3],
		hash = m[4],
		prefix = prefix,
	}
end

local function serial_page(prefix, id, hash)
	return prefix .. "/serial/" .. id .. "/" .. hash .. "/720p"
end

local function seria_embed(prefix, id, hash)
	return prefix .. "/seria/" .. id .. "/" .. hash .. "/720p"
end

-- parse_serial_page scrapes a kodik serial page: the dub title the
-- page serves (the seasons select's data-translation-title; the
-- fallback is the service dub — it never fires on live serial pages),
-- the translations select (data-title is the bare dub name; the
-- option text carries an «(N эп.)» suffix) and the VISIBLE series
-- select's per-episode seria options (the hidden div.season-N groups
-- under .series-options repeat the same markup and must not double
-- the table — live pages carry 24 hash-bearing options, 12 duplicated).
local function parse_serial_page(body)
	local doc = anicli.html.parse(body)

	local title = doc:find(".serial-seasons-box option[data-translation-title]"):attr("data-translation-title")
	title = trim(title)
	if title == "" then
		title = SERVICE_DUB
	end

	local translations = {}
	doc:find(".serial-translations-box option[data-media-id][data-media-hash]"):each(function(_, opt)
		local media_id = opt:attr("data-media-id")
		local media_hash = opt:attr("data-media-hash")
		local dub = trim(opt:attr("data-title"))
		if media_id ~= "" and media_hash ~= "" and dub ~= "" then
			translations[#translations + 1] = { id = media_id, hash = media_hash, title = dub }
		end
	end)

	local episodes, seen = {}, {}
	doc:find(".serial-series-box select option[data-id][data-hash]"):each(function(_, opt)
		local val = opt:attr("value")
		local id = opt:attr("data-id")
		local hash = opt:attr("data-hash")
		local key = val .. "/" .. id .. "/" .. hash
		if val ~= "" and id ~= "" and hash ~= "" and not seen[key] then
			seen[key] = true
			episodes[#episodes + 1] = { num = val, id = id, hash = hash }
		end
	end)

	return { title = title, translations = translations, episodes = episodes }
end

-- page_embeds lists the seria embeds of ONE parsed serial page that
-- carry the episode num (the per-dub resolve unit).
local function page_embeds(prefix, page, num)
	local embeds = {}
	for _, ep in ipairs(page.episodes) do
		if ep.num == num then
			embeds[#embeds + 1] = seria_embed(prefix, ep.id, ep.hash)
		end
	end
	return embeds
end

return {
	id = "animevib",
	name = "AnimeVib",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "дандадан",

	search = function(query)
		local search_url = base_url .. "/index.php?do=search&subaction=search&story=" ..
			anicli.http.query_escape(query)
		local resp = anicli.http.get(search_url)

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find("article.movie-item"):each(function(_, item)
			local link = item:find("a.short__title[href]")
			if link:len() == 0 then
				return
			end
			local href = link:attr("href")
			local title = trim(link:text())
			if href == "" or title == "" then
				return
			end
			local m = anicli.regexp.match(POSTER_RE, item:find("figure div[style]"):attr("style"))
			local poster = ""
			if m then
				poster = trim(m[2])
			end
			results[#results + 1] = { title = title, url = href, poster = trim(poster) }
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local embed = parse_embed(anicli.html.parse(resp.body):find("iframe.player-shar[src]"):attr("src"), anime_url)

		-- Movie: kodik video pages carry no translations or episode
		-- selects — one episode, one unnamed dub.
		if embed.kind == "video" then
			return { {
				num = "1",
				title = "1 серия",
				raw_id = anicli.json.encode({ n = "1", u = anime_url }),
				raw_embeds = { [SERVICE_DUB] = { embed.url } },
			} }
		end

		-- Serial: the main embed page IS a translation serial page (the
		-- default dub's) — parse it, then fetch every sibling
		-- translation bounded-parallel.
		local main_resp = anicli.http.get(embed.url)
		local main = parse_serial_page(main_resp.body)

		-- The main page's translation entry reuses the already-fetched
		-- page; its name comes from the translations select. The other
		-- translations build the bounded-parallel fetch list.
		local main_title = main.title
		local urls, dubs = {}, {}
		for _, tr in ipairs(main.translations) do
			if tr.id == embed.id and tr.hash == embed.hash then
				main_title = tr.title
			else
				urls[#urls + 1] = serial_page(embed.prefix, tr.id, tr.hash)
				dubs[#dubs + 1] = tr.title
			end
		end

		-- Bounded-parallel serial-page fetch per translation; a failed
		-- (transport-level) fetch contributes nothing — one dead dub
		-- team must not kill the table.
		local by_num, nums, any_error, first_error = {}, {}, false, nil
		local function add_seria(dub, episodes)
			for _, ep in ipairs(episodes) do
				local row = by_num[ep.num]
				if not row then
					row = {}
					by_num[ep.num] = row
					nums[#nums + 1] = ep.num
				end
				local list = row[dub]
				if not list then
					list = {}
					row[dub] = list
				end
				list[#list + 1] = seria_embed(embed.prefix, ep.id, ep.hash)
			end
		end

		add_seria(main_title, main.episodes)
		local batch = anicli.http.get_batch(urls, MAX_TRANSLATION_PARALLEL)
		for i, res in ipairs(batch) do
			if res.error then
				any_error = true
				if not first_error then
					first_error = res.error
				end
			else
				add_seria(dubs[i], parse_serial_page(res.body).episodes)
			end
		end

		if #nums == 0 then
			if any_error then
				anicli.fail("extract_failed", "kodik serial pages failed: " .. (first_error or "unknown"))
			end
			anicli.fail("not_found", "serial page carries selects but no episode rows from any translation")
		end

		table.sort(nums, function(a, b)
			local na, nb = tonumber(a), tonumber(b)
			if na and nb then
				return na < nb
			end
			return a < b
		end)

		local episodes = {}
		for _, num in ipairs(nums) do
			episodes[#episodes + 1] = {
				num = num,
				title = num .. " серия",
				raw_id = anicli.json.encode({ n = num, u = anime_url }),
				raw_embeds = by_num[num],
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- The state guard: raw_id is ALWAYS this script's own {n, u}
		-- json (the fresh-sandbox contract above), but any other byte
		-- sequence — a caller composing the merged prov:id convention
		-- without decomposing it (#157), a stale history record — must
		-- surface typed, never as the raw json.decode VM error through
		-- to the user (the anitokyo blob-decode guard precedent).
		local ok, state = pcall(anicli.json.decode, raw_id)
		if not ok or type(state) ~= "table" or type(state.u) ~= "string" or state.u == "" then
			anicli.fail("invalid_input",
				"episode raw_id is not the {n, u} state json: \"" ..
				string.sub(tostring(raw_id), 1, 64) .. "\"")
		end
		local num, page = tostring(state.n), state.u

		local resp = anicli.http.get(page)
		local embed = parse_embed(anicli.html.parse(resp.body):find("iframe.player-shar[src]"):attr("src"), page)

		local embeds, resolved_dub
		if embed.kind == "video" then
			embeds = { embed.url }
			resolved_dub = dub
		else
			-- the embed page IS a translation serial page (the embed
			-- dub's own) — parse it once; it is the fallback's first
			-- deterministic candidate.
			local main = parse_serial_page(anicli.http.get(embed.url).body)

			-- the requested dub by EXACT bare name (the kodik select's
			-- data-title; the merged "[prov] " track tag is stripped
			-- at the Go provider boundary)
			local target
			for _, tr in ipairs(main.translations) do
				if tr.title == dub then
					target = tr
					break
				end
			end
			local target_is_embed = target ~= nil and target.id == embed.id and target.hash == embed.hash

			-- Stage 1 — the candidates already in hand, walked in the
			-- episodes() table order: the requested dub first (it owns
			-- the playback when its own page carries the episode), then
			-- the embed dub.
			local candidates = {}
			local function add_candidate(tr, page)
				candidates[#candidates + 1] = { tr = tr, page = page }
			end
			if target then
				if target_is_embed then
					add_candidate(target, main) -- the embed dub IS the requested dub
				else
					add_candidate(target, parse_serial_page(anicli.http.get(serial_page(embed.prefix, target.id, target.hash)).body))
				end
			end
			if not target_is_embed then
				add_candidate({ id = embed.id, hash = embed.hash, title = main.title }, main)
			end

			local function walk()
				for _, cand in ipairs(candidates) do
					local ep_embeds = page_embeds(embed.prefix, cand.page, num)
					if #ep_embeds > 0 then
						return ep_embeds, cand.tr.title
					end
				end
				return nil, nil
			end

			embeds, resolved_dub = walk()

			if not embeds then
				-- Stage 2 — DUB FALLBACK (fix-round 2 of #158, see the
				-- header doctrine): the requested dub is not in this
				-- episode's table (absent from the select, or its
				-- per-episode coverage ends before this episode).
				-- Bounded-parallel page fetch per remaining translation
				-- (the episodes() fan-out) in the select order; a
				-- failed (transport-level) fetch contributes nothing —
				-- one dead dub team must not kill the fallback.
				local urls, metas = {}, {}
				for _, tr in ipairs(main.translations) do
					local is_target = target ~= nil and tr.id == target.id and tr.hash == target.hash
					local is_embed = tr.id == embed.id and tr.hash == embed.hash
					if not is_target and not is_embed then
						urls[#urls + 1] = serial_page(embed.prefix, tr.id, tr.hash)
						metas[#metas + 1] = tr
					end
				end
				local batch = anicli.http.get_batch(urls, MAX_TRANSLATION_PARALLEL)
				for i, res in ipairs(batch) do
					if not res.error then
						add_candidate(metas[i], parse_serial_page(res.body))
					end
				end
				embeds, resolved_dub = walk()
			end

			if not embeds then
				-- the ONLY dub wall that remains: NO dub carries this
				-- episode at all — a data-shape fact, not a caller
				-- mistake.
				anicli.fail("not_found", "episode " .. num .. " carries no dub from any of its " ..
					#main.translations .. " translations")
			end
		end

		local ok, links = pcall(anicli.extract, embeds)
		if not ok then
			anicli.fail("extract_failed", tostring(links))
		end
		return { dub_name = resolved_dub, links = links }
	end,
}
