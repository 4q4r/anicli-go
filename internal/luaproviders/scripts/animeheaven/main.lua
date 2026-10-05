-- AnimeHeaven (animeheaven.me) — the EN sub-only direct-MP4 catalog,
-- the PR126 Lua migration of the compiled Go provider (itself written
-- against the live site and the AniVault scraper family, PR105,
-- live-verified 2026-09-25 and re-verified 2026-10-05). NOT behind
-- Cloudflare; anonymous. Three legs off the ONE base_url literal:
--
--   - search: GET /fastsearch.php?xhr=1&s=<query> (Accept:
--     text/html,*/*) → anchor cards a[href*="anime.php?"] whose id IS
--     the href query part (5-char base36-ish, e.g. 11t3p); title from
--     div.fastname (HTML entities decode in the parser), img[alt]
--     fallback; a junk query answers HTTP 200 «No results found» —
--     zero cards, an honest empty surface.
--   - episodes: GET /anime.php?<id> → a[onmouseover*="gateh("] /
--     a[onclick*="gatea("] anchors; the gate key is the quoted arg of
--     gateh/gatea. LIVE DELTA vs the reference scrapers: the real
--     markup single-quotes its attributes and puts a SPACE after the
--     paren — onmouseover='gateh( "150ade…")' — which the reference
--     regex gate[ha]\("([^"]+)" (no \s*) can no longer match; ours
--     tolerates both shapes. Episode number is the div.watch2 text
--     ("71"), rendered newest-first; sorted ascending, deduped by key.
--     A parsed page with zero gate anchors is the typed not_found wall
--     (the roster doctrine — an empty list would fake a healthy title
--     with no episodes).
--   - streams: GET /gate.php with Cookie: key=<episode key> (the
--     stateless gate — a cold jar with only that cookie answers) and
--     the site root as Referer → a <video> element of DIRECT mp4
--     <source>s. The FIRST /video.mp4 source is the playable edge
--     (HTTP 206 with Range support; the 2nd/3rd sources are the
--     site's own onerror-fallback CDNs, &error / &error2 — direct
--     hits answer 404); the rule degrades to the first http(s) source
--     (the reference `|| sources[0]` rule); no source is the typed
--     extract wall. NO anicli.extract leg — the links are direct mp4,
--     there is no iframe embed anywhere (the anilibria precedent).
--     The gate exposes no quality selector — the captured file's MP4
--     tkhd reports 928x720, so the single link is labelled 720.
--
-- The single dub name "Sub" keys every episode's raw_embeds: the site
-- is sub-only (JA audio, EN subs — no dub option anywhere on the
-- player) and the one unnamed source needs a stable key.
--
-- State passing: streams(raw_id, dub) receives the gate key as
-- raw_id — the only state the gate leg needs (no {n,u} payload; the
-- key alone re-opens the gate). An empty key or a foreign dub is the
-- typed invalid_input caller-bug wall (the wave-A review F2
-- precedent): a silent empty stream would read as a healthy
-- resolution.

local base_url = "https://animeheaven.me"

-- SERVICE_DUB is the one dub every episode carries (the site is
-- sub-only; the avServiceDub precedent).
local SERVICE_DUB = "Sub"

-- GATE_KEY_RE lifts the gate key out of an anchor's onmouseover /
-- onclick attribute; \s* after the paren tolerates the live
-- space-after-paren shape AND the reference no-space shape.
local GATE_KEY_RE = 'gate[ha]\\(\\s*"([^"]+)"'

local function trim(s)
	return (string.match(s or "", "^%s*(.-)%s*$"))
end

-- absolute resolves a site-relative src against the base_url root
-- (the reference absoluteUrl(url, BASE) semantics).
local function absolute(src)
	if src == "" then
		return ""
	end
	if string.sub(src, 1, 4) == "http" then
		return src
	end
	if string.sub(src, 1, 1) == "/" then
		return base_url .. src
	end
	return base_url .. "/" .. src
end

return {
	id = "animeheaven",
	name = "AnimeHeaven",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ja",
	name_preference = "latin",

	search = function(query)
		local search_url = base_url .. "/fastsearch.php?xhr=1&s=" .. anicli.http.query_escape(query)
		local resp = anicli.http.get(search_url, { headers = { Accept = "text/html,*/*" } })

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find('a[href*="anime.php?"]'):each(function(_, card)
			-- The id IS the href query part ("/anime.php?11t3p" → "11t3p").
			local raw_id = trim(string.match(card:attr("href"), "%?(.+)$"))
			if raw_id == nil or raw_id == "" then
				return
			end

			-- div.fastname text with the img[alt] fallback (the
			-- reference rule); a card with neither is skipped.
			local title = card:find(".fastname"):text()
			if title == "" then
				title = trim(card:find("img"):attr("alt"))
			end
			if title == "" then
				return
			end

			results[#results + 1] = {
				title = title,
				url = base_url .. "/anime.php?" .. raw_id,
				poster = absolute(trim(card:find("img"):attr("src"))),
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		-- The id IS the query part (the site's own addressing scheme);
		-- a URL without one is caller error.
		local raw_id = trim(string.match(anime_url, "%?(.+)$"))
		if raw_id == nil or raw_id == "" then
			anicli.fail("invalid_input", "anime url \"" .. anime_url .. "\" carries no id query part")
		end

		local resp = anicli.http.get(base_url .. "/anime.php?" .. raw_id)
		local doc = anicli.html.parse(resp.body)

		local seen, entries = {}, {}
		doc:find('a[onmouseover*="gateh("], a[onclick*="gatea("]'):each(function(_, el)
			-- onmouseover when present, onclick otherwise (the
			-- live anchors carry both, same key).
			local attr = el:attr("onmouseover")
			if trim(attr) == "" then
				attr = el:attr("onclick")
			end
			local m = anicli.regexp.match(GATE_KEY_RE, attr)
			local key = m and m[2] or ""
			if key == "" or seen[key] then
				return
			end
			-- div.watch2 text: digits only — animeheaven numbers are
			-- plain integers.
			local num = string.match(el:find(".watch2"):text(), "%d+")
			if num == nil then
				return
			end
			seen[key] = true
			entries[#entries + 1] = { key = key, num = num, ord = tonumber(num) }
		end)

		-- The page renders newest-first; the contract expects ascending.
		table.sort(entries, function(a, b)
			return a.ord < b.ord
		end)

		-- A parsed page with zero gate anchors is a typed wall: an
		-- empty list here would fake a healthy title with no episodes
		-- (the roster doctrine; this site's own space-after-paren
		-- drift shows the markup-shift case is real).
		if #entries == 0 then
			anicli.fail("not_found", "anime page carries no gate episode anchors")
		end

		local episodes = {}
		for _, e in ipairs(entries) do
			episodes[#episodes + 1] = {
				num = e.num,
				title = "Episode " .. e.num,
				raw_id = e.key,
				raw_embeds = { [SERVICE_DUB] = { e.key } },
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		-- A dub the episode does not carry (or a keyless episode) is a
		-- caller bug: a silent empty stream would read as a healthy
		-- resolution (the resolveAllStreams contract — TUI flows only
		-- pass keys present in raw_embeds, so this is the typed wall).
		if dub ~= SERVICE_DUB then
			anicli.fail("invalid_input", "episode carries no dub \"" .. dub .. "\"")
		end
		if raw_id == nil or raw_id == "" then
			anicli.fail("invalid_input", "episode carries no key for dub \"" .. dub .. "\"")
		end

		-- The stateless gate: a cold jar with only the key cookie
		-- answers; the Referer pins playback to the site origin (the
		-- edge answers without it today, but the header is
		-- load-bearing the moment that tightens — the anistar
		-- convention).
		local referer = base_url .. "/"
		local resp = anicli.http.get(base_url .. "/gate.php", {
			headers = {
				Cookie = "key=" .. raw_id,
				Referer = referer,
				Accept = "text/html,*/*",
			},
		})

		local doc = anicli.html.parse(resp.body)
		local primary, fallback = "", ""
		doc:find("video source"):each(function(_, el)
			local src = trim(el:attr("src"))
			if string.sub(src, 1, 7) ~= "http://" and string.sub(src, 1, 8) ~= "https://" then
				return
			end
			if fallback == "" then
				fallback = src
			end
			if primary == "" and string.find(src, "/video.mp4", 1, true) then
				primary = src
			end
		end)
		if primary == "" then
			primary = fallback
		end
		if primary == "" then
			anicli.fail("extract_failed", "gate page carries no playable source")
		end

		return {
			dub_name = dub,
			links = {
				["720"] = {
					url = primary,
					quality = "720",
					type = "mp4",
					headers = { Referer = referer },
				},
			},
		}
	end,
}
