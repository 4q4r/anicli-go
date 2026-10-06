-- HDRezka (rezka-ua.tv) — the RU rezka family's anime section, the
-- PR141 Lua migration of the compiled Go provider, and the roster's
-- HYBRID: the scraping lives here, the Anubis proof-of-work gate
-- solves in pure Go through anicli.solve_anubis (internal/anubis —
-- the sha256 ladder the compiled provider owned since PR69; porting
-- the hash loop into a sandbox with no bit library would be neither
-- honest nor fast). The wire shapes (the frozen anicli-api hdrezka
-- port, live-verified 2026-09-19; the gate re-verified live
-- 2026-10-06 at Anubis v1.27.0 — the sha256 PoW contract is stable):
--
--   - search: GET /search/?do=search&subaction=search&q=<q> answers
--     SSR .b-content__inline_item cards; ONLY cards whose data-url
--     carries /animation/ are results (the site mixes films and TV
--     series into the same listing). Titles compose like the
--     reference: card title + " " + span.info (a trailing space when
--     the card carries no info line). The query is percent-encoded by
--     py_quote (spaces %20, one UTF-8 byte at a time — never the
--     form-style "+" of the SDK's query_escape; the anidub precedent).
--   - episodes: the /animation/ page carries the favs token
--     (input#ctrl_favs), the player bootstrap id/translator_id (the
--     sof.tv.initCDN{Series,Movies}Events argument list), the
--     translator roster (#translators-list li.b-translator__item,
--     title attributes verbatim — the live UA-translator title ends
--     in a space) and SSR episode items (.b-simple_episode__item).
--     A page WITHOUT episode items is a movie: one synthetic episode
--     whose payloads carry the get_movie fields. Every payload is
--     self-sufficient: streams never refetches the page.
--   - streams: POST /ajax/get_cdn_series/?t=<unix-40> with the
--     payload's id/translator_id/season/episode/favs/action form (the
--     reference ts is SECONDS minus 40; the server ignores the value,
--     the presence is ported). The success answer's url field is the
--     comma-separated "[Qp (Ultra)?]URL or URL,..." list — first URL
--     per quality wins the quality key, mp4/m3u8 by extension, the
--     page_url rides back as the stream Referer. success:false fails
--     loud with the server message; the REAL url:false shape (the
--     site serves pages but withholds stream links from
--     stream-refusing exits — geo/premium, live-verified) fails loud
--     with geo_blocked where the upstream reference CRASHES.
--   - the Anubis ladder (every leg, on the marker): extract the
--     anubis_challenge JSON blob, solve it through the Go binding,
--     round-trip /.within.website/x/cmd/anubis/api/pass-challenge
--     (id/response/nonce/redir/elapsedTime + the page Referer) and
--     retry the original request exactly once — the auth cookie rides
--     the wired netclient's jar. A still-challenged retry fails loud.
--
-- Mirror note: the family geo-fences per domain (hdrezka-home.tv
-- withholds stream links from datacenter exits; rezka-ua.tv serves
-- them — the PR72 route matrix). The literal below pins the serving
-- mirror; [providers.hdrezka] base_url re-points every leg through
-- anicli.provider_setting("base_url") (the PR140 seam) when the
-- family rotates again.
--
-- Contract shifts vs the compiled provider (the kodik/anidub
-- precedent): the per-(episode, dub) payload map rides episode raw_id
-- as JSON {[dub]: payload} — the fresh-sandbox streams(raw_id, dub)
-- state channel; the span.info text trims (the html:text contract).

local base_url = anicli.provider_setting("base_url")
if not base_url or base_url == "" then
	base_url = "https://rezka-ua.tv"
end

-- ANUBIS_MARKER detects the challenge page: the first request of any
-- operation (and any later one after the auth cookie expires) is
-- answered with it instead of the real content.
local ANUBIS_MARKER = "<script id=\"anubis_challenge\""

