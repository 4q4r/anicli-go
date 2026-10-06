-- SameBand (sameband.studio) — the RU DLE catalog of the SameBand
-- dubbing studio, the PR131 Lua migration of the compiled Go provider
-- (itself a port of anicli-py anicli/providers/sameband.py). The
-- observed shapes:
--
--   - search: the DLE search form POST (do=search&subaction=search&
--     story=…) to /index.php?do=search — the results render
--     server-side as shortstory cards (.col-auto / .image[href] /
--     .poster[title] / img.swiper-lazy); no client-side filtering and
--     no extra headers (LIVE-VERIFIED 2026-09-18: HTTP 200, no
--     Cloudflare challenge, no Referer needed, junk query → 0 cards).
--     The poster src is ALWAYS prefixed with the site root, even when
--     already absolute — the Python original's verbatim quirk
--     (sameband.py:44).
--   - episodes: the anime page embeds the player in an iframe
--     (.player > .player-content > iframe[src]); the player page's
--     Playerjs bootstrap (possibly retyped by a Cloudflare Rocket
--     Loader wrapper) carries file:"<playlist>" — captured with the
--     same [^>] class regex as the Python original, which prose
--     naming file:"..." cannot fool (any ">" breaks the match). The
--     playlist URL may carry RAW SPACES (the HTTP client fetches it
--     percent-encoded). The playlist is a Playerjs JSON ARRAY of
--     {title?, file} entries; titles are raw HTML blobs
--     (poster/duration markup) kept verbatim; a missing or null title
--     falls back to "Episode N" (the 1-based index, Python
--     item.get semantics).
--   - streams: the file field is a comma-joined quality map
--     ("[480p]url,[720p]url,…"). The raw field rides episode RawID
--     (the fresh-sandbox streams(raw_id, dub) state channel — the
--     animeheaven/anikoto precedent), so the resolve is pure string
--     mapping with no network, exactly like the Go original reading
--     RawEmbeds. Relative paths get the site root prefixed; every
--     source is m3u8 and carries Referer: <site root> so mpv can play
--     it (the PR5 Go ruling; the Python original left headers empty).
--
-- Typed walls (the PR47 task ruling, kept from the Go port): a
-- missing player iframe is not_found; a player page without a
-- Playerjs file field and a non-JSON playlist are extract_failed —
-- the Python original silenced exactly these breaks (its bare except
-- hid the 2026-09 API outage until the smoke run caught it).

local base_url = "https://sameband.studio"

-- DUB is the single dub name every episode carries (the studio's own
-- dubs; the Go provider's RawEmbeds key).
local DUB = "SameBand"

local function abs(u)
	if u == nil or u == "" then
		return ""
	end
	if string.sub(u, 1, 4) == "http" then
		return u
	end
	return base_url .. u
end

-- The Playerjs file field. Go regexp syntax through anicli.regexp
-- (identical pattern to the Go port, same RE2 semantics): the [^>]
-- capture class cannot cross a tag boundary, so the Rocket Loader
-- retyped script tag still matches while prose cannot.
local PLAYERJS_FILE_RE = "Playerjs[^>]+file:\\s*[\"']([^>]+)[\"']"

-- One "[NNNp]<url>" part of a file field (sameband.py:89).
local QUALITY_RE = [[^\[(\d+)p\](.*)]]

return {
	id = "sameband",
	name = "SameBand",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	smoke_query = "дьявол",

	search = function(query)
		local form = "do=search&subaction=search&story=" .. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. "/index.php?do=search", form, "application/x-www-form-urlencoded")

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find(".col-auto"):each(function(_, item)
			local link = item:find(".image[href]")
			local poster = item:find(".poster[title]")
			if link:len() == 0 or poster:len() == 0 then
				return
			end
			local img = ""
			if item:find("img.swiper-lazy"):len() > 0 then
				img = base_url .. item:find("img.swiper-lazy"):attr("src")
			end
			results[#results + 1] = {
				title = poster:attr("title"),
				url = link:attr("href"),
				poster = img,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local doc = anicli.html.parse(resp.body)

		local iframe = doc:find(".player > .player-content > iframe[src]")
		if iframe:len() == 0 then
			anicli.fail("not_found", "player iframe not found on anime page " .. anime_url)
		end
		local player_url = abs(iframe:attr("src"))

		local player = anicli.http.get(player_url)
		local m = anicli.regexp.match(PLAYERJS_FILE_RE, player.body)
		if not m then
			anicli.fail("extract_failed", "player page has no Playerjs file field: " .. player_url)
		end
		local playlist_url = abs(m[2])

		local list = anicli.http.get(playlist_url)
		local ok, playlist = pcall(anicli.json.decode, list.body)
		if not ok then
			anicli.fail("extract_failed", "decode playlist " .. playlist_url .. ": " .. tostring(playlist))
		end

		local episodes = {}
		for i, item in ipairs(playlist) do
			local num = tostring(i)
			local title = item.title
			if title == nil then
				title = "Episode " .. num
			end
			episodes[#episodes + 1] = {
				num = num,
				title = title,
				raw_id = item.file or "",
				raw_embeds = { [DUB] = { item.file or "" } },
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local stream = { dub_name = dub, links = {} }
		if dub ~= DUB then
			return stream
		end
		for part in string.gmatch(raw_id, "[^,]+") do
			local q = anicli.regexp.match(QUALITY_RE, part)
			if q then
				stream.links[q[2]] = {
					url = abs(q[3]),
					quality = q[2],
					type = "m3u8",
					headers = { Referer = base_url },
				}
			end
		end
		return stream
	end,
}
