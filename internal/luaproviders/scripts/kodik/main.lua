-- Kodik — the tokenled kodik-api.com search API plus the scraped
-- kodik.info player pages, the PR140 Lua migration of the compiled Go
-- provider (itself a port of anicli-py anicli/providers/kodik.py).
--
-- THE TOKEN: the API answers 401 without one (domain intel verified
-- 2026-09-12: the Python kodakapi.com base and its failover list are
-- dead; the live API lives at kodik-api.com). The token rides the
-- PR140 config-read SDK — anicli.provider_setting("token") — which the
-- factory flattens from providers.kodik.token (or ANICLI_KODIK_TOKEN).
-- An empty or missing token fails LOUD on search, before any request,
-- naming both settings paths — the Go port's error policy verbatim.
-- Only the search leg needs the token: the player pages and the
-- extractor resolve are anonymous (the Go port's shape too).
--
-- LIVE VERIFICATION IS IMPOSSIBLE: no owner token exists, so the live
-- legs of this provider are UNPROVEN by design. The fixture suite
-- (internal/providers/kodik_test.go over the kodik_*.json/.html
-- captures) carries the whole proof, and the parity smoke keeps kodik
-- in its credential-gated SKIP roster. Do not claim a live pass.
--
-- Observed shapes (the Go port's pins, kept verbatim):
--   - search: POST /search with the token/title/limit=20/
--     with_material_data/types=anime,anime-serial form. HTTP 401 maps
--     onto the loud config-flavored error (the Python failover loop
--     swallowed it into []); every OTHER transport failure and a bad
--     decode are silent-empty — the Python per-host except-block.
--   - episodes: the kodik.info player page the search returned.
--     Serial pages (the /serial/ URL or a .serial-series-box) build
--     one episode per series-box option with an embed per translation
--     (.serial-translations-box select option[data-media-id]
--     [data-media-hash]; season hardcoded 1, kodik.py:143-147). Movie
--     pages yield the single «Фильм» entry (.movie-translations-box,
--     kodik.py:153-168). Without a translations box the single
--     translation is scraped off the inline .media_id/.media_hash
--     script globals under the "Default" name (kodik.py:219-247,
--     first matching script only). A promo-error page and every
--     transport failure are silent-empty (the Python try-block).
--   - streams: the chosen dub's embed URL runs through the shared
--     extractor factory (anicli.extract), which folds the /ftor API
--     sources into the stream — the same loop the compiled provider
--     used via resolveEmbeds.
--
-- Contract shift forced by the fresh-sandbox Lua adapter (the
-- sameband precedent): the per-dub embed state rides episode raw_id
-- as the JSON {dub: url} map — the only channel into the
-- per-invocation streams(raw_id, dub) call; raw_embeds keeps carrying
-- the same embeds for consumers. An unknown or missing dub yields the
-- empty stream the Go provider produced (a nil embed slice resolved
-- into an empty link set, no error).

local base_url = "https://kodik-api.com"

-- The player embed origin: a CONSTRUCTED constant (kodik.py:144, 161),
-- never a fetched host — the shared extractor consumes the embed URLs.
local player_base = "https://kodik.info"

-- The single-translation fallback scrapes the script globals
-- (kodik.py:234-240). Go RE2 syntax through anicli.regexp — the same
-- patterns the compiled provider used. (Level-2 long brackets: the
-- pattern's own `]` characters would close a level-1 [[ ]] string.)
local MEDIA_ID_RE = [==[\.media_id\s*=\s*["']([^"']+)["']]==]
local MEDIA_HASH_RE = [==[\.media_hash\s*=\s*["']([^"']+)["']]==]

-- The token string or nil: provider_setting yields nil for a missing
-- key and for a provider without a settings section; the configured-
-- but-blank section yields "". Both mean "unconfigured" here.
local token = anicli.provider_setting("token")

-- asString parity: JSON scalars that are not strings render as ""
-- (the Go port's item.get() + string-typed consumer shape).
local function str_or_empty(v)
	if type(v) == "string" then
		return v
	end
	return ""
end

local function require_token()
	if token == nil or token == "" then
		anicli.fail("invalid_input",
			"kodik token is empty: set providers.kodik.token in settings.toml or ANICLI_KODIK_TOKEN")
	end
	return token
end

-- parse_translations scrapes the translation select box (the FIRST
-- matched select — the Go .First() pin); without one it falls back to
-- the single-translation script globals under "Default".
local function parse_translations(doc, is_serial)
	local selector = ".movie-translations-box select"
	if is_serial then
		selector = ".serial-translations-box select"
	end

	local translations = {}
	local boxes = doc:find(selector)
	if boxes:len() > 0 then
		boxes:each(function(i, box)
			if i > 1 then
				return -- .First(): only the first matched select
			end
			box:find("option"):each(function(_, opt)
				local id = opt:attr("data-media-id")
				local hash = opt:attr("data-media-hash")
				if id == "" or hash == "" then
					return
				end
				translations[opt:text()] = { id = id, hash = hash }
			end)
		end)
		return translations
	end

	-- First script carrying BOTH globals wins (the Go EachWithBreak).
	local done = false
	doc:find("script"):each(function(_, sc)
		if done then
			return
		end
		local txt = sc:text()
		if not string.find(txt, ".media_id", 1, true)
			or not string.find(txt, ".media_hash", 1, true) then
			return
		end
		done = true
		local idm = anicli.regexp.match(MEDIA_ID_RE, txt)
		local hashm = anicli.regexp.match(MEDIA_HASH_RE, txt)
		if idm and hashm then
			translations["Default"] = { id = idm[2], hash = hashm[2] }
		end
	end)
	return translations
end

-- state_of renders the streams() state channel: {dub: url} JSON.
local function state_of(embeds)
	local st = {}
	for name, urls in pairs(embeds) do
		st[name] = urls[1]
	end
	return anicli.json.encode(st)
end

return {
	id = "kodik",
	name = "Kodik",
	base_url = base_url,
	capabilities = "both",
	content_lang = "ru",

	search = function(query)
		local tok = require_token()

		-- The tokenled search form (kodik.py:42-95).
		local form = "token=" .. anicli.http.query_escape(tok) ..
			"&title=" .. anicli.http.query_escape(query) ..
			"&limit=20&with_material_data=true&types=anime%2Canime-serial"

		-- The Python per-host except-block: every transport failure is
		-- silent-empty EXCEPT the rejected-token 401, which is the loud
		-- config error (the Go port's mapping of perr.StatusCode == 401).
		local ok, resp = pcall(anicli.http.post, base_url .. "/search", form, "application/x-www-form-urlencoded")
		if not ok then
			if string.find(tostring(resp), "unexpected http status 401", 1, true) then
				anicli.fail("invalid_input",
					"kodik api rejected the token: check providers.kodik.token or ANICLI_KODIK_TOKEN")
			end
			return {}
		end
		if resp.status == 401 then
			anicli.fail("invalid_input",
				"kodik api rejected the token: check providers.kodik.token or ANICLI_KODIK_TOKEN")
		end

		-- safe_json_loads parity: a bad decode is silent-empty.
		local okj, data = pcall(anicli.json.decode, resp.body)
		if not okj or type(data) ~= "table" then
			return {}
		end

		local results = {}
		for _, item in ipairs(data.results or {}) do
			if type(item) == "table" then
				local display = str_or_empty(item.title)
				if display == "" then
					display = str_or_empty(item.title_orig)
				end
				if display == "" then
					display = "Unknown"
				end

				local link = str_or_empty(item.link)
				if link ~= "" then
					-- _common.normalize_protocol_url (kodik.py:73).
					if string.sub(link, 1, 2) == "//" then
						link = "https:" .. link
					end

					local poster = ""
					local md = item.material_data
					if type(md) == "table" then
						poster = str_or_empty(md.poster_url)
						if poster == "" then
							poster = str_or_empty(md.anime_poster_url)
						end
					end

					-- meta: present-but-null is a Pythonism with no Lua
					-- slot — a null year drops the key (the documented
					-- contract shift; consumers key-check either way).
					local meta = { type = str_or_empty(item.type) }
					if item.year ~= nil then
						meta.year = item.year
					end

					results[#results + 1] = {
						title = display,
						url = link,
						poster = poster,
						meta = meta,
					}
				end
			end
		end
		return results
	end,

	episodes = function(anime_url)
		-- The Python try-block: transport and parse failures are
		-- silent-empty (kodik.py:100-104).
		local ok, resp = pcall(anicli.http.get, anime_url)
		if not ok then
			return {}
		end
		local okd, doc = pcall(anicli.html.parse, resp.body)
		if not okd then
			return {}
		end

		if doc:find(".promo-error"):len() > 0 then
			return {}
		end

		local is_serial = string.find(anime_url, "/serial/", 1, true) ~= nil
			or doc:find(".serial-series-box"):len() > 0

		local translations = parse_translations(doc, is_serial)

		if is_serial then
			local boxes = doc:find(".serial-series-box select")
			if boxes:len() == 0 then
				return {}
			end
			local episodes = {}
			boxes:each(function(i, box)
				if i > 1 then
					return -- .First()
				end
				box:find("option"):each(function(_, opt)
					local num = opt:text()
					local embeds = {}
					for name, tr in pairs(translations) do
						-- kodik.py:143-147 (season hardcoded 1).
						embeds[name] = { player_base .. "/serial/" .. tr.id .. "/" .. tr.hash ..
							"/720p?min_age=16&first_url=false&season=1&episode=" .. num }
					end
					episodes[#episodes + 1] = {
						num = num,
						raw_id = state_of(embeds),
						raw_embeds = embeds,
					}
				end)
			end)
			return episodes
		end

		-- Movie / single episode (kodik.py:153-168).
		local embeds = {}
		for name, tr in pairs(translations) do
			embeds[name] = { player_base .. "/video/" .. tr.id .. "/" .. tr.hash ..
				"/720p?min_age=16&first_url=false" }
		end
		return {
			{
				num = "1",
				title = "Фильм",
				raw_id = state_of(embeds),
				raw_embeds = embeds,
			},
		}
	end,

	streams = function(raw_id, dub)
		-- The chosen dub's player link through the shared extractor
		-- factory (kodik.py:172-183). An unknown dub or unreadable
		-- state keeps the Go parity: the empty stream, no error.
		local stream = { dub_name = dub, links = {} }
		local ok, embeds = pcall(anicli.json.decode, raw_id)
		if not ok or type(embeds) ~= "table" then
			return stream
		end
		local url = embeds[dub]
		if type(url) ~= "string" or url == "" then
			return stream
		end
		stream.links = anicli.extract(url)
		return stream
	end,
}
