package lua

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// The PR136 iconv SDK: anicli.iconv(s, from_encoding) decodes a raw
// byte string (Lua strings are byte-safe) out of a legacy encoding
// into UTF-8 — the leg the cp1251 scrapers (anistar) need. Names
// resolve through x/text's IANA index plus a two-entry alias map for
// the short forms scripts actually type. Invalid bytes never error:
// x/text substitutes U+FFFD, so a scraped page always decodes. An
// unknown name raises the typed anicli:invalid_input: marker (the
// anicli.fail channel — the adapter re-attaches the contracts
// sentinel; a script can pcall it).

// TestSDKIconvCP1251: the anistar shape — cp1251 bytes in, UTF-8
// out. The fixture is pinned against the x/text primitive the SDK
// wraps (the crypto-test pattern): the Go-side encoder builds the
// cp1251 bytes, the SDK must decode them back. The canonical IANA
// name and the short aliases must all decode identically.
func TestSDKIconvCP1251(t *testing.T) {
	t.Parallel()

	const fixture = "Аниме Онлайн"
	cp1251, err := charmap.Windows1251.NewEncoder().Bytes([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}

	e, _, _ := newSDKEngine(t, nil)
	for _, name := range []string{"windows-1251", "cp1251", "CP1251"} {
		got, err := evalSDK(t, e, fmt.Sprintf(
			`return anicli.iconv(%s, %q)`, luaBytesLiteral(string(cp1251)), name))
		if err != nil {
			t.Fatalf("iconv(%q): %v", name, err)
		}
		if got != fixture {
			t.Fatalf("iconv(%q) = %q, want %q", name, got, fixture)
		}
	}
}

// TestSDKIconvUnknownEncoding: an unsupported name must raise —
// pcall-catchable (never a raw panic out of the VM), carrying the
// anicli:invalid_input: fail-style marker and the primitive's name.
func TestSDKIconvUnknownEncoding(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	got, err := evalSDK(t, e, fmt.Sprintf(`
		local ok, res = pcall(anicli.iconv, %s, "koi-unknown")
		if ok then return "UNDETECTED: " .. tostring(res) end
		return "raised: " .. tostring(res)
	`, luaBytesLiteral("\xc0\xed")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "raised") || strings.Contains(got, "UNDETECTED") {
		t.Fatalf("verdict = %q, want a pcall-catchable raise", got)
	}
	if !strings.Contains(got, "anicli:invalid_input:") {
		t.Fatalf("raise = %q, want the anicli:invalid_input: marker", got)
	}
	if !strings.Contains(got, "iconv") {
		t.Fatalf("raise = %q, want the primitive named", got)
	}
}

// TestSDKIconvUTF8Identity: utf-8 input under the utf-8 name (and the
// utf8 short form) is a no-op — scripts can hand the function any
// encoding name they read from a page header without a special case.
func TestSDKIconvUTF8Identity(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	for _, name := range []string{"utf-8", "utf8"} {
		got, err := evalSDK(t, e, fmt.Sprintf(
			`return anicli.iconv(%q, %q)`, "Аниме Онлайн", name))
		if err != nil {
			t.Fatalf("iconv(%q): %v", name, err)
		}
		if got != "Аниме Онлайн" {
			t.Fatalf("iconv(%q) = %q, want the identity", name, got)
		}
	}
}

// TestSDKIconvReplacesInvalidBytes: 0x98 is undefined in Windows-1251
// — x/text substitutes U+FFFD (EF BF BD) instead of erroring, the
// documented replacement semantics.
func TestSDKIconvReplacesInvalidBytes(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	got, err := evalSDK(t, e, fmt.Sprintf(
		`return anicli.iconv(%s, "cp1251")`, luaBytesLiteral("\x98")))
	if err != nil {
		t.Fatal(err)
	}
	if got != "\ufffd" {
		t.Fatalf("iconv(0x98) = % x, want the U+FFFD replacement", []byte(got))
	}
}

// TestSDKIconvEngineSmoke: the consumer composition through the real
// sandbox — a cp1251 page fragment decoded by iconv, then mined with
// the other SDK primitives (regexp/html) the way the anistar script
// does. End-to-end proof the decoded UTF-8 string is a plain Lua
// string every other SDK surface accepts.
func TestSDKIconvEngineSmoke(t *testing.T) {
	t.Parallel()

	page, err := charmap.Windows1251.NewEncoder().Bytes(
		[]byte(`<h1 class="t">Наруто — Ураганные хроники</h1>`))
	if err != nil {
		t.Fatal(err)
	}

	e, _, _ := newSDKEngine(t, nil)
	got, err := evalSDK(t, e, fmt.Sprintf(`
		local raw  = %s
		local utf8 = anicli.iconv(raw, "cp1251")
		local m = anicli.regexp.match('<h1 class="t">(.*?)</h1>', utf8)
		local d = anicli.html.parse("<div>" .. utf8 .. "</div>")
		return m[2] .. "|" .. d:find("h1"):text()
	`, luaBytesLiteral(string(page))))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Наруто — Ураганные хроники|Наруто — Ураганные хроники" {
		t.Fatalf("engine smoke = %q", got)
	}
}
