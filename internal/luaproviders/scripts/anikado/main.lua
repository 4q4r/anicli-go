-- AniKado (anikado.net) — the PR133 Lua migration of the compiled Go
-- provider (internal/providers/anikado.go, PR102, written against the
-- live DLE install and re-verified live 2026-10-06 through the direct
-- route: the search POST answers «черная лагуна» with 2 cards in
-- 0.73s, the title page renders the 12 server-side episode anchors,
-- the episode page serves the 4-translator kodik table and the movie
-- title keeps its kodik /video/ tab embed). The observed shapes:
--
--   - search: the DLE search form POST (do=search&subaction=search&
--     story=…) to /index.php?do=search. Results render server-side as
--     article.card elements inside #dle-content (10 per page); a junk
--     query answers the same shell with zero cards — an empty list,
--     not an error. No extra headers [LIVE-VERIFIED 2026-09-25: POST
--     and GET answer identically; the form method is POST]. Posters
--     are site-relative uploads paths prefixed with the site root.
--   - episodes: the title page renders the whole episode list
--     server-side as .flex-episodes-links anchors (…/episode-N.html;
--     verified up to 52 anchors, no pagination). Every episode page
--     carries the per-(episode, dub) kodik embed table as
--     li.b-translator__item rows (data-this_translator name,
--     data-this_link embed), so the listing fans out one fetch per
--     episode bounded-parallel through http.get_batch (the
--     kickassanime pattern). Movies: no anchors — the active kodik
--     tab's iframe (#kodik-player / .player-content) is the single
--     episode under the service dub name (the AniMedia service-dub
--     ruling).
--   - dubs: the b-translator__item names verbatim (including the
--     «Субтитры» entries kodik serves as translations).
--   - streams: the kodik embeds resolve through anicli.extract (the
--     shared extractor factory). Fresh-sandbox state (the raw_id
--     channel): series episodes carry the {n, u} page-state JSON —
--     the resolve re-fetches the episode page and rebuilds the
--     translator table (+1 fetch per resolve, the animedia rule);
--     movies carry the {n, e} embed-state JSON — the /video/ embed is
--     fully determined at listing time, so the movie resolve stays a
--     zero-fetch extract, the compiled behavior kept.
--   - embed normalization: protocol-relative srcs gain the https
--     scheme, and the kodik.info embed host is normalized onto the
--     interchangeable kodikplayer.com mirror (same /seria/ path
--     answers 200 with the hash-consistent player page,
--     LIVE-VERIFIED 2026-09-25). Everything after the host — path and
--     query, including the site's doubled ?hide_selectors=true quirk
--     — passes through verbatim.
--
-- Typed walls (kept from the compiled provider):
--   - a title page with neither episode anchors nor a kodik tab
--     iframe is not_found (an empty list would fake a healthy title
--     with no episodes);
--   - an episode page without translator rows is extract_failed
--     naming the episode (the site's own player would render an empty
--     iframe there);
--   - a dub the episode does not carry is invalid_input (a caller
--     bug); an embed no extractor yields links for is extract_failed
--     (a non-kodik embed such as the tomion tab's host, which also
--     404s outside its frame context).
--
-- Known walls, documented not hidden: the title page's two fallback
-- player tabs are NOT resolvable anonymously — the vkg tab is a
-- client-side hydrated <video-player> fed by the mali aggregator, and
-- the tomion tab's frame-gated embed 404s outside its iframe.
--
-- Parallelism: the compiled provider bounded its episode-page fan-out
-- with config network.max_parallel; the script cannot read the config
-- and get_batch clamps ≤0 to 1, so the bound (the config default, 4)
-- is pinned locally — the kickassanime review-F3 precedent.
-- get_batch cannot carry custom headers; every anikado leg answers
-- plain anonymous GETs (no headers anywhere on the wire), so nothing
-- is lost.
--
-- Route: every leg answers the direct route on the characterization
-- network (re-verified 2026-10-06: search, title, episode and movie
-- pages all 200 in ~0.73s) — no proxy class; the script is
-- route-agnostic.

local base_url = "https://anikado.net"

-- SERVICE_DUB is the dub name for titles with exactly one unnamed
-- kodik source (movies: the /video/ embed in the active kodik tab, no
-- episode pages, no translator list).
local SERVICE_DUB = "AniKado"

-- The fan-out bound: the config default for network.max_parallel, out
-- of script reach — pinned, never read (see the header).
local MAX_EPISODE_PARALLEL = 4

-- The episode-page href tail ("/episode-12.html"); the capture group
-- is the episode number. The dash is a literal — in Lua patterns a
-- bare "-" after a character is the lazy quantifier, so it escapes.
local EPISODE_NUM_PAT = "/episode%-(%d+)%.html$"

local function trim(s)
	return (string.match(s or "", "^%s*(.-)%s*$"))
end

-- normalize_embed turns a captured player embed into the URL the
-- shared extractor factory consumes: protocol-relative srcs gain the
-- https scheme, and the kodik.info embed host is normalized onto the
-- interchangeable kodikplayer.com mirror. Everything after the host —
-- path and query — passes through verbatim.
local function normalize_embed(src)
	src = string.gsub(src or "", "^//kodik%.info/", "//kodikplayer.com/")
	if string.sub(src, 1, 2) == "//" then
		src = "https:" .. src
	end
	return src
end

