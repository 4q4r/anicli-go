-- GogoAnime (anitaku.io) — the PR128 Lua migration of the compiled Go
-- provider (internal/providers/gogoanime.go). The gogoanime platform
-- rebranded to Anitaku (Kohi-den extensions-source issue #410): a
-- WordPress site running the DramaStream theme, characterized and
-- live-verified 2026-09-18, re-verified live 2026-10-06 through the
-- configured proxy (the honest route on the characterization network —
-- the standard per-provider netclient wiring carries it).
--
-- Shapes:
--   - search: POST /wp-admin/admin-ajax.php with the form fields
--     action=ts_ac_do_search, ts_ac_query=<query> (the theme's
--     autocomplete ajax — the WordPress /?s= form 301s to /browse/ and
--     silently ignores the query). The answer is
--     {"series":[{"all":[{post_title, post_image, post_link, …}]}]};
--     every series.all group contributes its entries and the absolute
--     /series/ hrefs stay verbatim. The zero-hit case is all:[] plus
--     the render template — an empty list, not an error.
--   - episodes: the series page renders the .eplister grid
--     newest-first (the live One Piece page carries the newest ~86
--     entries; the site exposes no older-episode ajax). The listing
--     reverses to ascending, the order the legacy ajax path produced.
--     The Lua provider contract has no DubsHydrator capability, so the
--     per-episode mirror lists hydrate EAGERLY — one bounded-parallel
--     get_batch leg per episode page (the yummy/animedia/anikoto
--     precedent); a dead leg leaves that episode's embeds empty and
--     the resolve re-fetches the page fresh.
--   - dubs/mirrors: each episode page stores every server as
--     base64-encoded iframe HTML in a select.mirror option's value
--     attribute (the theme's loadMi does atob(value) into #pembed).
--     The leading "Select Video Server" placeholder carries an empty
--     value and is skipped, as are options that do not decode to an
--     iframe src; the live option labels are blank, so servers land
--     under the "Unknown" slot — the convention for unlabeled servers.
--   - streams: the fresh-sandbox contract (raw_id + dub only)
--     re-fetches the episode page from raw_id, re-parses the mirror
--     options and resolves the chosen server's embeds through
--     anicli.extract — the shared extractor factory whose blogger
--     (video.g → batchexecute progressive mp4) and gogoplay
--     (encrypt-ajax → m3u8) families cover the mirror pool.
--
-- Header notes (deviations documented, live-verified 2026-10-06):
--   - The compiled provider sent Referer: <site root> on every leg (a
--     PR5 task ruling). The SDK http.post carries no custom headers,
--     so the search POST rides bare; the page GETs keep the Referer.
--     The site answers all three surfaces identically without it.
--   - The episode-page hydration legs ride get_batch (the bridge sends
--     no custom headers): the pages answer without the Referer, and
--     ~86 sequential fetches per listing would blow the smoke budget.

local base_url = "https://anitaku.io"

-- The search ajax endpoint (the DramaStream autocomplete action).
local SEARCH_AJAX = "/wp-admin/admin-ajax.php"

-- The bounded-parallel hydration width (the animevib precedent: a
-- polite fan, never unbounded — netclient.Parallel treats <=0 as
-- unbounded and that must not leak into scripts).
local HYDRATION_PARALLEL = 6

local function trim(s)
	return (string.gsub(string.gsub(s, "^%s+", ""), "%s+$", ""))
end

-- absolute joins a page href the way the compiled provider did: refs
-- already carrying a scheme stay, bare paths ride the site root.
local function absolute(ref)
	if string.sub(ref, 1, 4) == "http" then
		return ref
	end
	return base_url .. ref
end

-- parse_mirrors folds an episode page into {[serverName] = {urls…}}:
-- one entry per select.mirror option whose value decodes to an iframe
-- src, document order preserved; blank labels land under "Unknown".
-- The compiled provider skipped placeholders, undecodable values and
-- src-less frames silently — the same soft skips run here.
local function parse_mirrors(body)
	local embeds = {}
	anicli.html.parse(body):find("select.mirror option[value]"):each(function(_, option)
		local encoded = option:attr("value")
		if encoded == "" then
			return
		end
		local ok, decoded = pcall(anicli.base64.decode, encoded)
		if not ok then
			return
		end
		local src = trim(anicli.html.parse(decoded):find("iframe"):attr("src"))
		if src == "" then
			return
		end
		local server = trim(option:text())
		if server == "" then
			server = "Unknown"
		end
		local list = embeds[server] or {}
		list[#list + 1] = src
		embeds[server] = list
	end)
	return embeds
end

return {
	id = "gogoanime",
	name = "GogoAnime",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ja",
	-- The Anitaku live-search index stopped surfacing the shared probes
	-- (2026-09-18: {"all":[]} for черная лагуна / black lagoon) while
	-- this broad hit keeps returning its results.
	smoke_query = "one piece",

	search = function(query)
		local form = "action=ts_ac_do_search&ts_ac_query="
			.. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. SEARCH_AJAX, form,
			"application/x-www-form-urlencoded")
		local data = anicli.json.decode(resp.body)
		local results = {}
		for _, group in ipairs(data.series or {}) do
			for _, item in ipairs(group.all or {}) do
				if (item.post_link or "") ~= "" then
					results[#results + 1] = {
						title = item.post_title or "",
						url = item.post_link,
						poster = item.post_image or "",
					}
				end
			end
		end
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(absolute(anime_url),
			{ headers = { Referer = base_url } })
		local items = {}
		anicli.html.parse(resp.body):find(".eplister li"):each(function(_, item)
			local href = trim(item:find("a"):attr("href"))
			if href == "" then
				return
			end
			local num = trim(item:find(".epl-num"):text())
			if num == "" then
				num = "0"
			end
			local title = trim(item:find(".epl-title"):text())
			if title == "" then
				title = "Episode " .. num
			end
			items[#items + 1] = { num = num, title = title, href = href }
		end)

		-- The grid renders newest-first; walk it back-to-front so the
		-- list ascends like the legacy ajax path produced.
		local episodes = {}
		for i = #items, 1, -1 do
			episodes[#episodes + 1] = {
				num = items[i].num,
				title = items[i].title,
				raw_id = items[i].href,
				raw_embeds = {},
			}
		end

		-- Eager mirror hydration (no DubsHydrator in the Lua contract —
		-- the yummy/animedia/anikoto precedent): one bounded-parallel
		-- leg per episode page; a dead leg leaves that episode's embeds
		-- empty (the resolve re-fetches fresh and fails loudly there).
		local urls = {}
		for _, ep in ipairs(episodes) do
			urls[#urls + 1] = absolute(ep.raw_id)
		end
		local batch = anicli.http.get_batch(urls, HYDRATION_PARALLEL)
		for i, res in ipairs(batch) do
			if not res.error and res.status == 200 then
				episodes[i].raw_embeds = parse_mirrors(res.body)
			end
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local page = anicli.http.get(absolute(raw_id),
			{ headers = { Referer = base_url } })
		local links = parse_mirrors(page.body)[dub]
		if links == nil or #links == 0 then
			anicli.fail("invalid_input",
				"dub \"" .. dub .. "\" carries no mirrors to resolve")
		end
		-- The shared factory resolves every embed family and raises
		-- typed when nothing resolved — the no-silent-failure rule the
		-- compiled provider pinned at factorySources.
		local sources = anicli.extract(links)
		return { dub_name = dub, links = sources }
	end,
}
