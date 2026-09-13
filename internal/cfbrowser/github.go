package cfbrowser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

	// EnvDownloadURL overrides the asset download base URL
	// ($CLOAKBROWSER_DOWNLOAD_URL). When set, every asset download is
	// rewritten onto that base (path and query kept) — the documented
	// escape hatch for mirrors and tests; the override host is allowed
	// by construction.
	EnvDownloadURL = "CLOAKBROWSER_DOWNLOAD_URL"
)

// allowedDownloadHosts is the closed set of hosts asset downloads may
// stream from: github.com plus the CDN/API hosts its release assets
// are served through. Matched by exact host or subdomain suffix.
var allowedDownloadHosts = []string{
	"github.com",
	"objects.githubusercontent.com",
	"api.github.com",
	"release-assets.githubusercontent.com",
}

// DownloadHostError reports an asset download URL whose host is not on
// the GitHub allowlist (hostile or unexpected API data): nothing is
// fetched from it.
type DownloadHostError struct {
	// Asset names the asset whose URL was rejected.
	Asset string
	// Host is the rejected host.
	Host string
}

// Error implements error with the override hint.
func (e *DownloadHostError) Error() string {
	return fmt.Sprintf("cfbrowser: asset %q download host %q is not allowed "+
		"(expected github.com or its asset CDNs; set $%s to override)", e.Asset, e.Host, EnvDownloadURL)
}

// hostAllowedBy suffix-matches host against the allowlist (exact host
// or a dot-separated subdomain of an entry; lookalike suffixes like
// "github.com.evil.example" never match).
func hostAllowedBy(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, allowed := range allowedDownloadHosts {
		if h == allowed || strings.HasSuffix(h, "."+allowed) {
			return true
		}
	}
	return false
}

// downloadAssetURL resolves the URL asset bytes stream from. The
// $CLOAKBROWSER_DOWNLOAD_URL override rewrites the URL onto its base
// first (its host is allowed by construction); otherwise asset.URL is
// validated against the GitHub host allowlist so hostile or malformed
// API data fails typed instead of streaming from an attacker-chosen
// host.
func downloadAssetURL(asset ghAsset) (string, error) {
	if asset.URL == "" {
		return "", fmt.Errorf("cfbrowser: asset %q has no download URL", asset.Name)
	}
	au, err := url.Parse(asset.URL)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: asset %q has malformed download URL %q: %w", asset.Name, asset.URL, err)
	}
	if (au.Scheme != "http" && au.Scheme != "https") || au.Host == "" {
		return "", fmt.Errorf("cfbrowser: asset %q has non-http download URL %q", asset.Name, asset.URL)
	}
	if base := os.Getenv(EnvDownloadURL); base != "" {
		return rewriteOntoBase(au, base)
	}
	if !hostAllowedBy(au.Hostname()) {
		return "", &DownloadHostError{Asset: asset.Name, Host: au.Hostname()}
	}
	return asset.URL, nil
}

// rewriteOntoBase rewrites the parsed URL onto the override base
// (path and query kept). Shared by asset downloads and the
// github-shaped manifest origin.
func rewriteOntoBase(au *url.URL, base string) (string, error) {
	bu, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: $%s %q: %w", EnvDownloadURL, base, err)
	}
	if (bu.Scheme != "http" && bu.Scheme != "https") || bu.Host == "" {
		return "", fmt.Errorf("cfbrowser: $%s %q is not an absolute http(s) URL", EnvDownloadURL, base)
	}
	bu.Path = strings.TrimSuffix(bu.Path, "/") + au.Path
	bu.RawQuery = au.RawQuery
	return bu.String(), nil
}

// rewriteWithDownloadOverride applies the $CLOAKBROWSER_DOWNLOAD_URL
// base rewrite to an absolute URL string (identity when unset).
func rewriteWithDownloadOverride(rawURL string) (string, error) {
	au, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: rewrite %q: %w", rawURL, err)
	}
	if (au.Scheme != "http" && au.Scheme != "https") || au.Host == "" {
		return "", fmt.Errorf("cfbrowser: rewrite %q: not an absolute http(s) URL", rawURL)
	}
	base := os.Getenv(EnvDownloadURL)
	if base == "" {
		return rawURL, nil
	}
	return rewriteOntoBase(au, base)
}

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
// asset — the Pro line and darwin-less tags — are skipped.
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

// FreeReleaseForVersion resolves the free release carrying spec.Asset
// for one exact version (the pinned-version rung). The listing walk
// tolerates newer pro-line tags; only tag chromium-v<version> can
// match.
func (g *GitHubClient) FreeReleaseForVersion(ctx context.Context, spec PlatformSpec, version string) (*FreeRelease, error) {
	wantTag := tagPrefix + version
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
		if rel.TagName != wantTag {
			continue
		}
		for _, a := range rel.Assets {
			if a.Name != spec.Asset {
				continue
			}
			return &FreeRelease{TagName: rel.TagName, Version: version, Asset: a}, nil
		}
	}
	return nil, &AssetUnavailableError{Platform: spec, LatestTag: latest}
}

// DownloadAsset streams asset bytes into w while hashing (SHA-256) and
// reporting progress as integer percent (5% granularity is the
// caller's concern; this reports every chunk crossing a percent).
// progress may be nil. The returned digest is "sha256:<hex>". The
// download URL passes through downloadAssetURL (host allowlist /
// $CLOAKBROWSER_DOWNLOAD_URL override) before anything is fetched.
func (g *GitHubClient) DownloadAsset(ctx context.Context, asset ghAsset, progress func(pct int), w io.Writer) (string, error) {
	dlURL, err := downloadAssetURL(asset)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
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
