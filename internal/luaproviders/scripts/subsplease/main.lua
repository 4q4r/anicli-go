-- SubsPlease (subsplease.org) — the EN seasonal TORRENT provider, the
-- PR144 Lua migration of the compiled Go provider (PR89). The
-- whole-catalog JSON API is the same one the site's own JavaScript
-- drives:
--
--   GET /api/?f=search&s=<query>&tz=0   → matching releases, newest
--                                         first (≤30), a JSON OBJECT
--                                         keyed by release display name
--   GET /shows/<slug>/                  → the show page; its
--                                         show-release-table tag
--                                         carries sid="…" — the only
--                                         slug→sid hop
--   GET /api/?f=show&sid=<id>&tz=0      → {"batch":…, "episode":…}
--
-- The tz parameter is REQUIRED on every /api/ leg: without it the
-- endpoint answers HTTP 200 with zero bytes (live-verified 2026-09-23
-- and again 2026-10-06), which would decode as a silent empty
-- catalog. The site's RSS feeds are latest-only and queryless — the
-- API is the search.
--
-- THE ENGINE BOUNDARY (the PR144 owner ruling): only the SEARCH
-- surface migrated. The torrent engine (internal/torrent, the
-- anacrolix core) stays Go, and the roster slot serves the
-- search-in-Lua hybrid: this script surfaces the tracker-rich magnet
-- links, TorrentBase ingests them and resolves playback. The
-- mandatory episodes/streams functions below are the typed wall
-- naming the engine — a pure-Lua resolve would fake the chain.
--
-- Result shape (byte-faithful with the compiled provider):
--   - title: the magnet dn= display name (URL-decoded, trimmed), or
--     the synthesized "<show> - <episode> (<res>p)" fallback so the
--     list never renders an empty row;
--   - url: the API magnet VERBATIM — the engine owns magnets. The
--     base32 xt= btih is engine-ingestable as-is (anacrolix accepts
--     40-hex and 32-base32 encodings); the per-download torrent=
--     view-page URLs on the 504-flaky upstream host are deliberately
--     not taken;
--   - meta.quality: the resolution badge parsed from the title
--     ("1080p", "?" when unknown); meta.size: the humanized xl= byte
--     length ("369.2 MiB");
--   - downloads without an engine-ingestable magnet (missing magnet,
--     missing or malformed btih) are dropped, like the compiled
--     provider dropped them;
--   - order: the wire is newest-first and the caps below are only
--     honest when the head is the newest — top_level_entries
--     preserves the wire key order the Lua table cannot keep.
--
-- The no-match answer is the literal "[]" (the PHP empty-assoc quirk:
-- empty results serialize as an array, populated ones as an object)
-- and settles as zero results; an EMPTY body (the tz-less answer) and
-- structurally broken JSON fail loud.
--
-- Route note (live 2026-10-06): both routes answer anonymously — the
-- three search legs take ~0.3s direct and ~1-1.5s through the
-- configured proxy; the parity smoke keeps the proxy for the
-- engine-resolve legs (the route matrix's honest route).

local base_url = "https://subsplease.org"

-- The surfaced-surface caps (the compiled provider's constants): 18
-- episode results (the newest releases × resolutions) plus 6 batch
-- results keep the list and the smoke's resolve surface predictable;
-- the batch expansion walks the TOP matched show only.
local EPISODE_CAP = 18
local BATCH_CAP = 6

-- The show-page sid hop pattern (the live tag: <table
-- id="show-release-table" cellpadding="0" … sid="87">); capture 2 is
-- the id.
local SID_RE = 'id="show-release-table"[^>]*\\ssid="(\\d+)"'

-- The PR35 quality badge patterns: the resolution token wins, the
-- width×height form answers when no p-suffix is present.
local RESOLUTION_P_RE = "(?i)\\b(2160|1440|1080|720|480|360)p\\b"
local RESOLUTION_WH_RE = "\\b\\d{3,4}\\s*[x×]\\s*(2160|1440|1080|720|480|360)\\b"

-- str reads an optional string field as "" (the JSON zero-value
-- semantics; tostring(nil) would fabricate "nil").
local function str(value)
	if type(value) == "string" then
		return value
	end
	return ""
end

-- trim mirrors Go strings.TrimSpace over the whitespace set the
-- payloads carry.
local function trim(s)
	return (s:gsub("^%s+", ""):gsub("%s+$", ""))
end

-- first_wire_byte returns the first non-space byte of one body, or ""
-- when the body is empty (the tz-less zero-byte answer).
local function first_wire_byte(body)
	return string.match(body, "^%s*(%S)") or ""
end

-- top_level_entries scans one raw JSON object body and returns its
-- depth-1 entries in WIRE order: the decoded key plus the raw value
-- slice. Both the Go map decode and the Lua table lose object key
-- order, but the wire order IS the contract: it is newest-first and
-- the result caps are only honest when the head is the newest (the
-- compiled provider rode a token decoder for the same reason). The
-- walk tracks string/escape state and bracket depth; a depth-1 string
-- counts as a key only when the next non-space byte is the mapping
-- colon, so scalar values never masquerade as keys.
local function top_level_entries(body)
	local entries = {}
	local depth = 0
	local in_str = false
	local esc = false
	local tok_start = nil
	local phase = "key" -- what the next depth-1 string is: key | colon_seen | value
	local value_start = nil
	local n = #body
	local i = 1
	while i <= n do
		local b = string.byte(body, i)
		if in_str then
			if esc then
				esc = false
			elseif b == 92 then
				esc = true
			elseif b == 34 then
				in_str = false
				if depth == 1 and tok_start ~= nil then
					local raw = string.sub(body, tok_start, i)
					tok_start = nil
					if phase == "key" then
						local j = i + 1
						while j <= n do
							local c = string.byte(body, j)
							if c ~= 32 and c ~= 9 and c ~= 10 and c ~= 13 then
								break
							end
							j = j + 1
						end
						if j <= n and string.byte(body, j) == 58 then
							local ok, decoded = pcall(anicli.json.decode, raw)
							if not ok then
								error("decode releases: malformed object key", 0)
							end
							entries[#entries + 1] = { key = decoded }
							phase = "colon_seen"
						end
					elseif phase == "value" then
						entries[#entries].value = raw
						phase = "key"
					end
				end
			end
		elseif b == 34 then
			in_str = true
			if depth == 1 and tok_start == nil and (phase == "key" or phase == "value") then
				tok_start = i
			end
		elseif b == 123 or b == 91 then
			depth = depth + 1
			if depth == 2 and phase == "value" and value_start == nil then
				value_start = i
			end
		elseif b == 125 or b == 93 then
			if depth == 2 and value_start ~= nil then
				entries[#entries].value = string.sub(body, value_start, i)
				value_start = nil
				phase = "key"
			elseif depth == 1 and value_start ~= nil then
				-- A scalar value before the object's closing brace.
				entries[#entries].value = string.sub(body, value_start, i - 1)
				value_start = nil
				phase = "key"
			end
			depth = depth - 1
		elseif b == 58 and depth == 1 and phase == "colon_seen" then
			phase = "value"
		elseif b == 44 and depth == 1 then
			if phase == "value" and value_start ~= nil then
				-- A comma closes a scalar value slice.
				entries[#entries].value = string.sub(body, value_start, i - 1)
				value_start = nil
				phase = "key"
			end
		elseif depth == 1 and phase == "value" and value_start == nil
			and b ~= 32 and b ~= 9 and b ~= 10 and b ~= 13 then
			value_start = i
		end
		i = i + 1
	end
	return entries
end

-- decode_releases settles one /api/ release-map body into the
-- wire-ordered {title, release} list. The no-match answer is the
-- literal "[]" (the PHP empty-assoc quirk) and settles empty; an
-- empty body and anything that is not an object fail loud — the
-- tz-less zero-byte answer must never read as a healthy catalog.
local function decode_releases(body, what)
	local first = first_wire_byte(body)
	if first == "" then
		error("decode " .. what .. ": empty response body (the tz parameter is mandatory on every /api/ leg)", 0)
	end
	if first == "[" then
		return {} -- the documented no-match answer
	end
	if first ~= "{" then
		error("decode " .. what .. ": release payload opens with " .. first .. ", want an object", 0)
	end
	local out = {}
	for _, entry in ipairs(top_level_entries(body)) do
		local ok, rel = pcall(anicli.json.decode, entry.value)
		if not ok or type(rel) ~= "table" then
			error("decode " .. what .. ": release " .. str(entry.key) .. " did not decode into an object", 0)
		end
		out[#out + 1] = { title = entry.key, release = rel }
	end
	return out
end

-- query_unescape decodes one application/x-www-form-urlencoded value
-- (the magnet parameter convention: '+' is a space, %xx is the byte).
-- A malformed escape returns nil — the compiled provider's strict
-- query parse failed the whole magnet, and surfacing a garbled title
-- would be worse than dropping it.
local function query_unescape(s)
	local plain = s:gsub("+", " ")
	local escaped = false
	local out = plain:gsub("%%(%x%x)", function(h)
		escaped = true
		return string.char(tonumber(h, 16))
	end)
	if escaped then
		-- A leftover % means a malformed escape the pattern skipped.
		if string.find(out, "%%", 1, true) then
			return nil
		end
	elseif string.find(plain, "%%", 1, true) then
		return nil
	end
	return out
end

-- path_escape escapes one show slug for the /shows/ URL (Go's
-- url.PathEscape: the unreserved set rides verbatim, every other byte
-- rides %xx).
local function path_escape(s)
	return (s:gsub("[^%w%-_%.~]", function(c)
		return string.format("%%%02X", string.byte(c))
	end))
end

-- parse_magnet extracts the pieces one result needs from a magnet
-- link: the dn display name and the xl byte length. The link is
-- engine-ingestable only when xt carries a v1 btih hash (32 base32 or
-- 40 hex chars — the API issues base32; anacrolix accepts both), the
-- exact gate the compiled provider's metainfo.ParseMagnetUri applied.
-- Returns nil for a magnet the engine could not ingest.
local function parse_magnet(link)
	if string.sub(link, 1, 8) ~= "magnet:?" then
		return nil
	end
	local xt, dn, xl
	for pair in string.gmatch(string.sub(link, 9), "[^&]+") do
		local raw_name, value = string.match(pair, "^([^=]*)=(.*)$")
		if raw_name ~= nil then
			local name = query_unescape(raw_name)
			if name == "xt" and xt == nil then
				xt = value
			elseif name == "dn" and dn == nil then
				dn = value
			elseif name == "xl" and xl == nil then
				xl = value
			end
		end
	end
	if xt == nil then
		return nil
	end
	local hash = string.match(xt, "^urn:btih:(.+)$")
	if hash == nil then
		return nil
	end
	local base32 = #hash == 32 and string.match(hash, "^[A-Z2-7]+$") ~= nil
	local hex = #hash == 40 and string.match(hash, "^%x+$") ~= nil
	if not base32 and not hex then
		return nil
	end
	local display = ""
	if dn ~= nil then
		local decoded = query_unescape(dn)
		if decoded == nil then
			return nil
		end
		display = trim(decoded)
	end
	local size_raw = ""
	if xl ~= nil then
		local decoded = query_unescape(xl)
		if decoded == nil then
			return nil
		end
		size_raw = trim(decoded)
	end
	return { display = display, size_raw = size_raw }
end

-- quality_badge renders the PR35 quality badge of one release title:
-- the resolution token ("1080p"), "?" when the title carries none.
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

-- human_bytes renders a byte count the torrent-suffix way
-- (1024-based, one decimal, the KiB/MiB/GiB ladder — the compiled
-- provider's humanBytes renderer); non-numeric input stays verbatim
-- (fail-soft: the feed owns the format).
local function human_bytes(raw)
	local n = tonumber(raw, 10)
	if n == nil then
		return raw
	end
	local unit = 1024
	if n < unit then
		return string.format("%d B", n)
	end
	local div, exp = unit, 0
	local m = math.floor(n / unit)
	while m >= unit do
		div = div * unit
		exp = exp + 1
		m = math.floor(m / unit)
	end
	return string.format("%.1f", n / div) .. " " .. string.sub("KMGTPE", exp + 1, exp + 1) .. "iB"
end

-- append_release_results appends one result per usable download of
-- the release, stopping at limit. A download without an
-- engine-ingestable magnet is dropped; a release left with none
-- contributes nothing.
local function append_release_results(rel, out, limit)
	local downloads = rel.downloads
	if type(downloads) ~= "table" then
		return
	end
	for _, dl in ipairs(downloads) do
		if #out >= limit then
			return
		end
		if type(dl) == "table" then
			local magnet = trim(str(dl.magnet))
			if magnet ~= "" then
				local parsed = parse_magnet(magnet)
				if parsed ~= nil then
					local title = parsed.display
					if title == "" then
						title = str(rel.show) .. " - " .. str(rel.episode) .. " (" .. str(dl.res) .. "p)"
					end
					local meta = { quality = quality_badge(title) }
					if parsed.size_raw ~= "" then
						meta.size = human_bytes(parsed.size_raw)
					end
					out[#out + 1] = { title = title, url = magnet, meta = meta }
				end
			end
		end
	end
end

-- top_page returns the first non-empty show slug of the wire-ordered
-- releases (the top match), bounded to one expansion.
local function top_page(releases)
	for i, entry in ipairs(releases) do
		if i > 1 then
			return ""
		end
		local slug = trim(str(entry.release.page))
		if slug ~= "" then
			return slug
		end
	end
	return ""
end

-- show_batches walks the back-catalog hop: show page (the sid
-- attribute) → f=show payload → the batch release entries. Every leg
-- failure raises — the caller fail-softs.
local function show_batches(slug)
	local page_resp = anicli.http.get(base_url .. "/shows/" .. path_escape(slug) .. "/")
	local sid = anicli.regexp.match(SID_RE, page_resp.body)
	if sid == nil then
		error("show page: no show-release-table sid", 0)
	end
	local show_resp = anicli.http.get(base_url .. "/api/?f=show&sid=" .. sid[2] .. "&tz=0")
	local envelope = show_resp.body
	if first_wire_byte(envelope) ~= "{" then
		error("show payload opens with " .. str(first_wire_byte(envelope)) .. ", want an object", 0)
	end
	-- The batch object is sliced RAW out of the envelope so its wire
	-- order survives the round-trip.
	for _, entry in ipairs(top_level_entries(envelope)) do
		if entry.key == "batch" then
			return decode_releases(entry.value, "batch releases")
		end
	end
	error("show payload carries no batch object", 0)
end

return {
	id = "subsplease",
	name = "SubsPlease",
	base_url = base_url,
	capabilities = "both",
	torrent = true,
	content_lang = "ja",
	-- The index matches EN/romaji release names only — the search
	-- fan-out must route it the latin variants, never the Cyrillic
	-- ones (PR42 semantics).
	name_preference = "latin",
	-- The declared smoke probe (PR51): the shared "black lagoon"
	-- probe is outside the EN-seasonal catalog entirely (f=search
	-- answers the literal "[]" there, live-verified), so the provider
	-- speaks for itself. Re Zero is a multi-cour flagship: its
	-- episode AND batch releases stay in the searchable catalog for
	-- the long haul.
	smoke_query = "re:zero",

	search = function(query)
		-- Empty queries are a caller bug: reject before any network
		-- I/O.
		if trim(query) == "" then
			anicli.fail("invalid_input", "пустой поисковый запрос")
		end

		local resp = anicli.http.get(base_url .. "/api/?f=search&s=" ..
			anicli.http.query_escape(query) .. "&tz=0")
		local releases = decode_releases(resp.body, "releases")

		-- The matched episode releases first (wire-order head, capped).
		local results = {}
		for _, entry in ipairs(releases) do
			if #results >= EPISODE_CAP then
				break
			end
			append_release_results(entry.release, results, EPISODE_CAP)
		end

		-- Batch back-catalog expansion (fail-soft): the top matched
		-- show's page keys its f=show payload; batches surface AFTER
		-- the episode results under their own cap. Any hop failure is
		-- a logged skip, never a failed search.
		local slug = top_page(releases)
		if slug ~= "" then
			local ok, batches = pcall(show_batches, slug)
			if not ok then
				anicli.log.info("subsplease: batch expansion skipped for " ..
					slug .. ": " .. tostring(batches))
			else
				local batch_start = #results
				for _, entry in ipairs(batches) do
					if #results >= batch_start + BATCH_CAP then
						break
					end
					append_release_results(entry.release, results, batch_start + BATCH_CAP)
				end
			end
		end
		return results
	end,

	-- The engine boundary: torrent links resolve through the Go
	-- torrent core, never in-script. The roster serves the
	-- search-in-Lua hybrid whose GetEpisodes/ResolveStream ride
	-- TorrentBase; these mandatory functions exist for the loader's
	-- contract alone and fail loud naming the engine.
	episodes = function(anime_url)
		anicli.fail("extract_failed",
			"torrent links resolve through the Go torrent engine (the search-in-Lua hybrid), not in-script")
	end,

	streams = function(episode_url, dub)
		anicli.fail("extract_failed",
			"torrent links resolve through the Go torrent engine (the search-in-Lua hybrid), not in-script")
	end,
}
