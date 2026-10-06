-- AniRena (anirena.com) — the PR143 Lua migration of the compiled Go
-- provider (PR88; the fifth TorrentBase roster member) — the
-- twenty-sixth Go→Lua provider migration, the torrent family's second
-- Lua slot (the PR142 rutor migration was the first).
-- Anonymous public tracker, JA/multilingual releases, no credentials,
-- nothing to configure: the documented JSON API gates uploads AND
-- torrent search behind personal API keys (POST
-- /api/v1/torrents/search without a bearer answers 401, live-verified
-- 2026-09-23), so the anonymous RSS is the provider route.
--
-- Route (the compiled provider's, byte-faithful):
--   - search: GET /rss?q=<url-encoded raw query> — the RSS 2.0 feed
--     of the site search (their guide documents only /rss and
--     ?category=; q= answers the web search's query). The documented
--     ?category= filter is IGNORED by the server (the feed spans ALL
--     categories even with category=anime, live-verified 2026-09-23),
--     so the request never carries it and the Anime scope is enforced
--     client-side on the item description's "Category: …" field
--     (exact "Anime" or an "Anime …"/"Anime>…" subcategory).
--   - There is no usable server-side limit parameter (per_page/limit
--     ignored; a feed answers up to 250 items), so the FIRST 30 items
--     are capped client-side BEFORE the category filter — the Go
--     adapter's preflight fan-out downstream can never grow with the
--     feed (the compiled provider capped parsed items the same way).
--   - Item titles carry a leading "[Category > Subcategory] " prefix
--     — stripped only when the bracket group names one of the site
--     categories; release-group tags like [SubsPlease] survive
--     verbatim.
--   - Each item's <enclosure> is the DIRECT .torrent URL on the site
--     itself; it rides the result URL byte-faithfully (the engine
--     consumes it downstream). Items without one are dropped. The
--     feed carries NO seed fields, so no seed meta keys are ever set
--     (the fail-soft "no field, no filter" seedless contract).
--   - meta.size is the description's "Size: …" field; meta.quality is
--     the PR35 resolution badge (the torrent.ParseQuality badge: the
--     resolution token or "?"). Both resolution patterns terminate on
--     a word boundary and no video extension contains a resolution
--     token, so the compiled path's extension strip cannot change the
--     badge and is not modeled.
--
-- The TORRENT PLUMBING STAYS GO: this script serves the search
-- surface only. The PR66 dead-host preflight, the metadata ingestion
-- and the episodes/streams resolution run in the luaTorrent adapter
-- (internal/providers/lua_torrent.go) around this script — the
-- torrent engine handle never crosses the sandbox. The episodes and
-- streams entries below are loud walls for a bare-sandbox load (they
-- are unreachable through the adapter).
--
-- Documented divergences from the compiled provider (the Lua
-- engine's hand-rolled RSS reading): the feed envelope is checked as
-- the <channel> element (a full XML parse would need a second
-- dependency — a body without one is the typed extract wall, a
-- well-formed feed with a channel parses best-effort); entity
-- decoding covers the named XML entities and ASCII numeric
-- references (higher codepoints stay literal — the feed's titles
-- carry named entities only, e.g. &gt;); a CDATA-wrapped field is
-- taken verbatim (the compiled parser decodes entities in plain text
-- and keeps CDATA literal — mixed text/CDATA concatenation is not
-- modeled; the feed fields are pure CDATA); whitespace trimming is
-- the ASCII %s subset of Go's Unicode TrimSpace.

local base_url = "https://www.anirena.com"

-- AniRenaSearchLimit (the compiled provider's cap, the animetosho
-- rule): the feed has no server-side limit, the first 30 items are
-- the bounded surface.
local SEARCH_LIMIT = 30

-- The site's torrent categories (their guide, live titles matched):
-- a leading "[<category>…]" title group naming one of these is the
-- feed's category prefix and is stripped from the release title.
local CATEGORIES = {
	"Anime", "Manga/Manhwa/Comic", "Audio", "Literature",
	"Live Action", "Pictures", "Software", "Hentai", "Other",
}

-- trim is the ASCII whitespace subset of Go's strings.TrimSpace.
local function trim(s)
	return (string.gsub(s, "^%s*(.-)%s*$", "%1"))
end

-- xml_decode resolves the named XML entities and ASCII numeric
-- character references in plain element text. Unknown or non-ASCII
-- references stay literal (the divergence note in the header).
local NAMED_ENTITIES = { lt = "<", gt = ">", amp = "&", quot = '"', apos = "'" }
local function xml_decode(s)
	return (string.gsub(s, "&(#?)(%w+);", function(hash, name)
		if hash == "#" then
			local code
			if string.sub(name, 1, 1) == "x" or string.sub(name, 1, 1) == "X" then
				code = tonumber(string.sub(name, 2), 16)
			else
				code = tonumber(name)
			end
			if code and code >= 32 and code < 256 then
				return string.char(code)
			end
		elseif NAMED_ENTITIES[name] then
			return NAMED_ENTITIES[name]
		end
		-- keep the reference verbatim
		return "&" .. hash .. name .. ";"
	end))
end

-- xml_text reads one element's inner text: a CDATA-wrapped body is
-- taken verbatim (the compiled XML parser never decodes entities
-- inside CDATA — the description's Category field relies on that),
-- plain text is entity-decoded.
local function xml_text(s)
	if not s then
		return nil
	end
	local cdata = string.match(s, "^<!%[CDATA%[(.-)%]%]>$")
	if cdata then
		return cdata
	end
	return xml_decode(s)
end

-- desc_field extracts one "Key: value" field from the description
-- CDATA (up to the next " | " separator); "" when absent.
local function desc_field(desc, key)
	local start = string.find(desc, key, 1, true)
	if not start then
		return ""
	end
	local rest = string.sub(desc, start + #key)
	local stop = string.find(rest, " | ", 1, true)
	if stop then
		return trim(string.sub(rest, 1, stop - 1))
	end
	return trim(rest)
end

-- strip_category_prefix removes a leading "[<category>[ > subcategory]]
-- " group when it names one of the site categories (exact or a
-- "<category> " subcategory prefix); anything else — release-group
-- tags like [SubsPlease] — is part of the title and stays verbatim.
local function strip_category_prefix(raw)
	local title = trim(raw or "")
	if string.sub(title, 1, 1) ~= "[" then
		return title
	end
	local close = string.find(title, "]", 1, true)
	if not close then
		return title
	end
	local inside = string.sub(title, 2, close - 1)
	for _, cat in ipairs(CATEGORIES) do
		if inside == cat or string.sub(inside, 1, #cat + 1) == cat .. " " then
			return trim(string.sub(title, close + 1))
		end
	end
	return title
end

-- is_anime_category scopes the description's Category field into the
-- Anime category (exact "Anime" or an "Anime …"/"Anime>…"
-- subcategory). An item without a parseable category is out of scope
-- too: catalog scope is a hard contract, not a dead-result filter.
local function is_anime_category(desc)
	local cat = desc_field(desc, "Category: ")
	return cat == "Anime"
		or string.sub(cat, 1, 6) == "Anime "
		or string.sub(cat, 1, 6) == "Anime>"
end

-- quality_badge renders the PR35 resolution badge ("1080p", "?"): the
-- same two resolution patterns the torrent.ParseQuality badge reads,
-- case-insensitive, word-bounded (the NNNp token, else the WxH form).
local function quality_badge(title)
	local m = anicli.regexp.match("(?i)\\b(2160|1440|1080|720|480|360)p\\b", title)
	if not m then
		m = anicli.regexp.match("\\b\\d{3,4}\\s*[x×]\\s*(2160|1440|1080|720|480|360)\\b", title)
	end
	if m then
		return m[2] .. "p"
	end
	return "?"
end

return {
	id = "anirena",
	name = "AniRena",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ja",
	-- PR42: the index matches romaji/english release names only — the
	-- search fan-out routes it the latin variants, never the Cyrillic
	-- ones.
	name_preference = "latin",

	search = function(query)
		-- Empty queries are a caller bug: reject before any network
		-- I/O (the compiled provider's guard, same message).
		if trim(query) == "" then
			anicli.fail("invalid_input", "пустой поисковый запрос")
		end

		local resp = anicli.http.get(base_url .. "/rss?q=" .. anicli.http.query_escape(query))
		local body = resp.body

		-- The envelope wall (the compiled provider's xml decode
		-- error): a body without an RSS channel is the typed extract
		-- failure, never an empty success.
		if not string.find(body, "<channel", 1, true) then
			anicli.fail("extract_failed", "decode rss: no RSS channel envelope")
		end

		-- The bounded surface: the FIRST SEARCH_LIMIT items, capped
		-- before the filter exactly like the compiled provider (the
		-- cap counts parsed items, not survivors).
		local items = {}
		for item in string.gmatch(body, "<item>(.-)</item>") do
			items[#items + 1] = item
			if #items >= SEARCH_LIMIT then
				break
			end
		end

		local results = {}
		for _, item in ipairs(items) do
			local title = strip_category_prefix(xml_text(string.match(item, "<title>(.-)</title>")))
			if title ~= "" then
				local desc = xml_text(string.match(item, "<description>(.-)</description>")) or ""
				if is_anime_category(desc) then
					-- The <enclosure> carries the direct .torrent URL;
					-- an item without one has nothing the engine could
					-- ingest (the live feed always carries it).
					local link = trim(string.match(item, '<enclosure.-url="([^"]*)"') or "")
					if link ~= "" then
						results[#results + 1] = {
							title = title,
							url = link,
							meta = {
								-- The human release size rides the
								-- description; the feed has no seed
								-- fields — the seed meta keys stay
								-- absent so filterSeedless keeps the
								-- item.
								size = desc_field(desc, "Size: "),
								quality = quality_badge(title),
							},
						}
					end
				end
			end
		end
		return results
	end,

	-- Torrent slots resolve through the Go engine (the luaTorrent
	-- adapter wraps this script's slot: preflight, ingestion,
	-- EpisodesWait and the loopback stream resolve). Reaching these
	-- walls means the script was loaded WITHOUT the adapter (a bare
	-- sandbox) — fail loud instead of pretending.
	episodes = function()
		anicli.fail("extract_failed", "anirena resolves through the torrent engine adapter, not the script")
	end,

	streams = function()
		anicli.fail("extract_failed", "anirena resolves through the torrent engine adapter, not the script")
	end,
}
