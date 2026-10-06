-- animetosho (feed.animetosho.org) — the PR146 Lua migration of the
-- compiled Go torrent provider (PR38), the TWENTY-NINTH Go→Lua
-- migration and the torrent family's fifth Lua slot (rutor PR142,
-- anirena PR143, subsplease PR144, anilibria-torrent PR145).
--
-- The search speaks the site's JSON API:
--
--   GET /json?q=<query>   → a JSON array of release records, relevance
--                           order (the same catalog the newznab feed
--                           serves, live-verified 2026-10-06: the
--                           endpoint answers anonymously, ~0.3s on
--                           the direct route).
--
-- The compiled provider spoke the newznab XML dialect
-- (/api?t=search&cat=5070&limit=30&offset=0); the Lua contract has no
-- XML parser, so this script speaks the JSON twin of the SAME feed.
-- Documented divergences, both live-verified 2026-10-06:
--   - /json has no server-side limit (&limit= changes nothing — 75
--     records answered with and without it), so the bounded-page rule
--     (the compiled AnimeToshoSearchLimit, 30) moved client-side into
--     SEARCH_LIMIT below, applied before the seedless filter (the
--     anirena PR143 precedent);
--   - /json has no category parameter: the site indexes anime only,
--     so the newznab cat=5070 filter had no twin and no observable
--     effect to port.
--
-- Record shape (the fields this script consumes): title (the release
-- name), torrent_url (the direct .torrent on AT's own storage
-- host — the PR66 bytes-ingestible, preflightable link), magnet_uri
-- (LIVE SHAPE: base32 nekoBT mirror hashes — the engine contract is
-- 40-hex, so it is taken only when hex), info_hash (40-hex),
-- total_size (byte count), seeders/leechers (absent on some records —
-- fail-soft: no field, no value, never a fabricated zero).
--
-- The torrent link ladder (the compiled animeToshoResultLink): the
-- torrent_url verbatim, a HEX magnet_uri verbatim (its tr= announces
-- aid peer discovery), a magnet built from a well-formed 40-hex
-- info_hash, else the record is dropped — it has nothing the engine
-- could ingest.
--
-- THE ENGINE BOUNDARY (the owner ruling on this migration): only the
-- SEARCH surface migrated. The torrent engine (internal/torrent, the
-- anacrolix core) stays Go, and the roster slot serves the
-- search-in-Lua hybrid: this script declares torrent = true, so the
-- factory wraps it in the Go torrent adapter — the PR66 dead-host
-- preflight (which fetches every surfaced torrent_url's bytes and
-- feeds the engine), the bounded metadata wait and the loopback
-- stream resolve remain the shared TorrentBase legs, byte-identical
-- to the compiled provider's. The mandatory episodes/streams stubs
-- below exist only to satisfy the script contract's mandatory shape
-- and fail loud: a bare-script load without the adapter can never
-- fake success.
--
-- Known walls (typed via anicli.fail / the transport markers):
--   - an empty or whitespace-only query → invalid_input before any
--     network leaves the process (the compiled provider's guard);
--   - a non-array search body (an error envelope) → extract_failed;
--   - HTML instead of JSON → the http.get_json decode raise.

local base_url = "https://feed.animetosho.org"

-- SEARCH_LIMIT bounds one search page (the compiled
-- AnimeToshoSearchLimit): a bounded list keeps the TUI predictable
-- and the Go adapter's preflight fan-out finite. The endpoint has no
-- server-side limit parameter, so the cap is client-side.
local SEARCH_LIMIT = 30

-- INFOHASH_HEX_LEN is the BitTorrent v1 infohash length in hex chars
-- (40) — the shared btih contract a usable torrent link must meet.
local INFOHASH_HEX_LEN = 40

-- The PR35 quality badge patterns (Go regexp through the SDK — the
-- subsplease PR144 precedent): the resolution token wins, the
-- width×height form answers when no p-suffix is present.
local RESOLUTION_P_RE = "(?i)\\b(2160|1440|1080|720|480|360)p\\b"
local RESOLUTION_WH_RE = "\\b\\d{3,4}\\s*[x×]\\s*(2160|1440|1080|720|480|360)\\b"

local function trim(s)
	return (string.gsub(s, "^%s*(.-)%s*$", "%1"))
end

-- is_hex40 reports whether s is exactly 40 lowercase hex chars (the
-- callers lowercase before checking — the compiled isHex contract).
local function is_hex40(s)
	return #s == INFOHASH_HEX_LEN and string.match(s, "^%x+$") ~= nil
end

-- magnet_uri reports whether s is a magnet: URI whose FIRST btih
-- parameter is a well-formed 40-hex infohash (the engine ingests it
-- verbatim; its tr= announces are peer-discovery value). The first
-- parameter decides — the compiled magnetURI contract. The live
-- feed's base32 magnets fail this gate on purpose.
local function magnet_uri(s)
	if string.sub(s, 1, 8) ~= "magnet:?" then
		return false
	end
	for param in string.gmatch(string.sub(s, 9), "[^&]+") do
		local hash = string.match(param, "^xt=urn:btih:(.+)$")
		if hash then
			return is_hex40(string.lower(hash))
		end
	end
	return false
end

-- torrent_link picks one record's link: the torrent_url verbatim (the
-- bytes-ingestible, preflightable .torrent on AT's own storage), a
-- hex magnet_uri verbatim, a magnet built from the info_hash,
-- nil when none is usable (the record is dropped, never handed
-- downstream as a dead result).
local function torrent_link(title, torrent_url, magnet, info_hash)
	local u = trim(torrent_url or "")
	if u ~= "" then
		return u
	end
	if magnet_uri(magnet or "") then
		return magnet
	end
	local h = string.lower(trim(info_hash or ""))
	if is_hex40(h) then
		return "magnet:?xt=urn:btih:" .. h .. "&dn=" .. anicli.http.query_escape(title)
	end
	return nil
end

-- human_bytes renders a byte count in binary units with one decimal —
-- the TUI torrent suffix convention ("21.6 GiB"; the compiled
-- humanBytes port).
local function human_bytes(n)
	if n < 1024 then
		return string.format("%d B", n)
	end
	local units = { "K", "M", "G", "T", "P", "E" }
	local div, exp = 1024, 0
	local m = math.floor(n / 1024)
	while m >= 1024 do
		div = div * 1024
		exp = exp + 1
		m = math.floor(m / 1024)
	end
	return string.format("%.1f %siB", n / div, units[exp + 1])
end

-- size_meta renders the total_size field: numeric values convert,
-- anything else (absent included) rides verbatim — fail-soft, the
-- feed owns the format. The parse is strict: a numeric-prefixed
-- value like "123abc" is garbage, not 123 (the compiled
-- humanBytesAttr contract).
local function size_meta(value)
	if type(value) == "number" then
		return human_bytes(value)
	end
	local raw = trim(tostring(value or ""))
	if raw ~= "" and string.match(raw, "^%d+$") then
		return human_bytes(tonumber(raw))
	end
	return raw
end

-- num_str renders a seed/leech count as the feed reports it: absent
-- stays empty (fail-soft), a number formats as its decimal string.
local function num_str(value)
	if value == nil then
		return ""
	end
	return string.format("%d", value)
end

-- quality_badge is the resolution slice of the engine's ParseQuality
-- badge: the leftmost "NNNNp" token wins, the "WxH" form resolves the
-- same way, "?" when the name carries neither (the subsplease PR144
-- port — Go regexp through the SDK, the alternation semantics).
local function quality_badge(title)
	local m = anicli.regexp.match(RESOLUTION_P_RE, title)
	if m == nil then
		m = anicli.regexp.match(RESOLUTION_WH_RE, title)
	end
	if m == nil then
		return "?"
	end
	return m[2] .. "p"
end

-- torrent_result maps one JSON record onto a search result row, or
-- nil when the record has no usable title or no usable torrent link.
local function torrent_result(item)
	if type(item) ~= "table" then
		return nil
	end
	local title = trim(item.title or "")
	if title == "" then
		return nil
	end
	local link = torrent_link(title, item.torrent_url, item.magnet_uri, item.info_hash)
	if link == nil then
		return nil
	end
	return {
		title = title,
		url = link,
		meta = {
			size = size_meta(item.total_size),
			seeders = num_str(item.seeders),
			leechers = num_str(item.leechers),
			quality = quality_badge(title),
		},
	}
end

-- filter_seedless drops rows whose seeder meta parses to 0 — a
-- seedless torrent is a dead result. Fail-soft at the edges: a row
-- with no (or an unparseable) seed field is kept (the shared Go
-- filterSeedless rule).
local function filter_seedless(rows)
	local out = {}
	for _, row in ipairs(rows) do
		local n = tonumber(row.meta and row.meta.seeders)
		if n == nil or n > 0 then
			out[#out + 1] = row
		end
	end
	return out
end

return {
	id = "animetosho",
	name = "AnimeTosho",
	base_url = base_url,
	content_lang = "ja",
	-- The feed indexes romaji/english release names only (PR42): the
	-- search fan-out routes it the latin variants, never the Cyrillic
	-- ones.
	name_preference = "latin",
	torrent = true,

	search = function(query)
		-- Empty queries are a caller bug: reject before any network.
		if trim(query) == "" then
			anicli.fail("invalid_input", "пустой поисковый запрос")
		end

		local items = anicli.http.get_json(base_url .. "/json?q=" .. anicli.http.query_escape(query))

		-- The search endpoint answers with an ARRAY of records; an
		-- error envelope decodes to a keyed table with no [1] — the
		-- typed wall.
		if type(items) ~= "table" or (items[1] == nil and next(items) ~= nil) then
			anicli.fail("extract_failed", "search response is not a record array")
		end

		local results = {}
		for _, item in ipairs(items) do
			if #results >= SEARCH_LIMIT then
				break
			end
			local row = torrent_result(item)
			if row then
				results[#results + 1] = row
			end
		end
		return filter_seedless(results)
	end,

	-- Structurally unreachable through the roster (the Go torrent
	-- adapter owns these legs — see the header); loud on any bare
	-- path, never a faked success.
	episodes = function(anime_url)
		anicli.fail("invalid_input", "animetosho serves episodes through the Go torrent adapter, not the script (got "
			.. tostring(anime_url) .. ")")
	end,

	streams = function(episode_url, dub)
		anicli.fail("invalid_input", "animetosho resolves streams through the Go torrent adapter, not the script (got "
			.. tostring(episode_url) .. ", dub " .. tostring(dub) .. ")")
	end,
}
