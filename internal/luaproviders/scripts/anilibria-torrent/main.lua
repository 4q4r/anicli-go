-- anilibria-torrent (aniliberty.top) — the PR145 Lua migration of
-- the compiled Go torrent provider (PR37; the torrent SIBLING of the
-- PR120 anilibria streaming script, riding the same release-search
-- endpoint and the same /api/v1 prefix of the ONE base_url literal
-- below).
--
-- The new API has no query parameter on its torrents feed
-- (/anime/torrents ignores query params — live-verified 2026-09-17);
-- search therefore rides the same release search as the stream
-- provider and expands each hit into the per-release torrent list
-- (GET /api/v1/anime/torrents/release/{id}, the endpoint the site's
-- own ReleaseTorrentsTab calls). Every torrent of a release
-- (quality/codec variants) becomes one search result carrying the
-- API's magnet — the infohash and announce trackers ride inside it,
-- so the engine ingests it directly and the passkey-gated .torrent
-- file endpoint (/anime/torrents/{hash}/file needs the profile
-- passkey) is never touched.
--
-- The torrent ENGINE stays Go (the owner ruling on this migration):
-- this script serves SEARCH only. The factory wraps the loaded
-- provider in the Go torrent adapter (luaTorrentProvider) the moment
-- the script declares torrent = true — episode listing (engine
-- ingest + bounded metadata wait) and stream resolution (the
-- loopback server) remain the shared TorrentBase legs, byte-identical
-- to the compiled provider's. The episodes/streams stubs below exist
-- only to satisfy the script contract's mandatory shape and fail
-- loud: a bare-script load without the adapter can never fake
-- success.
--
-- Known walls (typed via anicli.fail / the transport markers):
--   - an empty or whitespace-only query → invalid_input before any
--     network leaves the process (the compiled provider's guard);
--   - a non-array search body (error envelope) → extract_failed;
--   - HTML instead of JSON → the http.get_json decode raise;
--   - a release whose torrent list answers the typed 404 contributes
--     nothing (the API geo-hides content per requester IP —
--     live-verified: Dandadan's torrents from a RU exit — while the
--     release search still carries the stub); any OTHER transport
--     failure on a release fails the whole search loud.
--
-- Documented divergences from the compiled provider:
--   - base_url is the SITE root (the anilibria streaming precedent);
--     the compiled provider reported the /api/v1 API root. The
--     harness rewrites exactly this literal.
--   - the quality fallback ports only the RESOLUTION badge slice of
--     the engine's ParseQuality ("1080p", "?" when the name carries
--     none): the API's quality.value has been present on every
--     captured entry, the remaining ParseQuality slices (episode
--     numbers, source, codec) are engine-side and were never consumed
--     by the search mapping.

local base_url = "https://aniliberty.top"
local api = base_url .. "/api/v1"

