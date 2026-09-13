package cfbrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHub release constants. The free CloakBrowser line publishes
// prebuilt stealth-Chromium archives as plain release assets; Pro-line
// releases carry no downloadable assets (their binaries sit behind the
// pro-channel download API — documented TODO upstream) and are skipped
// by LatestFreeRelease.
const (
	githubAPIBase     = "https://api.github.com"
	githubRepo        = "CloakHQ/cloakbrowser"
	ManualReleasesURL = "https://github.com/CloakHQ/cloakbrowser/releases"
	releasesPerPage   = 100
	sumsAssetName     = "SHA256SUMS"
)

// ghAsset mirrors the GitHub release-asset fields the installer needs.
type ghAsset struct {
	// Name is the asset file name (cloakbrowser-linux-x64.tar.gz…).
	Name string `json:"name"`
	// Size is the asset byte size (progress denominator).
	Size int64 `json:"size"`
	// Digest is the API-provided sha256:<hex> integrity digest.
	Digest string `json:"digest"`
	// URL is the browser_download_url the bytes stream from.
	URL string `json:"browser_download_url"`
}

// ghRelease mirrors one entry of the releases listing.
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// FreeRelease is the newest release carrying a usable free asset for
// the requested platform, with its version already parsed.
type FreeRelease struct {
	// TagName is the release tag (chromium-v146.0.7680.177.5).
	TagName string
	// Version is the dotted browser version.
	Version string
	// Asset is the platform archive.
	Asset ghAsset
}

// AssetUnavailableError reports that no release in the listing carries
// an asset for the platform (the live case for darwin on some free
// releases: darwin assets appear only on selected tags).
type AssetUnavailableError struct {
	// Platform is the spec that found no asset.
	Platform PlatformSpec
	// LatestTag is the newest release seen (for the error message).
	LatestTag string
}

// Error implements error with the manual-download hint.
func (e *AssetUnavailableError) Error() string {
	return fmt.Sprintf("cfbrowser: no %s asset on any CloakBrowser release (latest checked: %s); "+
		"download manually from %s into the cache directory or set $CLOAKBROWSER_BINARY_PATH",
		e.Platform.Asset, e.LatestTag, ManualReleasesURL)
}

// GitHubClient reads the CloakHQ release API. apiBase is overridable
// for tests; the zero-value http.Client is replaced with a sane
// default. No authentication: the repository is public and install
// traffic is a handful of requests per release (GitHub's 60 req/h
// anonymous budget holds; the auto-updater probes before querying).
type GitHubClient struct {
	apiBase string
	hc      *http.Client
}

// NewGitHubClient builds a client against apiBase (empty = the public
// GitHub API); hc may be nil for the default transport.
func NewGitHubClient(apiBase string, hc *http.Client) *GitHubClient {
	if apiBase == "" {
		apiBase = githubAPIBase
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &GitHubClient{apiBase: apiBase, hc: hc}
}

// LatestFreeRelease walks the release listing (newest first) and
// returns the first release carrying spec.Asset. Releases without the
// asset — the Pro line and darwin-less tags — are skipped. When the
// API omits the asset digest, the release's SHA256SUMS asset is
// consulted as a fallback.
func (g *GitHubClient) LatestFreeRelease(ctx context.Context, spec PlatformSpec) (*FreeRelease, error) {
	var releases []ghRelease
	err := g.getJSON(ctx, "/repos/"+githubRepo+"/releases?per_page="+fmt.Sprint(releasesPerPage), &releases)
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: list CloakBrowser releases: %w (manual: %s)", err, ManualReleasesURL)
	}

	latest := ""
	for i := range releases {
		rel := &releases[i]
		if latest == "" {
			latest = rel.TagName
		}
		for _, a := range rel.Assets {
			if a.Name != spec.Asset {
				continue
			}
			if a.Digest == "" {
				a.Digest = g.digestFromSums(ctx, rel, a.Name)
			}
			version, err := ParseVersionFromTag(rel.TagName)
			if err != nil {
				// Malformed tag: skip rather than poison the ladder.
				continue
			}
			return &FreeRelease{TagName: rel.TagName, Version: version, Asset: a}, nil
		}
	}
	return nil, &AssetUnavailableError{Platform: spec, LatestTag: latest}
}

// digestFromSums fetches and parses the release SHA256SUMS asset for
// name; empty string when unavailable (verification then fails loud).
func (g *GitHubClient) digestFromSums(ctx context.Context, rel *ghRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name != sumsAssetName {
			continue
		}
		var body []byte
		if _, err := g.DownloadAsset(ctx, a, nil, &byteWriter{&body}); err != nil {
			return ""
		}
		for _, line := range strings.Split(string(body), "\n") {
			// "<hex>  <name>" (two spaces); tolerate tabs.
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == name && len(fields[0]) == 64 {
				return "sha256:" + fields[0]
			}
		}
	}
	return ""
}

// DownloadAsset streams asset bytes into w while hashing (SHA-256) and
// reporting progress as integer percent (5% granularity is the
// caller's concern; this reports every chunk crossing a percent).
// progress may be nil. The returned digest is "sha256:<hex>".
func (g *GitHubClient) DownloadAsset(ctx context.Context, asset ghAsset, progress func(pct int), w io.Writer) (string, error) {
	if asset.URL == "" {
		return "", fmt.Errorf("cfbrowser: asset %q has no download URL", asset.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: build download request %s: %w", asset.Name, err)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: download %s: %w (manual: %s)", asset.Name, err, ManualReleasesURL)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cfbrowser: download %s: status %s (manual: %s)", asset.Name, resp.Status, ManualReleasesURL)
	}

	total := asset.Size
	if total <= 0 && resp.ContentLength > 0 {
		total = resp.ContentLength
	}
	hasher := sha256.New()
	var written int64
	nextPct := 1
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := w.Write(chunk); err != nil {
				return "", fmt.Errorf("cfbrowser: store %s: %w", asset.Name, err)
			}
			_, _ = hasher.Write(chunk)
			written += int64(n)
			if progress != nil && total > 0 {
				pct := int(written * 100 / total)
				for pct >= nextPct && nextPct <= 100 {
					progress(nextPct)
					nextPct++
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("cfbrowser: read %s: %w", asset.Name, rerr)
		}
	}
	if progress != nil {
		progress(100)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// getJSON performs a GET and decodes the JSON body into out.
func (g *GitHubClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiBase+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// byteWriter adapts a *[]byte to io.Writer for internal fetches.
type byteWriter struct{ b *[]byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.b = append(*w.b, p...)
	return len(p), nil
}
