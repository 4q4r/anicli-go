-- AnimeVost (api.animevost.org) — the RU JSON-API catalog, the PR119
-- Lua migration of the compiled Go provider (itself the port of
-- anicli-py anicli/providers/animevost.py). The v1 methods answer on
-- the api host — POST /v1/search and POST /v1/playlist — re-verified
-- live 2026-10-05: the Let's Encrypt cert that expired 2026-09-20 was
-- renewed (valid through 2026-12-20) and the 2026-09-18 uTLS
-- fingerprint tarpit is gone (the netclient transport answers in
-- ~0.3s through the configured proxy; the direct route does not
-- resolve DNS on the characterization network — the usual RU routing
-- fact, not a provider defect). The v13.vost.pw mirror serves the site
-- only, no API — there is no fallback target and none is needed.
--
-- Wire shapes (the 2026-09-18 captures, unchanged 2026-10-05):
--   - search: POST form name=<query> → {"state":{...},"data":[...]}.
--     state.count is stale metadata (stays 0 while data carries hits)
--     and is not consulted. A miss answers HTTP 404 — the transport
--     maps it to the typed not-found class before the script runs (a
--     Lua provider keeps the errors.Is(ErrNotFound) contract; the
--     server's miss text is not recoverable, the transport drops error
--     bodies).
--   - playlist: POST form id=<id> → a bare array of
--     {name, hd, std, preview} entries, or HTTP 200
--     {"status":"fail","error":"Тайтл с таким id не найден"} for an
--     unknown id — routed on the first non-space byte like the Go port
--     did, and surfaced as a typed miss WITH the server's text.
--   - qualities: hd maps to 720 and std to 480, both typed mp4
--     (animevost.py:66-76) — the resolution is offline.
--
-- State passing: the hd/std links payload rides in raw_id (the
-- fresh-sandbox streams(raw_id, dub) state carrier — the adapter does
-- not pass raw_embeds into scripts) and is duplicated into raw_embeds
-- under the "AnimeVost" dub so consumers see the same ref the compiled
-- Go provider kept. streams() therefore never touches the network.

local base_url = "https://api.animevost.org/v1"

-- decode_json parses one API body, naming the operation on failure:
-- a malformed body is a surfaced error, never a silent empty surface
-- (the Python original's except-return-[] faked a healthy provider).
local function decode_json(body, what)
	local ok, value = pcall(anicli.json.decode, body)
	if not ok then
		error("decode " .. what .. " response: " .. tostring(value), 0)
	end
	return value
end

-- str_field reads an optional string field as "" (the Go port's JSON
-- zero-value semantics; tostring(nil) would fabricate "nil").
local function str_field(value)
	if type(value) == "string" then
		return value
	end
	return ""
end

return {
	id = "animevost",
	name = "AnimeVost",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",
	-- The 2026-09-18 index reliably matches canonical latin names
	-- (black lagoon, naruto) while Cyrillic phrases only hit in their
	-- exact inflected site-title form — the fan-out must send the
	-- romaji/english variant (PR42 semantics).
	name_preference = "latin",

	search = function(query)
		local form = "name=" .. anicli.http.query_escape(query)
		local resp = anicli.http.post(base_url .. "/search", form, "application/x-www-form-urlencoded")

		local data = decode_json(resp.body, "search")
		if type(data) ~= "table" then
			error("decode search response: expected a JSON object, got " .. type(data), 0)
		end
		-- A 200 body can still carry the error envelope; surface it.
		if str_field(data.error) ~= "" then
			anicli.fail("not_found", data.error)
		end
		if type(data.data) ~= "table" then
			error("decode search response: missing data array", 0)
		end

		local results = {}
		for _, item in ipairs(data.data) do
			-- str(id) semantics: the numeric id stringified; a missing
			-- id surfaces "None" exactly like Python str(None) did.
			local url = "None"
			if item.id ~= nil then
				url = tostring(item.id)
			end
			results[#results + 1] = {
				title = str_field(item.title),
				url = url,
				poster = str_field(item.urlImagePreview),
			}
		end
		return results
	end,

	episodes = function(anime_url)
		local form = "id=" .. anicli.http.query_escape(anime_url)
		local resp = anicli.http.post(base_url .. "/playlist", form, "application/x-www-form-urlencoded")

		-- A JSON body is either the playlist array or a fail object;
		-- route on the first non-space byte instead of inferring from
		-- decode failures.
		local entries
		if string.match(resp.body, "^%s*%[") then
			entries = decode_json(resp.body, "playlist")
		else
			local fail = decode_json(resp.body, "playlist")
			local msg = str_field(type(fail) == "table" and fail.error or nil)
			if msg == "" then
				msg = "unexpected playlist response: " .. string.sub(resp.body, 1, 120)
			end
			anicli.fail("not_found", msg)
		end

		local episodes = {}
		for i, item in ipairs(entries) do
			-- 1-based enumerate supplies num; the name falls back to
			-- the index string (animevost.py:51-52).
			local name = str_field(item.name)
			if name == "" then
				name = tostring(i)
			end
			local links = {}
			if str_field(item.hd) ~= "" then
				links.hd = item.hd
			end
			if str_field(item.std) ~= "" then
				links.std = item.std
			end
			local payload = anicli.json.encode(links)
			episodes[#episodes + 1] = {
				num = tostring(i),
				title = name,
				raw_id = payload,
				raw_embeds = { AnimeVost = { payload } },
			}
		end
		return episodes
	end,

	streams = function(raw_id, dub)
		local links = anicli.json.decode(raw_id)
		local out = {}
		if type(links) == "table" then
			if str_field(links.hd) ~= "" then
				out["720"] = { url = links.hd, quality = "720", type = "mp4" }
			end
			if str_field(links.std) ~= "" then
				out["480"] = { url = links.std, quality = "480", type = "mp4" }
			end
		end
		return { dub_name = dub, links = out }
	end,
}
