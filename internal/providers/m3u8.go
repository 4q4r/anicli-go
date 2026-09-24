package providers

import (
	"net/url"
	"regexp"
	"strings"
)

// m3u8Variant is one resolved master-playlist entry.
type m3u8Variant struct {
	uri    string
	height string
}

// m3u8ResolutionRe extracts the height from a RESOLUTION=WxH attribute.
var m3u8ResolutionRe = regexp.MustCompile(`RESOLUTION=(\d+)[xX](\d+)`)

// parseMasterPlaylist extracts variant entries from an m3u8 body the
// way the Python m3u8 lib did (the frozen reference's watch-page
// resolution path): #EXT-X-STREAM-INF lines carry RESOLUTION=WxH, the
// following non-comment line is the (possibly relative) URI resolved
// against the playlist URL; a missing RESOLUTION guesses 1080 (the
// Python height fallback).
func parseMasterPlaylist(body, playlistURL string) (variants []m3u8Variant, isVariant bool) {
	if !strings.Contains(body, "#EXT-X-STREAM-INF") {
		return nil, false
	}
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, true
	}

	lines := strings.Split(body, "\n")
	pendingHeight := ""
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pendingHeight = "1080"
			if m := m3u8ResolutionRe.FindStringSubmatch(line); m != nil {
				pendingHeight = m[2]
			}
		case line == "" || strings.HasPrefix(line, "#"):
			// attributes may continue on the same line only; skip
		default:
			if pendingHeight == "" {
				continue
			}
			ref, err := url.Parse(line)
			if err != nil {
				continue
			}
			variants = append(variants, m3u8Variant{
				uri:    base.ResolveReference(ref).String(),
				height: pendingHeight,
			})
			pendingHeight = ""
		}
	}
	return variants, true
}
