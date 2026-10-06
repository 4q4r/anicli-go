-- RuTor (rutor.info) — the RU public-tracker torrent catalog, the
-- PR142 Lua migration of the compiled Go provider (PR87) — the
-- twenty-fifth Go→Lua migration and the FIRST TORRENT one. The
-- extraction recipe is the mature Jackett definition
-- (src/Jackett.Common/Definitions/rutor.yml), re-verified against the
-- live HTML on 2026-09-23 (the PR87 record) and re-verified live on
-- 2026-10-06 for this port: the search route answers the shapes below
-- byte-identically («черная лагуна» → 20 rows, «Результатов поиска
-- 20», protocol-relative //d.rutor.info/download/{id} downgif hrefs,
-- span.green/span.red peer counts, per-td sizes like 38.92&nbsp;GB).
--
-- TORRENT CONTRACT: a torrent provider's results are magnet/.torrent
-- links the Go engine (internal/torrent, anacrolix) consumes
-- downstream. The script owns ONLY the search surface (row parsing,
-- the PR44 seedless filter, the magnet fallback, the PR35 quality
-- badge); the engine legs — the PR66 dead-host preflight, the
-- metadata wait (episodes) and the loopback stream resolve — are
-- Go-owned TorrentBase machinery grafted on by the factory's
-- luaTorrent adapter (this script declares torrent = true). The
-- episodes/streams functions below are therefore loud stubs: the
-- adapter intercepts both before they can run, and a hand-loaded
-- script gets a typed failure instead of a faked empty surface.
--
-- The shapes the script serves:
--
--   - search: GET {base}/search/0/0/100/{sort}/{query}/ — the Jackett
--     path (100 = title search), sort 0 = created desc (Jackett
--     default). Queries ride the path percent-encoded with Go's
--     url.PathEscape segment rules (spaces %20, UTF-8 bytes one at a
--     time — never the form-style "+" of http.query_escape; the
--     anidub py_quote lesson, tightened to the exact Go segment
--     semantics). RU queries are first-class (the index matches RU
--     titles; е/ё treated alike).
--   - Rows: tr:has(td:has(a[href^="magnet:?xt="])) — the Jackett row
--     selector verbatim: it keys on the magnet anchor, so the header
--     row (tr.backgr) and the tracker-news block are excluded by
--     construction, and a zebra-class rename upstream cannot break
--     row detection.
--   - Title: the /torrent/ anchor inside the second td (the Go
--     provider's row td #1 chain); the &amp; entity decodes with the
--     text.
--   - .torrent: a.downgif — on the wire a PROTOCOL-RELATIVE
--     //d.rutor.info/download/{id}; resolved against the site root
--     before surfacing (scheme inherited from base_url, host rides
--     the href). Anonymous (200, application/x-bittorrent).
--   - Magnet: a[href^="magnet:?xt="] — the fallback link when the row
--     has no download anchor; kept VERBATIM (it carries its own
--     tr= announces) and validated as a strict 40-hex btih (the
--     tokyotosho base32 lesson).
--   - Size: fished by content (`38.92&nbsp;GB` in its own td, the
--     NBSP normalized) — Jackett fishes too, because the comments
--     cell is OPTIONAL upstream and positional td indexes lie.
--   - Seed/leech: td span.green / td span.red (Jackett), digits
--     extracted from the span text.
--   - The PR44 seedless filter: a row reporting span.green = 0 is a
--     dead result, dropped before surfacing; a row with no seed span
--     at all is kept (no field, no filter). Zero results (HTTP 200,
--     «Результатов поиска 0», no data rows) settle as an empty
--     surface — the row selector is the zero-detection.
--   - The PR35 quality badge rides the search result meta: the
--     torrent.ParseQuality resolution regexps (2160|1440|1080|720|
--     480|360, the WxH form included) ported through the regexp SDK —
--     the same Go regexp engine, so the byte semantics match.
--
-- TRANSPORT (live-probed 2026-10-06, the pre-probe this port was
-- gated on): the PR87-era direct-route uTLS tarpit is GONE — but not
-- because the site softened. The direct route no longer even
-- resolves on the characterization network (DNS wall, the anidub
-- class), and through the proxy the STANDARD per-provider netclient
-- (uTLS fingerprint included) answers the search 200 in ~0.3s. The
-- honest route is the proxy (the route-matrix note: proxy PASS,
-- «черная лагуна» 7/7 at 886ms). PR87 solved the then-live tarpit
-- with a scoped plain-Go transport INSIDE the compiled provider —
-- that escape hatch died with rutor.go; the standard netclient needs
-- no escape hatch on the proxy route, so the script rides it like
-- every other bundled script (the browser-grade UA default plus the
-- RU-leaning Accept-Language the compiled provider sent).

local base_url = "https://rutor.info"

-- The Jackett recipe path with the title bitmask (100) and the
-- default sort (0 = created desc — fresh releases first; the seedless
-- filter handles quality of the rest).
local SEARCH_MASK = "100"
local SEARCH_SORT = "0"

-- NBSP is the size/peer cells' glue on the wire ("38.92&nbsp;GB") —
-- the UTF-8 byte pair, normalized exactly like the compiled
-- provider's \u00a0 replacement.
local NBSP = "\194\160"

-- The UA/Accept-Language pair mirrors the compiled provider's request
-- shape (browser-grade UA, RU-leaning language list for the RU
-- catalog).
local USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " ..
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
local ACCEPT_LANGUAGE = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"

-- trim drops the ASCII whitespace around a cell's text (the compiled
-- provider trimmed before matching).
local function trim(s)
	local t = string.gsub(s, "^%s+", "")
	return (string.gsub(t, "%s+$", ""))
