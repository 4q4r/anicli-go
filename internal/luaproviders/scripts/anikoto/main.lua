-- AniKoto (anikototv.to) — the PR127 Lua migration of the compiled Go
-- provider (internal/providers/anikoto.go, PR104). The site is a
-- HiAnime/Zoro-style clone of the anikoto.net platform family (the
-- AniVault-Scraper and the PyPI anikoto downloader document the
-- identical AJAX shape); it was written against the live site and
-- characterized 2026-09-25, re-verified live 2026-10-05 through the
-- configured proxy (the direct route tarpits on that network — the
-- per-provider netclient wiring carries the proxy, as for the other
-- geo-fenced catalogs).
--
-- Shapes:
--   - search: GET /filter?keyword= renders 30 server-side result cards
--     per page (#list-items .item); a junk query answers the same
--     shell with zero cards — an empty list, not an error. Card links
--     carry an /ep-N tail; every result URL is canonicalized onto the
--     bare /watch/{slug} page (both forms carry the watch data-id).
--   - episodes: the watch page carries the anime id (data-id), and
--     GET /ajax/episode/list/{id} answers the {"status":N,"result":…}
--     envelope whose HTML lists the episodes as li[data-html] anchors
--     (data-num, data-id, data-ids, title). RawID composes
--     "{ep data-id}:{data-ids}".
--   - dubs: GET /ajax/server/list?servers={data-ids} answers the same
--     envelope with SUB and DUB server groups (.servers .type[data-
--     type]) of four named servers each, as li[data-link-id] rows.
--     The compiled provider hydrated these lazily per episode through
--     the DubsHydrator capability; the Lua provider contract has no
--     such capability (the session resolves only from the listing's
--     raw_embeds), so the hydration runs EAGERLY per episode — one
--     bounded-parallel batch — the yummy/animedia precedent.
--   - streams: GET /ajax/server?get={link-id} resolves a server to
--     {"result":{"url":…}} where url is a megaplay.buzz player page.
--     The player's e1 bundle is XOR-obfuscated; the script statically
--     unpacks its string table (no JavaScript executes), reads the
--     AES key/IV + HMAC secret, decrypts the getSources enc blob and
--     HMAC-signs the resulting CDN URL. The fresh-sandbox contract
--     (streams receives raw_id and dub only) re-fetches the server
--     list at resolve time — the animedia/yummy state pattern.
--
-- Known walls (typed via anicli.fail, the Go sentinels' equivalents):
--   - The site answers malformed AJAX with HTTP 200 and the error
--     INSIDE the envelope ({"status":500,"result":"Bad request"} — the
--     /api/search path the controller probed is such a decoy). The
--     envelope status surfaces, never the HTTP one.
--   - A rotated or hostile player bundle fails typed at every unpack
--     stage (wrapper, payload length, XOR period, string table,
--     parameters), never by partial output.
--   - The skip_data (intro/outro ranges) the resolver hands out has
--     no slot in the stream contract — documented, out of scope.
--
-- DUB-MISS SEMANTICS (the #159 doctrine port — the animevib
-- fix-round-3 semantics, one format across the state-carrying
-- scripts): streams() NEVER substitutes another dub group. When the
-- requested dub is absent from the episode's server groups, the
-- resolve walls typed not_found whose message LISTS the groups the
-- episode ACTUALLY carries — DUB first, then SUB (the fixed
-- canonical order of the two-group table the parser builds). The
-- stable marker is `carries no dub "X" (episode dubs: A, B)` — the
-- tui dubNotCarriedFailure predicate keys on the class + marker
-- pair, so every sibling's miss opens the ask-first flow; the
-- round-2 shape walled the miss invalid_input (a caller-mistake
-- class the predicate cannot see). A server list with no link rows
-- at all → not_found zero-dubs wall (the same marker, no list).
-- The episode identifier in the message is the raw_id itself: this
-- state carries no episode number. The malformed-raw_id wall (a
-- colonless raw_id, the merged prov:id convention composed without
-- decomposing it, #157) is invalid_input and stays.
--
-- Crypto material: the AES-256-CBC decrypt and the HMAC-SHA256 digest
-- ride the anicli.crypto SDK primitives (Go stdlib behind them — the
-- sandbox has no bit library for a pure-Lua cipher). The XOR wrapper
-- unpack and the JS string decoding are plain byte work, ported below.

local base_url = "https://anikototv.to"

-- The site's own audio axis (the server list groups every embed under
-- data-type="sub" or "dub"): the labels the reference implementations
-- expose.
local AUDIO_SUB = "SUB"
local AUDIO_DUB = "DUB"

-- The known plaintext of every obfuscated bundle's initializer: the
-- repeating XOR key is recovered from this prefix, never by executing
-- the script.
local XOR_PREFIX = "(function(){function "

-- The set decodeURI leaves escaped (JS's decodeURIComponent was NOT
-- used by the obfuscator — reserved URI punctuation stays
-- percent-encoded in the payload).
local RESERVED_PERCENT = ";/?:@&=+$,#"

-- The megaplay master playlist's default height (the m3u8 lib's
-- fallback when a STREAM-INF carries no RESOLUTION).
local DEFAULT_HEIGHT = "1080"

-- Go-regexp patterns the shared SDK regexp bridge evaluates (RE2
-- syntax — the compiled provider's patterns verbatim).
local JS_STR_BODY = "(?:\\\\.|[^\"\\\\])*"
local PARAM_RE = "\\bconst\\s+[\\w$]+\\s*=\"(" .. JS_STR_BODY .. ")\"" ..
	"\\s*(?:,|;\\s*const\\s+)\\s*[\\w$]+\\s*=\"(" .. JS_STR_BODY .. ")\"" ..
	"\\s*(?:,|;\\s*const\\s+)\\s*[\\w$]+\\s*=\"(" .. JS_STR_BODY .. ")\"" ..
	"\\s*(?:,|;\\s*const\\s+)\\s*[\\w$]+\\s*=(\\d+)\\s*;\\s*function\\s+[\\w$]+\\("
local RETURN_EVAL_RE = "\\breturn\\s+eval\\(\"(" .. JS_STR_BODY .. ")\"\\)"
local OBJECT_DECL_RE = "^\\s*let\\s+([A-Za-z_$][\\w$]*)\\s*;"
local STRING_ARRAY_RE = "(?:let|const|var)\\s+[\\w$]+\\s*=\\s*(\\[\\s*(?:\"" ..
	JS_STR_BODY .. "\"\\s*,?\\s*)*\\])"
local JS_LITERAL_RE = "\"(" .. JS_STR_BODY .. ")\""

-- bxor is the one byte-wide XOR the sandbox's arithmetic can express
-- (no bit library): eight bit-plane rounds.
local function bxor(a, b)
	local result, bit = 0, 1
	for _ = 1, 8 do
		local abit, bbit = a % 2, b % 2
		if abit ~= bbit then
			result = result + bit
		end
		a = (a - abit) / 2
		b = (b - bbit) / 2
		bit = bit * 2
	end
	return result
end

local function is_hex_digit(c)
	return (c >= 48 and c <= 57) or (c >= 97 and c <= 102) or (c >= 65 and c <= 70)
end

-- The sandbox's registry bounds table.concat inputs (a 90k-element
-- table overflows the VM stack), so every byte-level rewrite
-- accumulates through a chunked builder: small concat blocks flush
-- into a parts list, and plain-byte runs ride single substrings.
local BUILDER_BLOCK = 2048

local function new_builder()
	return { inner = {}, parts = {}, n = 0 }
end

local function builder_add(b, s)
	b.n = b.n + 1
	b.inner[b.n] = s
	if b.n >= BUILDER_BLOCK then
		b.parts[#b.parts + 1] = table.concat(b.inner)
		b.inner = {}
		b.n = 0
	end
end

local function builder_build(b)
	if b.n > 0 then
		b.parts[#b.parts + 1] = table.concat(b.inner)
	end
	return table.concat(b.parts)
end

-- utf8_encode renders one code point the way Go's rune write did (the
-- parser consumes ASCII crypto identifiers; lone surrogates decode as
-- their raw value — the compiled provider carried the same limitation).
local function utf8_encode(cp)
	if cp < 0x80 then
		return string.char(cp)
	end
	if cp < 0x800 then
		return string.char(0xC0 + math.floor(cp / 0x40), 0x80 + cp % 0x40)
	end
	if cp < 0x10000 then
		return string.char(0xE0 + math.floor(cp / 0x1000),
			0x80 + math.floor(cp / 0x40) % 0x40, 0x80 + cp % 0x40)
	end
	return string.char(0xF0 + math.floor(cp / 0x40000),
		0x80 + math.floor(cp / 0x1000) % 0x40,
		0x80 + math.floor(cp / 0x40) % 0x40, 0x80 + cp % 0x40)
end

-- unquote_js decodes a double-quoted JS string literal's CONTENT as
-- data: \xNN and \uNNNN escapes, JS-only escape sequences, and the
-- obfuscator's literal \r\n pair (dropped). The input excludes the
-- surrounding quotes; a malformed literal contributes its decodable
-- prefix (the compiled provider's best-effort semantics).
local JS_ESCAPES = {
	[98] = "\b", [102] = "\f", [110] = "\n", [114] = "\r",
	[116] = "\t", [118] = "\v", [48] = "\0",
}
local function unquote_js(content)
	local out = new_builder()
	local i, n = 1, #content
	while i <= n do
		local c = string.byte(content, i)
		if c ~= 92 then
			-- Plain run: append everything up to the next escape as one
			-- substring instead of byte-wise entries.
			local run = i
			while run <= n and string.byte(content, run) ~= 92 do
				run = run + 1
			end
			builder_add(out, string.sub(content, i, run - 1))
			i = run
		else
			i = i + 1
			if i > n then
				break
			end
			local e = string.byte(content, i)
			if e == 120 and i + 2 <= n
				and is_hex_digit(string.byte(content, i + 1))
				and is_hex_digit(string.byte(content, i + 2)) then
				builder_add(out, string.char(tonumber(string.sub(content, i + 1, i + 2), 16)))
				i = i + 3
			elseif e == 117 and i + 4 <= n
				and is_hex_digit(string.byte(content, i + 1))
				and is_hex_digit(string.byte(content, i + 2))
				and is_hex_digit(string.byte(content, i + 3))
				and is_hex_digit(string.byte(content, i + 4)) then
				builder_add(out, utf8_encode(tonumber(string.sub(content, i + 1, i + 4), 16)))
				i = i + 5
			elseif e == 13 and i + 1 <= n and string.byte(content, i + 1) == 10 then
				i = i + 2
			elseif e == 10 or e == 13 then
				i = i + 1
			else
				builder_add(out, JS_ESCAPES[e] or string.char(e))
				i = i + 1
			end
		end
	end
	return builder_build(out)
end

-- decode_uri reproduces the obfuscator's decodeURI pass: %XX sequences
-- whose decoded byte is reserved URI punctuation stay escaped,
-- everything else decodes (multi-byte UTF-8 passes through
-- byte-exact).
local function decode_uri(s)
	local out = new_builder()
	local i, n = 1, #s
	while i <= n do
		local c = string.byte(s, i)
		if c == 37 and i + 2 <= n
			and is_hex_digit(string.byte(s, i + 1))
			and is_hex_digit(string.byte(s, i + 2)) then
			local v = tonumber(string.sub(s, i + 1, i + 2), 16)
			if string.find(RESERVED_PERCENT, string.char(v), 1, true) then
				builder_add(out, string.sub(s, i, i + 2))
			else
				builder_add(out, string.char(v))
			end
			i = i + 3
		else
			local run = i
			while run <= n do
				local rc = string.byte(s, run)
				if rc == 37 and run + 2 <= n
					and is_hex_digit(string.byte(s, run + 1))
					and is_hex_digit(string.byte(s, run + 2)) then
					break
				end
				run = run + 1
			end
			builder_add(out, string.sub(s, i, run - 1))
			i = run
		end
	end
	return builder_build(out)
end

-- regexp_iterate walks every match of a Go regexp over s: fn(full,
-- captures, match_offset) runs per match; returning true stops the
-- walk. The SDK bridge exposes first-match-only results, so the walk
-- re-locates each full match verbatim and resumes past it.
local function regexp_iterate(pattern, s, fn)
	local rest = s
	local offset = 0
	while true do
		local m = anicli.regexp.match(pattern, rest)
		if not m then
			return
		end
		local full = m[1]
		local at = string.find(rest, full, 1, true)
		if not at then
			return
		end
		if fn(full, m, offset + at) then
			return
		end
		rest = string.sub(rest, at + #full)
		offset = offset + at + #full - 1
	end
end

local function all_present(table_entries, needles)
	for _, needle in ipairs(needles) do
		local found = false
		for _, v in ipairs(table_entries) do
			if v == needle then
				found = true
				break
			end
		end
		if not found then
			return false
		end
	end
	return true
end

-- unpack_player_strings statically decodes the player's XOR wrapper
-- and string table (the AniVault reference implementation's
-- algorithm, never executing anything): recover the repeating XOR key
-- from the known payload prefix, decode the initializer, find the
-- string-table array whose entries XOR-mask onto the crypto
-- identifiers, and substitute the lookups back into the bundle source.
local function unpack_player_strings(script)
	local wrapper_m = anicli.regexp.match(RETURN_EVAL_RE, script)
	local object_m = anicli.regexp.match(OBJECT_DECL_RE, script)
	if not wrapper_m or not object_m then
		anicli.fail("extract_failed", "unrecognized obfuscated player wrapper")
	end
	local wrapper = unquote_js(wrapper_m[2])
	local payload_quoted = string.match(wrapper, "%}%)%(\"(.*)\"%)%s*$")
	if not payload_quoted then
		anicli.fail("extract_failed", "obfuscated player payload missing")
	end
	local payload = decode_uri(unquote_js(payload_quoted))
	-- A hostile or rotated bundle can match every wrapper shape yet
	-- carry a payload shorter than the known plaintext prefix — that is
	-- a typed failure, never an index error.
	if #payload < #XOR_PREFIX then
		anicli.fail("extract_failed",
			"obfuscated player payload too short (" .. #payload .. " bytes)")
	end

	-- XOR key recovery from the known prefix: the key repeats with a
	-- period that must divide the whole derived key stream. The search
	-- runs to the full prefix length; the prefix+suffix validation
	-- rejects wrong periods typed.
	local known = {}
	for i = 1, #XOR_PREFIX do
		known[i] = bxor(string.byte(payload, i), string.byte(XOR_PREFIX, i))
	end
	local decoded = nil
	for size = 1, #known do
		local periodic = true
		for i = 1, #known do
			if known[i] ~= known[(i - 1) % size + 1] then
				periodic = false
				break
			end
		end
		if periodic then
			local out = new_builder()
			for i = 1, #payload do
				builder_add(out, string.char(bxor(known[(i - 1) % size + 1], string.byte(payload, i))))
			end
			local candidate = builder_build(out)
			local trimmed = string.gsub(candidate, "[%s]+$", "")
			if string.sub(candidate, 1, #XOR_PREFIX) == XOR_PREFIX
				and string.sub(trimmed, -2) == "})" then
				decoded = candidate
				break
			end
		end
	end
	if not decoded then
		anicli.fail("extract_failed", "player XOR wrapper format changed")
	end

	-- Locate the string table: an array whose 7-char entries XOR-mask
	-- onto "AES-CBC" and whose unmasked siblings cover the WebCrypto
	-- identifiers.
	local crypto_table = nil
	regexp_iterate(STRING_ARRAY_RE, decoded, function(_, m)
		local entries = {}
		regexp_iterate(JS_LITERAL_RE, m[2], function(_, lm)
			entries[#entries + 1] = unquote_js(lm[2])
		end)
		for _, entry in ipairs(entries) do
			if #entry == 7 then
				local mask = bxor(string.byte(entry, 1), string.byte("A"))
				local unmasked = {}
				for i = 1, #entry do
					unmasked[i] = string.char(bxor(string.byte(entry, i), mask))
				end
				if table.concat(unmasked) == "AES-CBC" then
					local candidate = {}
					for i, v in ipairs(entries) do
						local buf = {}
						for j = 1, #v do
							buf[j] = string.char(bxor(string.byte(v, j), mask))
						end
						candidate[i] = table.concat(buf)
					end
					if all_present(candidate, { "HMAC", "SHA-256", "importKey", "decrypt" }) then
						crypto_table = candidate
						return true
					end
				end
			end
		end
		return false
	end)
	if not crypto_table then
		anicli.fail("extract_failed", "player crypto string table could not be decoded")
	end

	-- Substitute ONLY the declared obfuscation object's name.fn(N)
	-- calls, exactly like the reference implementation pins them. The
	-- VM's pattern engine has no %f frontier, so the Go \b becomes an
	-- explicit preceding-character capture (a word-char prefix rejects
	-- the call, and a guard space covers a hypothetical byte-0 call);
	-- an out-of-range index keeps the whole call verbatim (the gsub nil
	-- return).
	local lookup = "([^%w_])" .. object_m[2] .. "%.[%w$]+%((%d+)%)"
	local substituted = string.gsub(" " .. script, lookup, function(pre, idx_s)
		local idx = tonumber(idx_s)
		-- The lookups index the table 0-based (the Go slice semantics
		-- the compiled provider pinned); Lua arrays are 1-based.
		if idx == nil or idx < 0 or idx + 1 > #crypto_table then
			return nil
		end
		return pre .. anicli.json.encode(crypto_table[idx + 1])
	end)
	return string.sub(substituted, 2)
end

-- player_parameters recovers the player's crypto parameters from the
-- bundle source: a plaintext bundle (AES-CBC already visible) is read
-- directly, an obfuscated one is statically unpacked first. Every
-- candidate's following bytes must mention AES-CBC and HMAC (the
-- nearby-check that disambiguates multiple const-declaration runs).
local function player_parameters(script)
	local src = script
	if not string.find(src, "AES-CBC", 1, true) then
		src = unpack_player_strings(src)
	end

	local params = nil
	regexp_iterate(PARAM_RE, src, function(full, m)
		local nearby = string.sub(src, m and 0 or 0)
		local at = string.find(src, full, 1, true)
		if not at then
			return false
		end
		nearby = string.sub(src, at + #full)
		if #nearby > 12000 then
			nearby = string.sub(nearby, 1, 12000)
		end
		if not string.find(nearby, "AES-CBC", 1, true)
			or not string.find(nearby, "HMAC", 1, true) then
			return false
		end
		local ttl = tonumber(m[5])
		if ttl == nil then
			return false
		end
		params = {
			key = unquote_js(m[2]),
			iv = unquote_js(m[3]),
			secret = unquote_js(m[4]),
			ttl = ttl,
		}
		return true
	end)
	if not params then
		anicli.fail("extract_failed", "player crypto parameters changed")
	end
	return params
end

-- ak_ajax performs one XMLHttpRequest against the site and unwraps the
-- {"status":N,"result":…} envelope. The site answers malformed calls
-- with HTTP 200 and the error INSIDE the envelope: a status >= 400
-- surfaces as the typed extract wall quoting both.
local function ak_ajax(url)
	local resp = anicli.http.get(url, { headers = {
		["X-Requested-With"] = "XMLHttpRequest",
		Referer = base_url .. "/",
	} })
	local ok, env = pcall(anicli.json.decode, resp.body)
	if not ok or type(env) ~= "table" then
		anicli.fail("extract_failed", "ajax " .. url .. " answered non-JSON")
	end
	if (env.status or 0) >= 400 then
		anicli.fail("extract_failed", "ajax " .. url .. " answered envelope status "
			.. tostring(env.status) .. ": " .. tostring(env.result))
	end
	return env.result
end

local function trim(s)
	return (string.gsub(string.gsub(s, "^%s+", ""), "%s+$", ""))
end

-- parse_server_groups folds the server-list HTML into {SUB = {ids…},
-- DUB = {ids…}} — one entry per li[data-link-id] under each data-type
-- group, document order preserved.
local function parse_server_groups(body)
	local embeds = {}
	anicli.html.parse(body):find(".servers .type[data-type]"):each(function(_, group)
		local dub = string.upper(trim(group:attr("data-type")))
		if dub ~= AUDIO_SUB and dub ~= AUDIO_DUB then
			return
		end
		local links = embeds[dub] or {}
		group:find("li[data-link-id]"):each(function(_, li)
			local link_id = trim(li:attr("data-link-id"))
			if link_id == "" then
				return
			end
			links[#links + 1] = link_id
		end)
		embeds[dub] = links
	end)
	return embeds
end

-- resolve_reference ports the URL resolution the embed chain needs:
-- absolute refs stay, scheme-relative gain the scheme, root-relative
-- gain the origin, bare paths join the embedding directory.
local function resolve_reference(base, ref)
	if string.match(ref, "^%a+://") then
		return ref
	end
	if string.sub(ref, 1, 2) == "//" then
		return (string.match(base, "^(%a+:)") or "https:") .. ref
	end
	local origin = string.match(base, "^(%a+://[^/?#]+)") or ""
	if string.sub(ref, 1, 1) == "/" then
		return origin .. ref
	end
	local dir = string.match(base, "^(.*/)") or origin .. "/"
	return dir .. ref
end

-- rfc3339_unix parses the anicli.time.now() stamp into Unix seconds
-- (the sandbox opens no os library, so the civil-to-epoch arithmetic
-- lives here).
local function rfc3339_unix(s)
	local y, mo, d, h, mi, sec = string.match(s,
		"^(%d+)-(%d+)-(%d+)T(%d+):(%d+):(%d+)")
	if not y then
		return nil
	end
	y, mo, d = tonumber(y), tonumber(mo), tonumber(d)
	if mo <= 2 then
		y = y - 1
	end
	local era = math.floor(y / 400)
	local yoe = y - era * 400
	local doy = math.floor((153 * (mo + (mo > 2 and -3 or 9)) + 2) / 5) + d - 1
	local doe = yoe * 365 + math.floor(yoe / 4) - math.floor(yoe / 100) + doy
	local days = era * 146097 + doe - 719468
	return days * 86400 + tonumber(h) * 3600 + tonumber(mi) * 60 + tonumber(sec)
end

local function pad_bytes(s, n)
	if #s >= n then
		return string.sub(s, 1, n)
	end
	return s .. string.rep("\0", n - #s)
end

-- decrypt_source decrypts the getSources enc blob: AES-256-CBC with
-- the UTF-8 key truncated/zero-padded to 32 bytes (the JS TextEncoder
-- semantics), zero-padded 16-byte IV, URL-safe base64 and PKCS7 (the
-- SDK primitive validates it). The decrypted JSON carries the manifest
-- URL either as {"file": …} or as a list of such objects.
local function decrypt_source(encoded, params)
	local pad = #encoded % 4
	if pad ~= 0 then
		encoded = encoded .. string.rep("=", 4 - pad)
	end
	local ct = anicli.base64.url_decode(encoded)
	local ok, pt = pcall(anicli.crypto.aes_cbc_decrypt,
		pad_bytes(params.key, 32), pad_bytes(params.iv, 16), ct)
	if not ok then
		anicli.fail("extract_failed", "decrypt sources: " .. tostring(pt))
	end

	local single = anicli.json.decode(pt)
	if type(single) == "table" and (single.file or "") ~= "" then
		return single.file
	end
	if type(single) == "table" then
		for _, item in ipairs(single) do
			if type(item) == "table" and (item.file or "") ~= "" then
				return item.file
			end
		end
	end
	anicli.fail("extract_failed", "decrypted sources carry no file")
end

-- signed_url reproduces the player's CDN URL signing: the two 32-hex
-- path segments form the message "{expires}|{h1}/{h2}" (lowercase,
-- expires = now + ttl), HMAC-SHA256'd with the player secret; the
-- token is b64url(message) + "." + b64url(mac). URLs already carrying
-- a token — and URLs without the hex segments — pass through
-- unchanged.
local function signed_url(raw_url, params)
	if string.find(raw_url, "token=", 1, true) then
		return raw_url
	end
	local m = anicli.regexp.match("(?i)/([a-f0-9]{32})/([a-f0-9]{32})/", raw_url)
	if not m then
		return raw_url
	end
	local now = rfc3339_unix(anicli.time.now())
	if not now then
		anicli.fail("extract_failed", "the sandbox clock is unreadable")
	end
	local message = string.format("%d|%s/%s", now + params.ttl,
		string.lower(m[2]), string.lower(m[3]))
	local sep = "?"
	if string.find(raw_url, "?", 1, true) then
		sep = "&"
	end
	local mac = anicli.crypto.hmac_sha256(params.secret, message)
	return raw_url .. sep .. "token=" .. anicli.base64.url_encode(message)
		.. "." .. anicli.base64.url_encode(mac)
end

-- master_links splits the fetched manifest: a master playlist yields
-- one link per RESOLUTION height (relative URIs resolved against the
-- manifest URL), anything else is the single 1080 entry.
local function master_links(body, manifest_url, origin_referer)
	if not string.find(body, "#EXT-X-STREAM-INF", 1, true) then
		return {
			[DEFAULT_HEIGHT] = {
				url = manifest_url, quality = DEFAULT_HEIGHT,
				type = "m3u8", headers = { Referer = origin_referer },
			},
		}
	end
	local links = {}
	local pending = ""
	for line in string.gmatch(body .. "\n", "(.-)\n") do
		line = string.gsub(line, "\r$", "")
		if string.sub(line, 1, 18) == "#EXT-X-STREAM-INF:" then
			pending = DEFAULT_HEIGHT
			local res = anicli.regexp.match("RESOLUTION=(\\d+)[xX](\\d+)", line)
			if res then
				pending = res[3]
			end
		elseif line == "" or string.sub(line, 1, 1) == "#" then
			-- attributes may continue on the same line only
		elseif pending ~= "" then
			links[pending] = {
				url = resolve_reference(manifest_url, line),
				quality = pending,
				type = "m3u8",
				headers = { Referer = origin_referer },
			}
			pending = ""
		end
	end
	return links
end

-- resolve_megaplay walks the megaplay embed chain: the embed page
-- carries the player div (data-id) and the e1 player bundle; the
-- bundle's static unpack yields the AES key/IV + HMAC secret;
-- getSources answers an enc blob decrypting to the CDN manifest URL,
-- which must carry the HMAC token. The manifest splits into
-- per-quality variant links (Referer = the megaplay origin — the CDN
-- checks it on playback).
local function resolve_megaplay(embed_url)
	local origin = string.match(embed_url, "^(%a+://[^/?#]+)")
	if not origin then
		anicli.fail("extract_failed", "embed url " .. embed_url .. " does not parse")
	end
	local origin_referer = origin .. "/"

	local page = anicli.http.get(embed_url, { headers = { Referer = base_url .. "/" } })
	local doc = anicli.html.parse(page.body)
	local player = doc:find("#megaplay-player[data-id]")
	local data_id = player:attr("data-id")
	if player:len() == 0 or data_id == "" then
		anicli.fail("extract_failed", "embed page carries no #megaplay-player data-id")
	end
	local script_src = ""
	doc:find("script[src]"):each(function(_, s)
		if script_src == "" then
			local src = s:attr("src")
			if string.find(src, "e1-player", 1, true) then
				script_src = src
			end
		end
	end)
	if script_src == "" then
		anicli.fail("extract_failed", "embed page carries no e1-player bundle")
	end

	local params = player_parameters(
		anicli.http.get(resolve_reference(embed_url, script_src),
			{ headers = { Referer = origin_referer } }).body)

	local sources = anicli.http.get(
		origin .. "/stream/getSourcesNew?id=" .. anicli.http.query_escape(data_id),
		{ headers = {
			["X-Requested-With"] = "XMLHttpRequest",
			Referer = origin_referer,
		} })
	local payload = anicli.json.decode(sources.body)
	if type(payload) ~= "table" or (payload.enc or "") == "" then
		anicli.fail("extract_failed", "getSources returned no enc blob")
	end
	local file = decrypt_source(payload.enc, params)
	local manifest = signed_url(file, params)
	local playlist = anicli.http.get(manifest, { headers = { Referer = origin_referer } })
	return master_links(playlist.body, manifest, origin_referer)
end

return {
	id = "anikoto",
	name = "AniKoto",
	base_url = base_url,
	capabilities = "both",
	content_lang = "en",
	-- The catalog indexes romaji/English titles only — a Cyrillic
	-- query is guaranteed-zero.
	name_preference = "latin",

	search = function(query)
		local resp = anicli.http.get(base_url .. "/filter?keyword=" ..
			anicli.http.query_escape(query))
		local results = {}
		anicli.html.parse(resp.body):find("#list-items .item"):each(function(_, card)
			local href = card:find("a[href]"):attr("href")
			local title = card:find(".info .b1 a.name.d-title"):text()
			if href == "" or title == "" then
				return
			end
			local poster = card:find(".ani.poster.tip img[src]"):attr("src")
			if string.sub(poster, 1, 2) == "//" then
				poster = "https:" .. poster
			end
			results[#results + 1] = {
				title = title,
				url = (string.gsub(href, "/ep%-[^/]+$", "")),
				poster = poster,
			}
		end)
		return results
	end,

	episodes = function(anime_url)
		local resp = anicli.http.get(anime_url)
		local data_id = anicli.regexp.match("data-id=\"(\\d+)\"", resp.body)
		if not data_id then
			anicli.fail("not_found", "watch page carries no anime data-id")
		end

		local result = ak_ajax(base_url .. "/ajax/episode/list/" .. data_id[2] .. "?vrf=")
		if type(result) ~= "string" then
			anicli.fail("extract_failed", "episode list result is not HTML")
		end

		local anchors = {}
		anicli.html.parse(result):find("li[data-html]"):each(function(_, li)
			local a = li:find("a[data-id]")
			local num = a:attr("data-num")
			local ep_id = a:attr("data-id")
			local ids = a:attr("data-ids")
			if num == "" or ep_id == "" or ids == "" then
				return
			end
			anchors[#anchors + 1] = {
				num = num,
				title = trim(li:attr("title")),
				ep_id = ep_id,
				ids = ids,
			}
		end)
		if #anchors == 0 then
			anicli.fail("not_found", "episode list carries no episode anchors")
		end

		-- Eager dub hydration: the Lua contract has no DubsHydrator, so
		-- every episode's SUB/DUB server groups ride the listing (the
		-- yummy/animedia precedent). The legs are sequential AJAX calls
		-- (the SDK batch bridge cannot carry the XHR headers the site's
		-- endpoints answer), and one dead leg leaves that episode's
		-- embeds empty — the resolve re-fetches and fails loudly for the
		-- episode a user actually opens.
		local episodes = {}
		for _, anchor in ipairs(anchors) do
			local embeds = {}
			local ok, result = pcall(ak_ajax, base_url .. "/ajax/server/list?servers="
				.. anicli.http.query_escape(anchor.ids))
			if ok and type(result) == "string" then
				embeds = parse_server_groups(result)
			end
			episodes[#episodes + 1] = {
				num = anchor.num,
				title = anchor.title,
				raw_id = anchor.ep_id .. ":" .. anchor.ids,
				raw_embeds = embeds,
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local at = string.find(raw_id, ":", 1, true)
		local ids = at and string.sub(raw_id, at + 1) or ""
		if ids == "" then
			anicli.fail("invalid_input",
				"episode RawID " .. raw_id .. " is not {ep-id}:{data-ids}")
		end

		local embeds = parse_server_groups(
			ak_ajax(base_url .. "/ajax/server/list?servers="
				.. anicli.http.query_escape(ids)))
		local link_ids = embeds[dub]
		if link_ids == nil or #link_ids == 0 then
			-- DUB MISS (the #159 doctrine, see the header): never
			-- substitute the other dub group silently — list what the
			-- episode ACTUALLY carries (DUB first, the fixed canonical
			-- order of the two-group table, no extra fetches).
			local carriers = {}
			for _, name in ipairs({ AUDIO_DUB, AUDIO_SUB }) do
				if type(embeds[name]) == "table" and #embeds[name] > 0 then
					carriers[#carriers + 1] = name
				end
			end
			if #carriers == 0 then
				-- the ONLY dub wall with no ask behind it: the server
				-- list carries no link rows at all — a data-shape
				-- fact, not a caller mistake.
				anicli.fail("not_found", "episode " .. raw_id .. " carries no dub in its server groups")
			end
			anicli.fail("not_found", "episode " .. raw_id .. ' carries no dub "' .. dub ..
				'" (episode dubs: ' .. table.concat(carriers, ", ") .. ')')
		end

		-- Each link-id walks the chain server resolver → megaplay page →
		-- player bundle → getSources → master playlist, stopping at the
		-- first server that yields variants. Every server failing is
		-- the typed extract wall with the first server's cause attached.
		local links, first_err = nil, nil
		for _, link_id in ipairs(link_ids) do
			local ok, res = pcall(function()
				local resolved = ak_ajax(base_url .. "/ajax/server?get="
					.. anicli.http.query_escape(link_id))
				if type(resolved) ~= "table" or (resolved.url or "") == "" then
					anicli.fail("extract_failed", "stream resolver returned no url")
				end
				return resolve_megaplay(resolved.url)
			end)
			if ok then
				links = res
				break
			end
			if first_err == nil then
				first_err = res
			end
		end

		if links == nil then
			anicli.fail("extract_failed", "episode " .. raw_id .. " dub \""
				.. dub .. "\": " .. #link_ids .. " server(s) dead: "
				.. tostring(first_err))
		end
		return { dub_name = dub, links = links }
	end,
}
