package providers

// AllAnime source-URL decoding helpers. The pre-rotation key
// derivation stack (referer-page scraping for epoch/partB, mask
// hunting in entry-bundle chunks) was superseded on 2026-09-13 by the
// v3 client-crypto port (allanime_proto.go + allanime_bootstrap.go);
// what survives here is the "--"-URL decoder and the tobeparsed blob
// extractor, both still on the wire.

import (
	"encoding/hex"
	"regexp"
	"strings"
)

// aaSource is one decoded episode source: provider name, the possibly
// still "--"-prefixed encoded URL, and the live player hints (type
// "player" with fallBack "mp4" marks direct-media entries;
// [LIVE-VERIFIED 2026-09-13]).
type aaSource struct {
	Name     string
	URL      string
	Type     string
	FallBack string
}

// Scrapers for decrypted tobeparsed payloads that are not clean JSON.
var (
	aaSourceURLNameRe = regexp.MustCompile(`"sourceUrl"\s*:\s*"--([^"]*)"[^}]*"sourceName"\s*:\s*"([^"]*)"`)
	aaSourceNameURLRe = regexp.MustCompile(`"sourceName"\s*:\s*"([^"]*)"[^}]*"sourceUrl"\s*:\s*"--([^"]*)"`)
	aaToBeParsedRe    = regexp.MustCompile(`"tobeparsed"\s*:\s*"([^"]*)"`)
)

// aaExtractToBeParsedBlob pulls the base64 tobeparsed value out of a
// raw API response body, "" when absent.
func aaExtractToBeParsedBlob(response []byte) string {
	if m := aaToBeParsedRe.FindSubmatch(response); m != nil {
		return string(m[1])
	}
	return ""
}

// aaDecodeHexPairs ports the Python "--" URL decoder (allanime.py
// _decrypt): every hex pair maps to chr(int(pair, 16) ^ 56). Python's
// oct()/int(_, 8) round-trip is a no-op. Divergence: Python built a
// Unicode string (chr of 0x80..0xFF becomes two UTF-8 bytes); real URL
// alphabets are ASCII, so the raw byte is kept. ok=false on odd length
// or non-hex input (Python raised and killed the whole resolve).
func aaDecodeHexPairs(raw string) (string, bool) {
	if len(raw) == 0 || len(raw)%2 != 0 {
		return "", false
	}
	out := make([]byte, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		pair, err := hex.DecodeString(raw[i : i+2])
		if err != nil || len(pair) != 1 {
			return "", false
		}
		out = append(out, pair[0]^56)
	}
	return string(out), true
}

// decodeAllAnimeSourceURL turns one sourceUrls entry into a fetchable
// URL (port of allanime.py:196-208, transport updated for the mkissa
// rotation): hex-pair decode, rewrite "clock" to "clock.json", then
// absolutize — http(s) URLs pass through, "/..." is prefixed with the
// internal base (allanime.day in production, injectable for tests).
//
// The "--" marker may already be stripped by the payload decoder, so
// the encoded body is detected structurally: a "--"-marked value must
// decode or the source is rejected; an unmarked value that is entirely
// hex pairs decodes too; anything else is a plain URL passed through
// verbatim (documented divergence: Python pushed every value through
// the hex decoder and crashed the whole resolve on non-hex input).
// A decoded relative path without a leading slash gains the missing
// "/" (Python concatenated a malformed URL that could only 404).
func decodeAllAnimeSourceURL(src, internalBase string) (string, bool) {
	body := strings.TrimPrefix(src, "--")
	plain := body
	if strings.HasPrefix(src, "--") || aaLooksHexPairs(body) {
		decoded, ok := aaDecodeHexPairs(body)
		if !ok {
			return "", false
		}
		plain = decoded
	}
	// Python str.replace("clock", "clock.json") on the whole decoded
	// URL — bug-compatible including a "clock" inside a query value.
	plain = strings.ReplaceAll(plain, "clock", "clock.json")

	switch {
	case strings.HasPrefix(plain, "http"):
		return plain, true
	case strings.HasPrefix(plain, "/"):
		return internalBase + plain, true
	case plain == "":
		return "", false
	default:
		return internalBase + "/" + plain, true
	}
}

// aaLooksHexPairs reports whether s is a non-empty even-length run of
// hex digits (a candidate encoded body).
func aaLooksHexPairs(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		hexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !hexDigit {
			return false
		}
	}
	return true
}