-- RELEASE_PROBE_LIMIT caps how many release-search hits are probed
-- for torrents. Broad queries match dozens of releases and each probe
-- is a sequential API call — six top hits bound the search latency
-- while every practical query still yields torrent results (the
-- compiled provider's AniLibriaTorrentSearchReleaseLimit).
local RELEASE_PROBE_LIMIT = 6

-- INFOHASH_HEX_LEN is the BitTorrent v1 infohash length in hex chars
-- (40) — the shared btih contract a usable torrent link must meet.
local INFOHASH_HEX_LEN = 40

local RESOLUTIONS = { "2160", "1440", "1080", "720", "480", "360" }

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
-- parameter decides — the compiled magnetURI contract.
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

-- torrent_link picks one entry's link: the magnet field verbatim when
-- it carries a usable infohash, a magnet built from the hash field
-- otherwise, nil when neither is present (the entry is dropped, never
-- handed downstream as a dead result).
local function torrent_link(label, magnet, hash)
	if magnet_uri(magnet) then
		return magnet
	end
	local h = string.lower(trim(hash or ""))
	if is_hex40(h) then
		return "magnet:?xt=urn:btih:" .. h .. "&dn=" .. anicli.http.query_escape(label)
	end
	return nil
end

-- human_bytes renders a byte count in binary units with one decimal —
-- the TUI torrent suffix convention ("16.3 GiB"; the compiled
-- humanBytes port, moved to the shared torrent base for its Go
-- consumers when this migration landed).
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

-- quality_badge is the resolution slice of the engine's ParseQuality
-- badge fallback: the leftmost "NNNNp" token wins (list order breaks
-- same-position ties, the Go alternation semantics), the "WxH" form
-- resolves the same way, "?" when the name carries neither.
local function quality_badge(title)
	local lower = string.lower(title)
	local best, best_at
	for _, res in ipairs(RESOLUTIONS) do
		local at = string.find(lower, "%f[%w]" .. res .. "p%f[%W]")
		if at and (best == nil or at < best_at) then
			best, best_at = res, at
		end
	end
	if best == nil then
		for _, res in ipairs(RESOLUTIONS) do
			local at = string.find(lower, "%d%s*[x×]%s*" .. res .. "%f[%D]")
			if at and (best == nil or at < best_at) then
				best, best_at = res, at
			end
		end
	end
	if best == nil then
		return "?"
	end
	return best .. "p"
end

-- release_torrents fetches one release's torrent list by numeric id.
-- The typed not-found is the documented geo-hidden release (see the
-- header): it contributes nothing. Any other transport failure fails
-- the whole search loud — the compiled provider's error policy.
local function release_torrents(release_id)
	local ok, resp = pcall(anicli.http.get_json,
		api .. "/anime/torrents/release/" .. string.format("%d", release_id))
	if not ok then
		if string.match(tostring(resp), "anicli:not_found:") then
			return {}
		end
		error(resp)
	end
	if type(resp) ~= "table" or (resp[1] == nil and next(resp) ~= nil) then
		anicli.fail("extract_failed", "torrent list for release "
			.. string.format("%d", release_id) .. " is not a torrent array")
	end
	return resp
end

-- torrent_result maps one API torrent onto a search result row, or
-- nil when the entry has no usable title or no usable torrent link.
local function torrent_result(tr)
	if type(tr) ~= "table" then
		return nil
	end
	local title = trim(tr.label or "")
	if title == "" then
		return nil
	end
	local link = torrent_link(title, tr.magnet or "", tr.hash or "")
	if link == nil then
		return nil
	end
	local quality = trim((type(tr.quality) == "table" and tr.quality.value) or "")
	if quality == "" then
		quality = quality_badge(title)
	end
	return {
		title = title,
		url = link,
		meta = {
			size = human_bytes(tr.size or 0),
			seeders = string.format("%d", tr.seeders or 0),
			leechers = string.format("%d", tr.leechers or 0),
			quality = quality,
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
	id = "anilibria-torrent",
	name = "АниЛибрия (торренты)",
	base_url = base_url,
	content_lang = "ru",
	torrent = true,

	search = function(query)
		-- Empty queries are a caller bug: reject before any network.
		if trim(query) == "" then
			anicli.fail("invalid_input", "пустой поисковый запрос")
		end

		local items = anicli.http.get_json(api .. "/app/search/releases?query=" .. anicli.http.query_escape(query))

		-- The search endpoint answers with an ARRAY of releases; an
		-- error envelope decodes to a keyed table with no [1] — the
		-- typed wall (the anilibria streaming script's guard).
		if type(items) ~= "table" or (items[1] == nil and next(items) ~= nil) then
			anicli.fail("extract_failed", "search response is not a release array")
		end

		local results = {}
		local probed = 0
		for _, item in ipairs(items) do
			if probed >= RELEASE_PROBE_LIMIT then
				break
			end
			if type(item) == "table" and item.id ~= nil then
				probed = probed + 1
				local torrents = release_torrents(item.id)
				for _, tr in ipairs(torrents) do
					local row = torrent_result(tr)
					if row then
						results[#results + 1] = row
					end
				end
			end
		end
		return filter_seedless(results)
	end,

	-- Structurally unreachable through the roster (the Go torrent
	-- adapter owns these legs — see the header); loud on any bare
	-- path, never a faked success.
	episodes = function(anime_url)
		anicli.fail("invalid_input", "anilibria-torrent serves episodes through the Go torrent adapter, not the script (got "
			.. tostring(anime_url) .. ")")
	end,

	streams = function(episode_url, dub)
		anicli.fail("invalid_input", "anilibria-torrent resolves streams through the Go torrent adapter, not the script (got "
			.. tostring(episode_url) .. ", dub " .. tostring(dub) .. ")")
	end,
}
