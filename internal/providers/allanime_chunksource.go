package providers

// AllAnime live crypto-chunk source (PR70): discovers and fetches the
// CURRENT player crypto chunk from the mkissa bundle graph — the same
// discovery the browser bridge runs in-page (entry/app.*.js → its
// /chunks/ imports → the chunk carrying the stable "invalid_part_b"
// error literal), but in plain Go so the chunk parser (allanime_material.go)
// can derive the build-id material without a JS engine.

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/an0nx/anicli-go/internal/netclient"
)

var (
	// aaEntryBundleRe: the app entry bundle URL on the root page (the
	// bridge's pattern; verified live 2026-09-13 and 2026-09-19).
	aaEntryBundleRe = regexp.MustCompile(`https?://[^"'\s]+/entry/app\.[A-Za-z0-9_.-]+\.js`)
	// aaChunkSpecRe: quote-delimited chunk specifiers inside the entry
	// bundle (the live imports are "../chunks/x.js", not "./x.js").
	aaChunkSpecRe = regexp.MustCompile(`"([^"']*\.js)"`)
	// aaMaxChunkFetches bounds the marker scan (the entry bundle listed
	// 38 chunk specifiers live on 2026-09-19).
	aaMaxChunkFetches = 60
)

// aaLiveChunkSource fetches the crypto chunk through the provider's
// HTTP client (the netclient fingerprint/proxy stack).
type aaLiveChunkSource struct {
	// HTTP performs the page/bundle/chunk GETs.
	HTTP *netclient.Client
	// Root is the mkissa.to page origin the entry bundle is discovered
	// from (the provider's Referer).
	Root string
}

// FetchChunk discovers the crypto chunk and returns its source text.
// Every step fails loudly (wrapped, typed at the manager): a missing
// entry bundle, an entry without chunks, or no crypto chunk among them
// is a real site-shape change and must surface, not silently degrade.
func (s *aaLiveChunkSource) FetchChunk(ctx context.Context) (string, error) {
	resp, err := s.HTTP.Get(ctx, s.Root, map[string]string{
		"Referer": s.Root,
		"Origin":  s.Root,
	})
	if err != nil {
		return "", fmt.Errorf("allanime chunk source: root page: %w", err)
	}
	entryURL := aaEntryBundleRe.FindString(string(resp.Body))
	if entryURL == "" {
		return "", fmt.Errorf("allanime chunk source: entry bundle URL not found on %s", s.Root)
	}
	entry, err := s.fetchText(ctx, entryURL)
	if err != nil {
		return "", fmt.Errorf("allanime chunk source: entry bundle: %w", err)
	}
	base, err := url.Parse(entryURL)
	if err != nil {
		return "", fmt.Errorf("allanime chunk source: entry URL %q: %w", entryURL, err)
	}
	seen := map[string]bool{}
	count := 0
	for _, m := range aaChunkSpecRe.FindAllStringSubmatch(entry, -1) {
		spec := m[1]
		u, err := url.Parse(spec)
		if err != nil {
			continue
		}
		resolved := base.ResolveReference(u)
		if !strings.Contains(resolved.Path, "/chunks/") {
			continue
		}
		href := resolved.String()
		if seen[href] {
			continue
		}
		seen[href] = true
		count++
		if count > aaMaxChunkFetches {
			break
		}
		body, err := s.fetchText(ctx, href)
		if err != nil {
			// A single dead chunk must not abort the scan.
			continue
		}
		if strings.Contains(body, aaCryptoChunkMarker) {
			return body, nil
		}
	}
	return "", fmt.Errorf("allanime chunk source: crypto chunk (%q marker) not found among %d chunk imports of %s", aaCryptoChunkMarker, count, entryURL)
}

// fetchText GETs url with the page headers and returns the body.
func (s *aaLiveChunkSource) fetchText(ctx context.Context, url string) (string, error) {
	resp, err := s.HTTP.Get(ctx, url, map[string]string{
		"Referer": s.Root,
		"Origin":  s.Root,
	})
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}
