-- AniPub (anipub.xyz) — the PR139 Lua migration of the compiled Go
-- provider (internal/providers/anipub.go, PR107). The catalog is an
-- open Express+Mongo API (github.com/AnimePub/AniPub, the site's own
-- source; the api. subdomain is a static GitHub Pages site — the real
-- API rides the apex host). Anonymous on every leg: the validkey
-- middleware guarding /api/info calls next() on a missing key (the
-- rejection branch is commented out in the site's backend source),
-- and search plus /v1/api/details are unguarded outright.
--
-- Shapes:
--   - search: GET /api/searchAll/<query> — a paged name search over
--     the MongoDB Name index (>= 3 chars, 20 per page). The query is
--     ONE path segment, percent-encoded the url.PathEscape way
--     (spaces as %20 — the form the live server answers; the "/"
--     escape keeps multi-segment queries inside the :query param).
--     The no-hit body is {"found":false} (same shape the short-query
--     guard returns) — a clean empty result, not an error.
--   - episodes: GET /v1/api/details/<id> — the release doc with the
--     ep array. Each episode carries its stream link as "src=<url>"
--     (the schema's _id/name/title fields ride empty in the
--     captures); the numbering is the array order, and a linkless
--     slot contributes nothing. TV releases fill ep[]; MOVIES and
--     specials leave ep[] EMPTY and carry the stream on the doc-level
--     link field instead (live-verified: Cowboy Bebop: The Movie
--     1387 and Session XX 7152). Every link is the site's own
--     /video/<n>/<sub|dub> player page; the host normalizes onto the
--     provider base (the catalog data carries both the apex and the
--     www host, both serving the identical player page).
--   - streams: the /video player page's single iframe embeds a
--     megaplay.buzz stream page whose data-id feeds the same-origin
--     /stream/getSourcesNew endpoint, whose enc payload decrypts
--     (static AES-256-CBC parameters) to the master.m3u8. Megaplay
--     hotlink-gates the stream page on the embedding site's Referer
--     (no Referer answers its Error shell, no data-id), and the CDN
--     403s playback without the stream-origin Referer, so that rides
--     on the resolved source. Sub and Dub emit per episode — the
--     site's own player toggles the flavors through its
--     changeStreamType rewrite (the type= query form and the
--     /video/<n>/<lang> path form), so the fresh-sandbox state
--     channel (raw_id, the sameband/anidub single-value precedent)
--     carries the catalog-flavor player URL and the requested dub's
--     URL is re-derived through the same grammar.
--
-- Route note (2026-10-06): the honest route is the proxy —
-- «cowboy bebop» answers 3 cards and the full resolve chain lands
-- through the configured proxy (the parity smoke command passes
-- --proxy; the direct route is not this catalog's path).
--
-- Crypto material: the megaplay AES-256-CBC decrypt rides the
-- anicli.crypto SDK primitive (Go stdlib behind it) — no cipher is
-- reimplemented in the script, the anikoto precedent.

local base_url = "https://anipub.xyz"

-- The megaplay AES-256-CBC parameters, lifted verbatim from
-- megaplay.buzz/lib/newclient.min.js (SegmentDecrypt key import +
-- decryptToken IV; live-verified 2026-09-25 and re-verified
-- 2026-10-06 against a fresh enc capture — it decrypts to the pinned
-- master.m3u8 URL). The key string is zero-padded to the 32-byte
-- AES-256 length exactly like the site's client
-- (new Uint8Array(32).set(key.subarray(0, 32))).
local AES_KEY = "i?LMTAx0Q6,:}50U"
local AES_IV = "W0;27ToaUpl_P%'c"

-- pad_bytes truncates or zero-pads s to exactly n bytes (the JS
-- Uint8Array semantics the site's client imports the key with).
local function pad_bytes(s, n)
	if #s >= n then
		return string.sub(s, 1, n)
	end
	return s .. string.rep("\0", n - #s)
end

local function is_hex_digit(c)
	return (c >= 48 and c <= 57) or (c >= 97 and c <= 102) or (c >= 65 and c <= 70)
end

-- path_segment_quote ports url.PathEscape (Go's encodePathSegment
-- mode): the URL-unreserved set plus the segment-allowed reserved
-- punctuation ($&+=:@) ride unescaped; everything else — spaces, "/",
-- ";", ",", "?", control bytes, non-ASCII — is percent-encoded
-- uppercase, one UTF-8 byte at a time. The form-style "+" of
-- http.query_escape would not answer on this API.
local function path_segment_quote(s)
	local out = {}
	for i = 1, #s do
		local c = string.sub(s, i, i)
		if string.match(c, "^[A-Za-z0-9%-%.~%$%&%+%=%:%@]$") then
			out[#out + 1] = c
		else
			out[#out + 1] = string.format("%%%02X", string.byte(s, i))
		end
	end
	return table.concat(out)
end

-- first_at reports the leftmost plain occurrence of either needle in
-- the lowercased copy (Go's regexp leftmost match of the
-- (?i)type=(sub|dub) and (?i)/(sub|dub) alternations; the byte length
-- is unchanged by the lowercasing, so offsets carry over).
local function first_at(low, a, b)
	local pa = string.find(low, a, 1, true)
	local pb = string.find(low, b, 1, true)
	if pa and pb then
		return math.min(pa, pb)
	end
	return pa or pb
end

-- change_lang ports the site's changeStreamType (app.js): both
-- stream-link spellings carry their lang — the type= query form (the
-- doc-level gogo link) and the /video/<n>/<lang> path form (every
-- captured ep link). No match returns the link unchanged.
local function change_lang(link, lang)
	if string.find(link, "type=", 1, true) then
		local at = first_at(string.lower(link), "type=sub", "type=dub")
		if not at then
			return link
		end
		return string.sub(link, 1, at - 1) .. "type=" .. lang .. string.sub(link, at + 8)
	end
	local at = first_at(string.lower(link), "/sub", "/dub")
	if not at then
		return link
	end
	return string.sub(link, 1, at - 1) .. "/" .. lang .. string.sub(link, at + 4)
end

-- url_bytes_ok rejects the shapes url.Parse refuses: ASCII control
-- characters anywhere and malformed percent-escapes. The escapes
-- themselves are validated once here so the path decoder below can
-- translate them unchecked.
local function url_bytes_ok(raw)
	local i, n = 1, #raw
	while i <= n do
		local c = string.byte(raw, i)
		if c < 0x20 or c == 0x7f then
			return false
		end
		if c == 37 then
			if i + 2 > n
				or not is_hex_digit(string.byte(raw, i + 1))
				or not is_hex_digit(string.byte(raw, i + 2)) then
				return false
			end
			i = i + 3
		else
			i = i + 1
		end
	end
	return true
end

-- url_path extracts the path component the way url.Parse's Path saw
-- it: the scheme and authority dropped, everything before the first
-- "?" or "#" kept, percent-escapes decoded (the query and fragment
-- off).
local function url_path(raw)
	local at = string.find(raw, "[?#]")
	local path = at and string.sub(raw, 1, at - 1) or raw
	local scheme_at = string.match(path, "^%a+://")
	if scheme_at then
		local host_end = string.find(path, "/", #scheme_at + 1, true)
		path = host_end and string.sub(path, host_end) or ""
	end
	local out = {}
	local i, n = 1, #path
	while i <= n do
		if string.byte(path, i) == 37 and i + 2 <= n then
			out[#out + 1] = string.char(tonumber(string.sub(path, i + 1, i + 2), 16))
			i = i + 3
		else
			out[#out + 1] = string.sub(path, i, i)
			i = i + 1
		end
	end
	return table.concat(out)
end

-- parse_stream_ref strips the "src=" wrapper and pulls the
-- /video/<n>/<lang> player path off a catalog link. The host is
-- DROPPED — the caller re-anchors the path onto the provider base
-- (the anikado host-normalization precedent). "" and foreign shapes
-- parse as nil; empty path segments split faithfully (a "video//n"
-- link is the compiled provider's reject too).
local function parse_stream_ref(link)
	if string.sub(link, 1, 4) ~= "src=" then
		return nil
	end
	local raw = string.sub(link, 5)
	if raw == "" or not url_bytes_ok(raw) then
		return nil
	end
	local path = url_path(raw)
	local trimmed = string.gsub(string.gsub(path, "^/+", ""), "/+$", "")
	local segs = {}
	local start = 1
	while true do
		local slash = string.find(trimmed, "/", start, true)
		if slash then
			segs[#segs + 1] = string.sub(trimmed, start, slash - 1)
			start = slash + 1
		else
			segs[#segs + 1] = string.sub(trimmed, start)
			break
		end
	end
	if #segs ~= 3 or segs[1] ~= "video" or segs[2] == "" or segs[3] == "" then
		return nil
	end
	return path
end

-- decrypt_sources decrypts the getSourcesNew enc payload: URL-safe
-- base64 (padded) → AES-256-CBC with the static parameters above →
-- {"file": <master.m3u8>}. An empty payload (the track-only answer a
-- bad CDN selector or unknown type gets) and an undecryptable or
-- file-less payload are the typed extract wall.
local function decrypt_sources(encoded)
	local pad = #encoded % 4
	if pad ~= 0 then
		encoded = encoded .. string.rep("=", 4 - pad)
	end
	local ct = anicli.base64.url_decode(encoded)
	local ok, plain = pcall(anicli.crypto.aes_cbc_decrypt,
		pad_bytes(AES_KEY, 32), pad_bytes(AES_IV, 16), ct)
	if not ok then
		anicli.fail("extract_failed", "decrypt enc payload: " .. tostring(plain))
	end
	local decoded_ok, decoded = pcall(anicli.json.decode, plain)
	if not decoded_ok then
		anicli.fail("extract_failed", "decode decrypted sources: " .. tostring(decoded))
	end
	if type(decoded) == "table" and (decoded.file or "") ~= "" then
		return decoded.file
	end
	anicli.fail("extract_failed", "decrypted sources carry no file")
end

-- episode_row folds one parsed player path into the episode shape:
-- num, the raw_id state (the catalog-flavor /video URL) and both dub
-- rows through the changeStreamType rewrite.
local function episode_row(num, video_url)
	return {
		num = num,
		raw_id = video_url,
		raw_embeds = {
			Sub = { change_lang(video_url, "sub") },
			Dub = { change_lang(video_url, "dub") },
		},
	}
end

return {
	id = "anipub",
	name = "AniPub",
	base_url = base_url,
	capabilities = "both",
	content_lang = "en",
	-- The catalog's search index matches the Latin Name field only —
	-- a Cyrillic query is guaranteed-zero.
	name_preference = "latin",
	-- The declared live probe: the shared RU probe misses this EN-only
	-- name index, so the provider speaks for itself («cowboy bebop»
	-- surfaces 3 releases, cheap for the whole-surface smoke).
	smoke_query = "cowboy bebop",

	search = function(query)
		local resp = anicli.http.get(base_url .. "/api/searchAll/" .. path_segment_quote(query))
		local env = anicli.json.decode(resp.body)
		local rows = type(env) == "table" and env.AniData or nil
		if type(rows) ~= "table" then
			return {}
		end
		local results = {}
		for _, item in ipairs(rows) do
			results[#results + 1] = {
				title = tostring(item.Name or ""),
				url = tostring(item._id),
				poster = tostring(item.ImagePath or ""),
			}
		end
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(base_url .. "/v1/api/details/" .. path_segment_quote(anime_url))
		local env = anicli.json.decode(resp.body)
		-- `local` is a Lua keyword: the doc field needs the bracket
		-- spelling.
		local doc = type(env) == "table" and env["local"] or nil

		local eps = {}
		if type(doc) == "table" and type(doc.ep) == "table" then
			for i, ep in ipairs(doc.ep) do
				local path = parse_stream_ref(ep.link or "")
				if path then
					eps[#eps + 1] = episode_row(tostring(i), base_url .. path)
				end
			end
		end
		if #eps == 0 and type(doc) == "table" then
			-- Movie/special shape: no ep array, the stream on the doc
			-- link — one synthetic episode (live-verified 2026-09-25).
			local path = parse_stream_ref(doc.link or "")
			if path then
				eps[#eps + 1] = episode_row("1", base_url .. path)
			end
		end
		if #eps == 0 then
			anicli.fail("not_found", "details for " .. anime_url .. " carry no episode links")
		end
		return eps
	end,

	streams = function(raw_id, dub)
		local lang = string.lower(dub)
		if lang ~= "sub" and lang ~= "dub" then
			anicli.fail("not_found",
				"episode " .. raw_id .. " carries no dub \"" .. dub .. "\"")
		end
		-- Hop 1: the site's own player page (the requested dub's
		-- flavor through the changeStreamType rewrite).
		local video_url = change_lang(raw_id, lang)
		local page = anicli.http.get(video_url)
		-- The src value is UNQUOTED in the captured markup
		-- (`<iframe src=https://megaplay.buzz/... ` — the first real
		-- iframe element; the Cloudflare bootstrap rides script, not
		-- markup). Quoted shapes strip the quotes.
		local m = anicli.regexp.match("(?i)<iframe[^>]+src=(\"([^\"]*)\"|[^>\\s]+)", page.body)
		local src = m and m[2] or ""
		if #src >= 2 and string.sub(src, 1, 1) == "\"" and string.sub(src, -1) == "\"" then
			src = string.sub(src, 2, -2)
		end
		if src == "" then
			anicli.fail("extract_failed",
				"/video page has no player iframe (" .. video_url .. ")")
		end

		-- Hop 2: the megaplay stream page → the numeric file id. The
		-- lang suffix decides the getSourcesNew type param.
		local scheme, host = string.match(src, "^(%a+)://([^/?#]+)")
		if not scheme then
			-- The compiled provider answered this branch plainly (no
			-- typed sentinel) — a non-URL iframe src is a generic
			-- provider error.
			error("parse embed url " .. src .. ": no origin")
		end
		local origin = string.lower(scheme) .. "://" .. host
		local suffix_zone = string.sub(src, #string.match(src, "^%a+://[^/?#]*") + 1)
		local cut = string.find(suffix_zone, "[?#]")
		local embed_path = cut and string.sub(suffix_zone, 1, cut - 1) or suffix_zone
		local last = string.match(string.gsub(string.gsub(embed_path, "^/+", ""), "/+$", ""), "[^/]*$")
		if last ~= "sub" and last ~= "dub" then
			anicli.fail("extract_failed",
				"embed url " .. src .. " carries no sub/dub suffix")
		end
		-- Megaplay hotlink-gates the page on the embedding site's
		-- Referer — without it the page is its Error shell, no
		-- data-id (live-verified 2026-09-25).
		local stream_page = anicli.http.get(src, { headers = { Referer = base_url .. "/" } })
		local fid = anicli.regexp.match("data-id=\"([0-9]+)\"", stream_page.body)
		if not fid then
			anicli.fail("extract_failed",
				"stream page has no data-id (" .. src .. ")")
		end

		-- Hop 3: same-origin getSourcesNew (the client fetches it
		-- relative), decrypt, done. s=bcdn is the CDN selector the
		-- player appends; without it the endpoint answers
		-- track-only, no enc.
		local sources = anicli.http.get(
			origin .. "/stream/getSourcesNew?id=" .. anicli.http.query_escape(fid[2])
				.. "&type=" .. last .. "&s=bcdn",
			{ headers = {
				["X-Requested-With"] = "XMLHttpRequest",
				Referer = src,
			} })
		local payload = anicli.json.decode(sources.body)
		local enc = type(payload) == "table" and payload.enc or ""
		if enc == "" then
			anicli.fail("extract_failed", "getSourcesNew carries no enc payload")
		end
		local manifest = decrypt_sources(enc)

		-- The CDN 403s playback without the stream-origin Referer
		-- (live-verified), so it rides on the source like
		-- kickassanime's.
		return {
			dub_name = dub,
			links = {
				auto = {
					url = manifest,
					quality = "auto",
					type = "m3u8",
					headers = { Referer = origin .. "/" },
				},
			},
		}
	end,
}