end

-- path_escape ports Go's url.PathEscape (the segment mode): the
-- unreserved set and the segment-allowed reserved set ride unescaped,
-- every other byte (space, Cyrillic, the segment delimiters / ; , ?)
-- percent-encoded uppercase, one UTF-8 byte at a time.
local function path_escape(s)
	local out = {}
	for i = 1, #s do
		local c = string.sub(s, i, i)
		if string.match(c, "^[A-Za-z0-9%-%.%~%$%&%+%:%=%@]$") then
			out[#out + 1] = c
		else
			out[#out + 1] = string.format("%%%02X", string.byte(s, i))
		end
	end
	return table.concat(out)
end

-- absolute resolves a (possibly protocol-relative) href against the
-- site root: "//d.rutor.info/download/1" gains the site's scheme and
-- surfaces as a preflightable URL; root-relative and absolute hrefs
-- pass through resolved. "" stays "".
local function absolute(href)
	if href == "" then
		return ""
	end
	if string.sub(href, 1, 2) == "//" then
		local scheme = string.match(base_url, "^(https?:)")
		return scheme .. href
	end
	if string.sub(href, 1, 1) == "/" then
		return base_url .. href
	end
	return href
end

-- quality_badge ports the torrent.ParseQuality resolution lines (the
-- only field the search meta carries): the NNp form first, then the
-- WxH form; "?" when the title carries no resolution (Badge's own
-- unknown marker). The regexp SDK is the Go regexp engine, so the
-- byte semantics of (?i)/\b match the compiled provider exactly.
local function quality_badge(title)
	local m = anicli.regexp.match("(?i)\\b(2160|1440|1080|720|480|360)p\\b", title)
	if not m then
		m = anicli.regexp.match("\\b\\d{3,4}\\s*[x×]\\s*(2160|1440|1080|720|480|360)\\b", title)
	end
	if not m then
		return "?"
	end
	return m[2] .. "p"
end

-- result_link picks the torrent link of one search row: the a.downgif
-- .torrent URL resolved against the site root, the row magnet
-- verbatim as the no-anchor fallback (strict 40-hex btih — the
-- tokyotosho lesson), "" when neither is usable (the caller drops
-- such rows).
local function result_link(row)
	local dl = trim(row:find("a.downgif"):attr("href"))
	if dl ~= "" then
		local link = absolute(dl)
		if link ~= "" then
			return link
		end
	end
	local magnet = trim(row:find('a[href^="magnet:?xt="]'):attr("href"))
	if magnet ~= "" and anicli.regexp.match("urn:btih:([0-9a-fA-F]{40})", magnet) then
		return magnet
	end
	return ""
end

-- row_size fishes the size cell's text out of the row by content —
-- the comments cell is optional upstream, so positional td indexes
-- lie (the Jackett comment). "" when the row carries no size cell
-- (fail-soft). First match wins (the compiled provider's
-- EachWithBreak).
local function row_size(row)
	local size = ""
	row:find("td"):each(function(_, td)
		if size ~= "" then
			return
		end
		local text = trim(string.gsub(td:text(), NBSP, " "))
		if string.match(text, "^[%d%.%,]+%s*[KkMmGgTtPpEe]?[Bb]$") then
			size = text
		end
	end)
	return size
end

-- row_peer extracts the count from the row's seed/leech span
-- (span.green / span.red); "" when the span is missing or carries no
-- digits (fail-soft — the PR44 filter keeps no-field rows).
local function row_peer(row, sel)
	local digits = string.match(row:find(sel):text(), "%d+")
	if digits == nil then
		return ""
	end
	return digits
end

return {
	id = "rutor",
	name = "RuTor",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	-- The torrent declaration: the factory grafts the Go engine legs
	-- (preflight, episodes, stream resolve) onto this script through
	-- the luaTorrent adapter. Without it the provider cannot play
	-- anything — the [torrent] disabled rule drops the slot.
	torrent = true,

	search = function(query)
		-- Empty queries are a caller bug: reject before any network
		-- I/O (the compiled provider's guard, typed through the
		-- anicli.fail marker).
		if trim(query) == "" then
			anicli.fail("invalid_input", "пустой поисковый запрос")
		end

		local search_url = base_url .. "/search/0/0/" .. SEARCH_MASK .. "/" .. SEARCH_SORT .. "/"
			.. path_escape(query) .. "/"
		local resp = anicli.http.get(search_url, {
			headers = {
				["User-Agent"] = USER_AGENT,
				["Accept-Language"] = ACCEPT_LANGUAGE,
			},
		})
		local doc = anicli.html.parse(resp.body)

		local results = {}
		-- The Jackett row selector verbatim: rows carrying a magnet
		-- anchor.
		doc:find('tr:has(td:has(a[href^="magnet:?xt="]))'):each(function(_, row)
			local title = trim(row:find('td:nth-of-type(2) a[href^="/torrent/"]'):text())
			if title == "" then
				return
			end
			local link = result_link(row)
			if link == "" then
				-- No .torrent anchor and no usable magnet: the row
				-- has nothing the engine could ingest.
				return
			end
			results[#results + 1] = {
				title = title,
				url = link,
				meta = {
					["size"] = row_size(row),
					["seeders"] = row_peer(row, "span.green"),
					["leechers"] = row_peer(row, "span.red"),
					-- The PR35 release parser rides the search
					-- result too: the badge is known before ingest.
					["quality"] = quality_badge(title),
				},
			}
		end)

		-- The PR44 seedless filter: a row reporting seeders 0 is a
		-- dead result, dropped before it can cost the preflight a
		-- fetch; no field survives (no field, no filter). The
		-- seeder string is always pure digits here (it comes from a
		-- %d+ extraction), so tonumber is exact.
		local out = {}
		for _, r in ipairs(results) do
			local seeders = r.meta["seeders"]
			if seeders == "" or tonumber(seeders) ~= 0 then
				out[#out + 1] = r
			end
		end
		return out
	end,

	-- The engine legs: loud stubs, never a faked empty surface. The
	-- production luaTorrent adapter intercepts episodes/streams
	-- before these run; a hand-loaded script fails with the pointer
	-- to the real owner.
	episodes = function(anime_url)
		anicli.fail("extract_failed",
			"rutor episodes are owned by the torrent engine (the luaTorrent adapter); the raw link is: "
				.. tostring(anime_url))
	end,

	streams = function(episode_url, dub)
		anicli.fail("extract_failed",
			"rutor streams are owned by the torrent engine (the luaTorrent adapter); the raw id is: "
				.. tostring(episode_url))
	end,
}
