package buffered

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// hlsMedia is the parsed download plan of one media playlist: the
// optional EXT-X-MAP init data and the resolved absolute segment URLs.
type hlsMedia struct {
	init     string
	segments []string
}

// fetchPlaylist GETs one playlist through netclient (browser-parity
// transport, proxy, CF solver) and returns the body with the FINAL url
// — the base every relative reference resolves against.
func (d *Downloader) fetchPlaylist(ctx context.Context, rawURL string, headers map[string]string) (string, string, error) {
	if d.net == nil {
		return "", "", fmt.Errorf("buffered: playlist fetch requires the net client")
	}
	resp, err := d.net.Get(ctx, rawURL, headers)
	if err != nil {
		return "", "", fmt.Errorf("buffered: fetch playlist: %w", err)
	}
	return string(resp.Body), resp.FinalURL, nil
}

// bufferHLS implements the minimal VOS-style downloader: master
// playlists resolve to the best (highest-BANDWIDTH) variant, media
// playlists yield the EXT-X-MAP init (prepended) and the sequential
// segment list; segments append to one temp file. Encrypted and live
// playlists fail typed — the streaming mode still plays them.
func (d *Downloader) bufferHLS(ctx context.Context, src Source, dir string, tr *tracker) (string, error) {
	playlistURL := src.URL
	var plan hlsMedia
	// One master hop maximum: a master pointing at another master is
	// outside the minimal downloader's contract.
	for level := 0; level < 2; level++ {
		body, finalURL, err := d.fetchPlaylist(ctx, playlistURL, src.Headers)
		if err != nil {
			return "", err
		}
		base, err := url.Parse(finalURL)
		if err != nil {
			return "", fmt.Errorf("buffered: playlist url %q: %w", finalURL, err)
		}
		variant, isMaster, err := pickHLSVariant(body, base)
		if err != nil {
			return "", err
		}
		if !isMaster {
			plan, err = parseHLSMedia(body, base)
			if err != nil {
				return "", err
			}
			break
		}
		if level == 1 {
			return "", fmt.Errorf("buffered: %w (master of masters)", ErrUnsupportedHLS)
		}
		playlistURL = variant
	}

	name := hlsName(src.Name, playlistURL)
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("buffered: create temp file: %w", err)
	}

	total := len(plan.segments)
	if plan.init != "" {
		if err := d.appendSegment(ctx, src, plan.init, f); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("buffered: init segment: %w", err)
		}
	}
	for i, seg := range plan.segments {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return "", err
		}
		if err := d.appendSegment(ctx, src, seg, f); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("buffered: сегмент %d/%d: %w", i+1, total, err)
		}
		tr.step(Progress{SegmentsDone: i + 1, SegmentsTotal: total})
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("buffered: close temp file: %w", err)
	}
	tr.force(Progress{SegmentsDone: total, SegmentsTotal: total})
	return path, nil
}

// appendSegment streams one segment (or the init) onto the file.
func (d *Downloader) appendSegment(ctx context.Context, src Source, segURL string, f *os.File) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, segURL, nil)
	if err != nil {
		return fmt.Errorf("buffered: build segment request: %w", err)
	}
	for k, v := range src.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Code: resp.StatusCode, URL: segURL}
	}
	_, err = io.Copy(f, resp.Body)
	return err
}

// hlsName derives the temp file name from the MEDIA playlist URL;
// media players sniff the concatenated container regardless of the
// extension.
func hlsName(name, playlistURL string) string {
	if name != "" {
		return sanitize(name, ".ts")
	}
	u, err := url.Parse(playlistURL)
	if err == nil {
		base := filepath.Base(u.Path)
		base = strings.TrimSuffix(base, ".m3u8")
		if base != "" && base != "/" && base != "." {
			return sanitize(base, ".ts") + ".ts"
		}
	}
	return "video.ts"
}

// pickHLSVariant resolves a master playlist to the best variant URI
// (absolute). isMaster reports whether the payload WAS a master.
func pickHLSVariant(body string, base *url.URL) (string, bool, error) {
	if !strings.Contains(body, "#EXT-X-STREAM-INF") {
		return "", false, nil
	}
	bestBandwidth := int64(-1)
	bestURI := ""
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		attrs := parseHLSAttrs(line)
		bw, _ := strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
		// The variant URI is the next non-comment, non-empty line.
		for j := i + 1; j < len(lines); j++ {
			cand := strings.TrimSpace(lines[j])
			if cand == "" || strings.HasPrefix(cand, "#") {
				continue
			}
			if bw > bestBandwidth {
				bestBandwidth = bw
				bestURI = resolveRef(base, cand)
			}
			break
		}
	}
	if bestURI == "" {
		return "", true, fmt.Errorf("buffered: %w: вариант не найден", ErrNotPlaylist)
	}
	return bestURI, true, nil
}

// parseHLSMedia turns one media playlist into the download plan.
func parseHLSMedia(body string, base *url.URL) (hlsMedia, error) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "#EXTM3U") {
		return hlsMedia{}, fmt.Errorf("buffered: %w", ErrNotPlaylist)
	}
	plan := hlsMedia{}
	endlist := false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-KEY"):
			attrs := parseHLSAttrs(line)
			method := strings.ToUpper(attrs["METHOD"])
			if method != "" && method != "NONE" {
				return hlsMedia{}, fmt.Errorf("buffered: %w", ErrEncryptedHLS)
			}
		case strings.HasPrefix(line, "#EXT-X-MAP"):
			attrs := parseHLSAttrs(line)
			if _, hasRange := attrs["BYTERANGE"]; hasRange {
				return hlsMedia{}, fmt.Errorf("buffered: %w (EXT-X-MAP:BYTERANGE)", ErrUnsupportedHLS)
			}
			if uri := attrs["URI"]; uri != "" {
				plan.init = resolveRef(base, uri)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE"):
			return hlsMedia{}, fmt.Errorf("buffered: %w (EXT-X-BYTERANGE)", ErrUnsupportedHLS)
		case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
			endlist = true
		case strings.HasPrefix(line, "#"):
			continue
		default:
			plan.segments = append(plan.segments, resolveRef(base, line))
		}
	}
	if !endlist {
		return hlsMedia{}, fmt.Errorf("buffered: %w", ErrLiveHLS)
	}
	if len(plan.segments) == 0 {
		return hlsMedia{}, fmt.Errorf("buffered: %w: сегменты не найдены", ErrNotPlaylist)
	}
	return plan, nil
}

// parseHLSAttrs parses the attribute list of one tag
// ("#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\""), honouring quoted
// values that may contain commas.
func parseHLSAttrs(line string) map[string]string {
	out := map[string]string{}
	colon := strings.Index(line, ":")
	if colon < 0 {
		return out
	}
	rest := line[colon+1:]
	var key, value strings.Builder
	inKey, inQuote := true, false
	flush := func() {
		if key.Len() > 0 {
			out[strings.ToUpper(strings.TrimSpace(key.String()))] = strings.TrimSpace(value.String())
		}
		key.Reset()
		value.Reset()
		inKey = true
	}
	for _, r := range rest {
		switch {
		case inQuote:
			if r == '"' {
				inQuote = false
			} else {
				value.WriteRune(r)
			}
		case r == '"':
			inQuote = true
		case r == '=' && inKey:
			inKey = false
		case r == ',':
			flush()
		default:
			if inKey {
				key.WriteRune(r)
			} else {
				value.WriteRune(r)
			}
		}
	}
	flush()
	return out
}

// resolveRef resolves a playlist reference against the playlist URL.
func resolveRef(base *url.URL, ref string) string {
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(r).String()
}