-- collect_translator_rows folds one page's b-translator__item rows
-- into the (dub → embeds) map: names verbatim (including the
-- «Субтитры» entries), same-named translators append, empty names or
-- links drop.
local function collect_translator_rows(doc)
	local embeds = {}
	doc:find("li.b-translator__item[data-this_link][data-this_translator]"):each(function(_, li)
		local name = trim(li:attr("data-this_translator"))
		local link = trim(li:attr("data-this_link"))
		if name == "" or link == "" then
			return
		end
		local list = embeds[name]
		if list == nil then
			list = {}
			embeds[name] = list
		end
		list[#list + 1] = normalize_embed(link)
	end)
	return embeds
end

-- episode_from_page builds one episode row from a fetched episode
-- page: the listing num, the {n, u} streams state and the dub table.
-- A page with no translator rows is the typed structural break — the
-- site's own player renders an empty iframe there.
local function episode_from_page(num, page_url, body)
	local embeds = collect_translator_rows(anicli.html.parse(body))
	if next(embeds) == nil then
		anicli.fail("extract_failed", "episode " .. num .. " page carries no translator rows")
	end
	return {
		num = num,
		raw_id = anicli.json.encode({ n = num, u = page_url }),
		raw_embeds = embeds,
	}
end

return {
	id = "anikado",
	name = "AniKado",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	-- No smoke_query declaration: the shared RU smoke probe
	-- («черная лагуна») surfaces this catalog — 2 real hits,
	-- live-verified — and a declared probe would override the shared
	-- one for no reason.

	search = function(query)
		local form = "do=search&subaction=search&story=" .. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. "/index.php?do=search", form, "application/x-www-form-urlencoded")

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find("#dle-content article.card"):each(function(_, card)
			local link = card:find("a.card__img[href]")
			local title = card:find("h2.card__title a")
			if link:len() == 0 or title:len() == 0 then
				return
			end
			local href = link:attr("href")
			local name = title:text()
			if href == "" or name == "" then
				return
			end
			local poster = ""
			local img = card:find("img[src]")
			if img:len() > 0 then
				local src = img:attr("src")
				if src ~= "" then
					poster = base_url .. src -- site-relative uploads path
				end
			end
			results[#results + 1] = { title = name, url = href, poster = poster }
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local doc = anicli.html.parse(resp.body)

		-- The title-page anchors, in document order (the site renders
		-- them ascending). Movies have none.
		local refs, seen = {}, {}
		doc:find(".flex-episodes-links a[href]"):each(function(_, anchor)
			local href = anchor:attr("href")
			local num = string.match(href, EPISODE_NUM_PAT)
			if num ~= nil and not seen[num] then
				seen[num] = true
				refs[#refs + 1] = { num = num, page = href }
			end
		end)

		if #refs == 0 then
			-- Movie path: the active kodik tab's iframe is the single
			-- episode under the service dub name. No kodik tab iframe
			-- at all is the typed wall (an empty list here would fake
			-- a healthy title with no episodes).
			local frame = doc:find("#kodik-player iframe[src], .player-content iframe[src]")
			if frame:len() == 0 then
				anicli.fail("not_found", "title page carries no episode anchors and no player-tab iframe: " ..
					tostring(anime_url))
			end
			local embed = normalize_embed(frame:attr("src"))
			return {
				{
					num = "1",
					raw_id = anicli.json.encode({ n = "1", e = embed }),
					raw_embeds = { [SERVICE_DUB] = { embed } },
				},
			}
		end

		-- Series: one bounded-parallel fetch per episode page; each
		-- page's translator rows become that episode's dub table. A
		-- failed page fetch fails loud — the compiled fan-out errored
		-- the whole walk, never a silent partial list.
		local urls = {}
		for _, ref in ipairs(refs) do
			urls[#urls + 1] = ref.page
		end
		local batch = anicli.http.get_batch(urls, MAX_EPISODE_PARALLEL)
		local episodes = {}
		for i, res in ipairs(batch) do
			if res.error ~= nil then
				anicli.fail("extract_failed", "episode " .. refs[i].num .. " page: " .. res.error)
			end
			episodes[#episodes + 1] = episode_from_page(refs[i].num, refs[i].page, res.body)
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local state = anicli.json.decode(raw_id)
		local num = tostring(state.n)

		-- Movies: the embed-state resolve — the /video/ embed rides
		-- the state alone, nothing is fetched (the compiled
		-- zero-fetch movie resolve kept).
		if state.e ~= nil and state.e ~= "" then
			if dub ~= SERVICE_DUB then
				anicli.fail("invalid_input", "episode " .. num .. " carries no dub \"" .. dub .. "\"")
			end
			local ok, links = pcall(anicli.extract, { state.e })
			if not ok then
				anicli.fail("extract_failed", tostring(links))
			end
			return { dub_name = dub, links = links }
		end

		-- Series: re-fetch the episode page named in the state and
		-- rebuild the translator table (the fresh-sandbox contract).
		local embeds = collect_translator_rows(anicli.html.parse(anicli.http.get(state.u).body))
		local links = embeds[dub]
		if links == nil or #links == 0 then
			-- A dub the episode does not carry is a caller bug (the
			-- typed ErrInvalidInput semantics).
			anicli.fail("invalid_input", "episode " .. num .. " carries no dub \"" .. dub .. "\"")
		end

		local ok, sources = pcall(anicli.extract, links)
		if not ok then
			-- No extractor yielded links (a non-kodik embed such as
			-- the tomion tab's host): the typed extract wall.
			anicli.fail("extract_failed", tostring(sources))
		end
		return { dub_name = dub, links = sources }
	end,
}
