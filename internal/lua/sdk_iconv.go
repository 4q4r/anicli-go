package lua

import (
	"strings"

	lua "github.com/yuin/gopher-lua"
	"golang.org/x/text/encoding/ianaindex"
)

// The PR136 iconv SDK: anicli.iconv(s, from_encoding) decodes a raw
// byte string (a Lua string is byte-safe) out of a legacy encoding
// into UTF-8 — the leg the cp1251 scrapers (anistar) need. Encoding
// names resolve through golang.org/x/text/encoding/ianaindex, which
// knows the IANA registry names case- and whitespace-insensitively
// ("windows-1251", "WINDOWS-1251"); the de-facto short forms scripts
// actually type ride a small alias map, because the index resolves
// registry names only.
//
// Invalid bytes never error: the x/text decoders substitute U+FFFD
// (replacement semantics), so a scraped page always decodes — garbage
// bytes become visible replacement runes instead of a provider wall.
// An unknown encoding name raises the typed anicli:invalid_input:
// marker — the same RaiseError channel as crypto, in the anicli.fail
// shape, so provider.go re-attaches the contracts sentinel and a
// script can pcall it.

// sdkEncodingAliases maps the short forms onto their IANA registry
// names. Keep this to aliases proven by tests, not speculation.
var sdkEncodingAliases = map[string]string{
	"cp1251": "windows-1251",
	"utf8":   "utf-8",
}

// openSDKIconv registers anicli.iconv onto the SDK module table.
func openSDKIconv(ls *lua.LState, mod *lua.LTable) {
	mod.RawSetString("iconv", ls.NewFunction(sdkIconv))
}

// sdkIconv implements anicli.iconv(s, from_encoding): the UTF-8
// decoding of the byte string s per the named encoding.
func sdkIconv(ls *lua.LState) int {
	s := ls.CheckString(1)
	orig := ls.CheckString(2)
	name := strings.ToLower(strings.TrimSpace(orig))
	if alias, ok := sdkEncodingAliases[name]; ok {
		name = alias
	}
	enc, err := ianaindex.IANA.Encoding(name)
	if err != nil {
		ls.RaiseError("anicli:invalid_input:iconv: unsupported encoding %q (want an IANA name, e.g. windows-1251/cp1251)", orig)
		return 0
	}
	decoded, err := enc.NewDecoder().Bytes([]byte(s))
	if err != nil {
		// Unreachable today: the IANA decoders replace instead of
		// erroring. Kept loud — silent failure paths are not allowed.
		ls.RaiseError("anicli:invalid_input:iconv: %v", err)
		return 0
	}
	ls.Push(lua.LString(decoded))
	return 1
}
