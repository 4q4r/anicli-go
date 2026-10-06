-- AniDUB (online.anidub.com) — the RU DLE catalog, the PR132 Lua
-- migration of the compiled Go provider. anidub is NOT a frozen
-- anicli-py port: the provider was characterized live on 2026-09-13
-- and written against the observed DLE shapes (the anizone
-- precedent). The shapes the script serves:
--
--   - search: the DLE search form answers as a GET with the query in
--     the URL (?do=search&subaction=search&story=<q>; the site's own
--     <form method="post"> works too, the compiled provider pinned
--     the GET). Results render server-side reusing the catalog
--     .th-item card template inside .sect-content.sect-items — the
--     selector scope keeps the top owl-slider .th-item cards
--     (ongoings) out. LIVE-VERIFIED 2026-09-13 and 2026-10-06:
--     story=naruto → 13 cards; junk query → HTTP 200 with zero
--     cards. The query is percent-encoded by py_quote (spaces %20,
--     one UTF-8 byte at a time — never the form-style "+" of the
--     SDK's query_escape, the compiled provider's pyQuote behavior).
--   - episodes: the anime page carries two player tabs inside
--     .fplayer. The «Основной плеер» span (ПЛЕЕР #1) points at a
--     full-title playlist player on an external host — its episodes
--     cannot be split without fetching that player, so it is skipped
--     by the «Серия» text gate. The «Запасной плеер» tab carries one
--     span per episode, text "Серия N", data attribute a
--     video.sibnet.ru/shell.php embed. Movie pages carry a single
--     "Серия 1" span and no primary tab. Spans emit in document
--     order; the single dub key is "AniDUB".
--   - streams: the embed URL rides raw_id (the fresh-sandbox state
--     channel — the sameband single-value precedent; the compiled
--     provider kept the episode number there, but the number already
--     rides num) and resolves through anicli.extract, the shared Go
--     extractor factory whose sibnet extractor turns shell.php
--     embeds into the 480p mp4 source. No Lua-side resolve logic —
--     the sibnet player-page regex lives in internal/extractors,
--     shared with the compiled providers.
--
-- Route note (2026-10-06): the anidub legs answer on BOTH the direct
-- and proxied routes (search 13 cards, anime page 200 after the
-- short-slug 301 redirect). The sibnet shell leg answers HTTP 403
-- ("Request forbidden by administrative rules") on every route since
-- at least the PR116 verification matrix — site-side drift the
-- shared extractor hit before this port; the live smoke keeps the
-- same resolve-leg failure shape the compiled Go provider shows.

local base_url = "https://online.anidub.com"

-- DUB is the single dub name every episode carries (the site's own
-- dubs; the compiled provider's RawEmbeds key).
local DUB = "AniDUB"

-- py_quote ports the compiled provider's pyQuote helper
-- (internal/providers/providers.go), itself a port of
-- urllib.parse.quote with its default safe="/" set: every byte
-- outside the URL-unreserved set (and "/") is percent-encoded
-- uppercase, one UTF-8 byte at a time — spaces become %20, not the
-- form-style "+" of http.query_escape.
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

-- trim drops the ASCII whitespace around a span label (the compiled
-- provider trimmed the text before and after the «Серия» prefix cut).
local function trim(s)
	local t = string.gsub(s, "^%s+", "")
	return (string.gsub(t, "%s+$", ""))
end

return {
	id = "anidub",
	name = "AniDUB",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",

	search = function(query)
		local resp = anicli.http.get(base_url .. "/?do=search&subaction=search&story=" .. py_quote(query))

		local doc = anicli.html.parse(resp.body)
		local results = {}
		doc:find(".sect-items .th-item"):each(function(_, item)
			local link = item:find("a.th-in[href]")
			local title = item:find(".th-title")
			if link:len() == 0 or title:len() == 0 then
				return
			end
			local href = link:attr("href")
			if href == "" then
				return
			end

			local poster = ""
			local img = item:find(".th-img img[src]")
			if img:len() > 0 then
				local src = img:attr("src")
				if string.sub(src, 1, 4) ~= "http" then
					poster = base_url .. src
				else
					poster = src
				end
			end

			results[#results + 1] = { title = title:text(), url = href, poster = poster }
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local doc = anicli.html.parse(resp.body)

		local episodes = {}
		doc:find(".fplayer .series-tab span[data]"):each(function(_, span)
			local title = span:text()
			-- The «Серия» prefix gate skips the ПЛЕЕР #1 playlist span
			-- and every other non-episode label (byte-wise UTF-8
			-- prefix compare; UTF-8 is prefix-free).
			local prefix = "Серия"
			if string.sub(title, 1, #prefix) ~= prefix then
				return
			end
			local num = trim(string.sub(title, #prefix + 1))
			if num == "" then
				return
			end

			local embed = span:attr("data")
			if embed == "" then
				return
			end

			episodes[#episodes + 1] = {
				num = num,
				title = title,
				raw_id = embed,
				raw_embeds = { [DUB] = { embed } },
			}
		end)
		return episodes
	end,

	streams = function(raw_id, dub)
		local stream = { dub_name = dub, links = {} }
		-- An unknown dub key carries no embeds, and an empty raw_id
		-- hydrates nothing: an empty stream without any network I/O
		-- (the compiled provider's RawEmbeds[dub] nil parity).
		if dub ~= DUB or raw_id == "" then
			return stream
		end
		stream.links = anicli.extract({ raw_id })
		return stream
	end,
}