-- py_quote ports urllib.parse.quote with its default safe="/" set
-- (the compiled provider's pyQuote behavior; the anidub script's
-- derivation).
local function py_quote(s)
	local out = {}
	for i = 1, #s do
		local c = string.sub(s, i, i)
		if string.match(c, "^[A-Za-z0-9%-%.%~%/_]$") then
			out[#out + 1] = c
		else
			out[#out + 1] = string.format("%%%02X", string.byte(s, i))
		end
	end
	return table.concat(out)
end

local function trim(s)
	local t = string.gsub(s, "^%s+", "")
	return (string.gsub(t, "%s+$", ""))
end

-- unix_now seconds from the SDK's RFC 3339 UTC stamp: the days-from-
-- civil arithmetic (Hinnant) — the sandbox opens no os library, and
-- the cache-buster's only wire contract is an honest integer.
local function unix_now()
	local y, mo, d, hh, mm, ss = string.match(anicli.time.now(),
		"(%d%d%d%d)%-(%d%d)%-(%d%d)T(%d%d):(%d%d):(%d%d)")
	y, mo, d, hh, mm, ss = tonumber(y), tonumber(mo), tonumber(d), tonumber(hh), tonumber(mm), tonumber(ss)
	if mo <= 2 then
		y = y - 1
	end
	local era = math.floor(y / 400)
	local yoe = y - era * 400
	local doy = math.floor((153 * (mo + (mo > 2 and -3 or 9)) + 2) / 5) + d - 1
	local doe = yoe * 365 + math.floor(yoe / 4) - math.floor(yoe / 100) + doy
	local days = era * 146097 + doe - 719468
	return days * 86400 + hh * 3600 + mm * 60 + ss
end

-- anubis_pass solves the challenge the page carries and completes the
-- pass-challenge round-trip; the auth cookie lands in the transport
-- jar. elapsedTime is the honest Go solve duration (the server only
-- logs it).
local function anubis_pass(body, original_url)
	local m = anicli.regexp.match(
		"(?s)<script id=\"anubis_challenge\" type=\"application/json\">(.*?)</script>", body)
	if not m then
		anicli.fail("provider_403", "anubis challenge page without challenge JSON")
	end
	local challenge = anicli.json.decode(m[2])
	local solution = anicli.json.decode(anicli.solve_anubis(m[2]))
	local pass = base_url ..
		"/.within.website/x/cmd/anubis/api/pass-challenge?id=" .. challenge.challenge.id ..
		"&response=" .. solution.digest ..
		"&nonce=" .. tostring(solution.nonce) ..
		"&redir=" .. original_url ..
		"&elapsedTime=" .. tostring(solution.elapsed_ms)
	anicli.http.get(pass, { headers = { Referer = original_url } })
end

-- with_anubis runs one request leg; an Anubis challenge answer is
-- solved and the ORIGINAL request retried exactly once. A
-- still-challenged retry fails loud — the gate refused us even after
-- an honest solve.
local function with_anubis(url, do_request)
	local resp = do_request()
	if not string.find(resp.body, ANUBIS_MARKER, 1, true) then
		return resp
	end
	anubis_pass(resp.body, url)
	local retry = do_request()
	if string.find(retry.body, ANUBIS_MARKER, 1, true) then
		anicli.fail("provider_403",
			"anubis gate did not clear after a valid proof-of-work solve")
	end
	return retry
end

return {
	id = "hdrezka",
	name = "HDRezka",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",

	search = function(query)
		local url = base_url .. "/search/?do=search&subaction=search&q=" .. py_quote(query)
		local resp = with_anubis(url, function()
			return anicli.http.get(url)
		end)

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find(".b-content__inline_item"):each(function(_, item)
			local data_url = item:attr("data-url")
			if not string.find(data_url, "/animation/", 1, true) then
				return
			end
			local link = item:find(".b-content__inline_item-link a")
			if link:len() == 0 then
				return
			end
			local href = link:attr("href")
			if href == "" then
				return
			end
			-- Reference composition, verbatim: title + " " + span.info
			-- (the info line is NOT stripped — hdrezka_parser.py
			-- _parse_season returns the raw text; the html:text trim is
			-- the documented shift).
			local title = link:text()
			local info = item:find("span.info")
			local season = ""
			if info:len() > 0 then
				season = info:text()
			end
			local poster = ""
			local img = item:find("img[src]")
			if img:len() > 0 then
				poster = img:attr("src")
			end
			results[#results + 1] = { title = title .. " " .. season, url = href, poster = poster }
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = with_anubis(anime_url, function()
			return anicli.http.get(anime_url)
		end)
		local doc = anicli.html.parse(resp.body)

		local title = doc:find(".b-post__title h1"):text()
		local favs = ""
		local favs_sel = doc:find("input#ctrl_favs[value]")
		if favs_sel:len() > 0 then
			favs = favs_sel:attr("value")
		end

		-- The init script: the first <script> mentioning the CDN
		-- bootstrap call — id and default translator_id live in its
		-- argument list.
		local init_script
		doc:find("script"):each(function(_, s)
			if init_script then
				return
			end
			local text = s:text()
			if string.find(text, "sof.tv.initCDNSeriesEvents", 1, true)
				or string.find(text, "sof.tv.initCDNMoviesEvents", 1, true) then
				init_script = text
			end
		end)
		local player_id, default_tr = "", ""
		if init_script then
			local mi = anicli.regexp.match("initCDN(?:Series|Movies)Events\\(\\s*(\\d+)", init_script)
			if mi then
				player_id = mi[2]
			end
			local mt = anicli.regexp.match("initCDN(?:Series|Movies)Events\\(\\s*\\d+\\s*,\\s*(\\d+)", init_script)
			if mt then
				default_tr = mt[2]
			end
		end

		if favs == "" or player_id == "" then
			anicli.fail("extract_failed",
				"anime page without the favs token or player id (not an /animation/ page?)")
		end

		local dubs = {}
		doc:find("#translators-list > li.b-translator__item"):each(function(_, li)
			dubs[#dubs + 1] = { title = li:attr("title"), id = li:attr("data-translator_id") }
		end)
		if #dubs == 0 then
			-- The single site default (hdrezka.py
			-- _get_series_sources/_get_movie_sources).
			dubs[1] = { title = "hdrezka", id = default_tr }
		end

		local eps = {}
		doc:find(".b-simple_episodes__list .b-simple_episode__item"):each(function(_, li)
			eps[#eps + 1] = {
				id = li:attr("data-id"),
				season = li:attr("data-season_id"),
				episode = li:attr("data-episode_id"),
				title = li:text(),
			}
		end)

		-- payload_for builds one (episode, dub) request payload; the
		-- per-dub map rides raw_id as JSON (the fresh-sandbox streams
		-- state) and mirrors into raw_embeds for consumers.
		local function payloads_for(ep)
			local payloads, embeds = {}, {}
			for _, dub in ipairs(dubs) do
				local payload
				if ep == nil then
					payload = {
						id = player_id,
						translator_id = dub.id,
						favs = favs,
						action = "get_movie",
						page_url = anime_url,
					}
				else
					payload = {
						id = ep.id,
						translator_id = dub.id,
						season = ep.season,
						episode = ep.episode,
						favs = favs,
						action = "get_stream",
						page_url = anime_url,
					}
				end
				local raw = anicli.json.encode(payload)
				payloads[dub.title] = raw
				embeds[dub.title] = { raw }
			end
			return anicli.json.encode(payloads), embeds
		end

		local out = {}
		if #eps == 0 then
			-- Movie: one synthetic episode (reference: data_id = page
			-- id, season/episode absent, is_movie).
			local state, embeds = payloads_for(nil)
			out[1] = { num = "1", title = title, raw_id = state, raw_embeds = embeds }
			return out
		end
		for i, ep in ipairs(eps) do
			local state, embeds = payloads_for(ep)
			out[i] = { num = ep.episode, title = ep.title, raw_id = state, raw_embeds = embeds }
		end
		return out
	end,

	streams = function(raw_id, dub)
		local stream = { dub_name = dub, links = {} }
		if raw_id == "" then
			anicli.fail("not_found", "no stream payload for dub \"" .. dub .. "\"")
		end
		local payloads = anicli.json.decode(raw_id)
		if type(payloads) ~= "table" or payloads[dub] == nil then
			anicli.fail("not_found", "no stream payload for dub \"" .. dub .. "\"")
		end
		local payload = anicli.json.decode(payloads[dub])

		local parts = {
			"id=" .. anicli.http.query_escape(payload.id),
			"translator_id=" .. anicli.http.query_escape(payload.translator_id),
			"favs=" .. anicli.http.query_escape(payload.favs),
			"action=" .. anicli.http.query_escape(payload.action),
		}
		if (payload.season ~= nil and payload.season ~= "")
			or (payload.episode ~= nil and payload.episode ~= "") then
			parts[#parts + 1] = "season=" .. anicli.http.query_escape(payload.season or "")
			parts[#parts + 1] = "episode=" .. anicli.http.query_escape(payload.episode or "")
		end

		-- Reference cache-buster: unix SECONDS minus 40 (the site JS
		-- sends millis; the server ignores the value — the reference
		-- shape is ported).
		local url = base_url .. "/ajax/get_cdn_series/?t=" .. tostring(unix_now() - 40)
		local referer = payload.page_url
		if referer == nil or referer == "" then
			referer = base_url
		end
		local resp = with_anubis(url, function()
			return anicli.http.post(url, table.concat(parts, "&"),
				"application/x-www-form-urlencoded; charset=UTF-8",
				{
					headers = {
						Accept = "application/json, text/javascript, */*; q=0.01",
						["X-Requested-With"] = "XMLHttpRequest",
						Origin = base_url,
						Referer = referer,
					},
				})
		end)

		local cdn = anicli.json.decode(resp.body)
		if not cdn.success then
			local msg = cdn.message
			if msg == nil or msg == "" then
				msg = "get_cdn_series refused without a message"
			end
			anicli.fail("extract_failed", msg)
		end
		-- url:false (or null) — the site answered success but serves no
		-- links: the stream-refusing-exit shape [LIVE-VERIFIED].
		if type(cdn.url) ~= "string" or cdn.url == "" then
			anicli.fail("geo_blocked", "site served no stream links (geo/premium refusal)")
		end

		for part in string.gmatch(cdn.url, "([^,]+)") do
			local qm = anicli.regexp.match("\\[.*?(\\d+).*?\\]", part)
			local idx = string.find(part, "]", 1, true)
			if qm and idx then
				local url_part = string.sub(part, idx + 1)
				-- " or " alternates (hls/mp4 twins): the first URL per
				-- quality wins the quality-keyed map (the Go contract's
				-- documented divergence from the reference's list).
				local alt = string.find(url_part, " or ", 1, true)
				local found = url_part
				if alt then
					found = string.sub(url_part, 1, alt - 1)
				end
				found = trim(found)
				if found ~= "" and stream.links[qm[2]] == nil then
					local kind = "m3u8"
					if string.sub(found, -4) == ".mp4" then
						kind = "mp4"
					end
					stream.links[qm[2]] = {
						url = found,
						quality = qm[2],
						type = kind,
						headers = { Referer = referer },
					}
				end
			end
		end
		if next(stream.links) == nil then
			anicli.fail("extract_failed",
				"no parsable stream urls in \"" .. cdn.url .. "\"")
		end
		return stream
	end,
}
