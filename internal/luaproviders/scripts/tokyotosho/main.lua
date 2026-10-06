-- TokyoTosho (tokyo-tosho.net) — the PR147 Lua migration of the
-- compiled Go provider (PR38) — the thirtieth and FINAL Go→Lua
-- provider migration: with this slot the roster carries no compiled
-- factory. Anonymous public tracker index, JA/multilingual releases,
-- no credentials, nothing to configure.
--
-- Route (the compiled provider's, byte-faithful):
--   - search: GET /rss.php?terms=<url-encoded raw query>&type=1 —
--     the "RSS Feed of these results" link the site's own search page
--     renders. Live-verified parameter traps baked in (curl
--     2026-09-17, re-verified through the proxy 2026-10-06):
--     rss.php?search= is a TRAP (the parameter is ignored and the
--     feed answers byte-identically to the plain latest-releases
--     feed) and search.php?q= is a trap too (the form posts `terms`;
--     the q= request returns an empty listing table) — `terms=` is
--     the real parameter and the request never carries the others.
--   - The feed's type=1 filter is SOFT (the live «black lagoon»
--     capture mixes 45 Hentai Manga, 24 Music and 19 Hentai items
--     into the 21 Anime ones), so the provider keeps only items whose
--     <category> is exactly "Anime" — an anime-search provider must
--     not hand the user raws, manga or hentai.
--   - The result link is the <link> element verbatim: a direct
--     .torrent URL, often a cross-posted mirror of the release
--     (anirena dl pages, nyaa.si downloads, the nekoBT API). The
--     engine ingests it by URL itself. The description's magnet is
--     base32 (nekoBT mirror hashes) and is deliberately NOT ingested:
--     the engine contract is a 40-hex btih, the <link> is the
--     ingest surface.
--   - The feed carries NO seed counts: no seed meta key is ever set,
--     so the PR44 seedless filter no-ops here (the fail-soft "no
--     field, no filter" contract).
--   - meta.size is the "Size: …" line inside the description HTML
--     blob, parsed fail-soft ("" when the feed omits it — the key is
--     still set, the compiled provider set it unconditionally);
--     meta.quality is the PR35 resolution badge (the
--     torrent.ParseQuality badge: the resolution token or "?").
--   - No result cap: the compiled provider capped nothing (the TT
--     search feed is a bounded editorial surface), so neither does
--     the script.
--
-- The zero-result shape (live curl 2026-09-17, re-verified
-- 2026-10-06): an unmatched query answers HTTP 200 with the bare feed
-- FOOTER — closing tags only, no <?xml/<rss/<channel opening, no
-- items. A body that is empty or opens with a closing tag is
-- definitionally not a feed: it settles as zero results, never as a
-- raw XML-syntax crash.
--
-- THE ENGINE BOUNDARY (the PR147 owner ruling): only the SEARCH
-- surface migrated. The torrent engine (internal/torrent, the
-- anacrolix core) stays Go, and the roster slot serves the
-- search-in-Lua hybrid: this script surfaces the <link> .torrent
-- URLs, the luaTorrent adapter preflights them (the PR66 dead-host
-- ruling) and TorrentBase ingests and resolves playback. The
-- mandatory episodes/streams functions below are the typed wall
-- naming the engine — a pure-Lua resolve would fake the chain.
--
-- Documented divergences from the compiled provider (the Lua
-- engine's hand-rolled RSS reading, the anirena precedent): a body
-- that opens the feed but never closes it (</rss> missing) is the
-- typed truncation wall — the compiled XML decoder failed such
-- bodies the same way; malformation INSIDE a closed envelope parses
-- best-effort (a full XML well-formedness check would need a second
-- dependency, and the live feed is well-formed); entity decoding
-- covers the named XML entities and ASCII numeric references (higher
-- codepoints stay literal — the feed's titles carry raw UTF-8);
-- a CDATA-wrapped field is taken verbatim (the compiled parser
-- decodes entities in plain text and keeps CDATA literal — mixed
-- text/CDATA concatenation is not modeled; the feed's link and
-- description fields are pure CDATA); whitespace trimming is the
-- ASCII %s subset of Go's Unicode TrimSpace.

local base_url = "https://www.tokyo-tosho.net"

-- The exact <category> value kept by the search: the feed's own
-- type=1 filter is soft, the category element is not.
local ANIME_CATEGORY = "Anime"

-- The PR35 quality badge patterns: the resolution token wins, the
-- width×height form answers when no p-suffix is present.
local RESOLUTION_P_RE = "(?i)\\b(2160|1440|1080|720|480|360)p\\b"
local RESOLUTION_WH_RE = "\\b\\d{3,4}\\s*[x×]\\s*(2160|1440|1080|720|480|360)\\b"

-- The size line inside the description HTML blob ("Size: 1.66GB");
-- the feed owns the format, so it rides verbatim.
local SIZE_RE = "Size:\\s*([0-9.]+\\s*[KMGTPE]?B)"

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
-- inside CDATA — the description's HTML blob relies on that), plain
-- text is entity-decoded.
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

-- quality_badge renders the PR35 resolution badge of one release
-- title: the resolution token ("1080p"), "?" when the title carries
-- none.
local function quality_badge(title)
	local m = anicli.regexp.match(RESOLUTION_P_RE, title)
	if not m then
		m = anicli.regexp.match(RESOLUTION_WH_RE, title)
	end
	if m then
		return m[2] .. "p"
	end
	return "?"
end

-- size_text extracts the size text ("1.66GB") from the description
-- HTML blob; "" when the feed omits the line (fail-soft).
local function size_text(description)
	local m = anicli.regexp.match(SIZE_RE, description)
	if m == nil then
		return ""
	end
	return trim(m[2])
end

return {
	id = "tokyotosho",
	name = "TokyoTosho",
	base_url = base_url,
	capabilities = "both",
	torrent = true,
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

		local resp = anicli.http.get(base_url .. "/rss.php?terms=" ..
			anicli.http.query_escape(query) .. "&type=1")
		local body = resp.body

		-- The site's own zero-result shape (the header note): a
		-- trimmed body that is empty or opens with a closing tag is
		-- definitionally not a feed — it settles as zero results,
		-- never as a raw XML-syntax crash.
		local first = string.match(body, "^%s*(%S)") or ""
		if first == "" or string.match(body, "^%s*</") then
			return {}
		end

		-- The envelope wall (the compiled provider's xml decode
		-- error): a body without an RSS channel is the typed extract
		-- failure, never an empty success.
		if not string.find(body, "<channel", 1, true) then
			anicli.fail("extract_failed", "decode rss: no RSS channel envelope")
		end
		-- A body that opens the feed and breaks mid-stream is a
		-- provider malfunction: it stays a typed failure, never a
		-- silent partial surface (every real feed closes the root
		-- element).
		if not string.find(body, "</rss>", 1, true) then
			anicli.fail("extract_failed", "decode rss: feed truncated mid-stream")
		end

		local results = {}
		for item in string.gmatch(body, "<item>(.-)</item>") do
			-- The soft type=1 filter enforced on the authoritative
			-- element: exact Anime category only.
			local category = trim(xml_text(string.match(item, "<category>(.-)</category>")) or "")
			if category == ANIME_CATEGORY then
				local title = trim(xml_text(string.match(item, "<title>(.-)</title>")) or "")
				if title ~= "" then
					-- The <link> .torrent URL verbatim — the engine
					-- ingests it by URL. An item without one has
					-- nothing the engine could ingest (the TorrentBase
					-- rule): dropped, never a dead result.
					local link = trim(xml_text(string.match(item, "<link>(.-)</link>")) or "")
					if link ~= "" then
						local desc = xml_text(string.match(item, "<description>(.-)</description>")) or ""
						results[#results + 1] = {
							title = title,
							url = link,
							meta = {
								-- The size text rides the description
								-- blob verbatim; "" on some items
								-- (fail-soft). The feed carries no seed
								-- counts at all — no seed meta key is
								-- ever set, so filterSeedless keeps the
								-- item.
								size = size_text(desc),
								-- The PR35 release parser rides the
								-- search result too: the badge is known
								-- before ingest.
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
		anicli.fail("extract_failed", "tokyotosho resolves through the torrent engine adapter, not the script")
	end,

	streams = function()
		anicli.fail("extract_failed", "tokyotosho resolves through the torrent engine adapter, not the script")
	end,
}
