package cfbrowser

// Pro binary channel (PR15): license-keyed downloads from the
// cloakbrowser.dev download API. Mirrors the upstream contract:
//
//	GET {base}/api/download/version      header X-Platform: {tag}
//	    → {"version": v, "requested_channel": c, "resolved_channel": c}
//	GET {base}/api/download/{version}    headers Authorization: Bearer {key},
//	                                              X-Platform: {tag}
//	    → the platform archive bytes
//
// {base} is the download base: $CLOAKBROWSER_DOWNLOAD_URL overrides
// the default https://cloakbrowser.dev (the same override the free
// asset downloads honor — one env, one mirror). The version lookup
// is marker-cached (.last_pro_version_check_{tag}) with a one-hour
// interval so the auto-updater's 30m cadence does not hammer the API.
// Downloaded archives STILL pass the pinned Ed25519 manifest
// verification (verify.go) — the license unlocks the channel, not
// the checks.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// proVersionMarkerInterval is how long a pro version answer
	// stays authoritative before the API is re-consulted.
	proVersionMarkerInterval = time.Hour
	defaultDownloadBase      = "https://cloakbrowser.dev"
)

// proVersionMarker is the .last_pro_version_check_{tag} JSON shape.
type proVersionMarker struct {
	Version   string `json:"version"`
	CheckedAt string `json:"checked_at"`
}

// proMarkerName renders the marker file name for a platform tag.
func proMarkerName(tag string) string {
	return ".last_pro_version_check_" + tag
}

// ProVersionOptions scopes pro version resolution.
type ProVersionOptions struct {
	// CacheDir overrides the cache directory (marker location).
	CacheDir string
	// DownloadBase overrides the download API base (empty = env >
	// default).
	DownloadBase string
	// HTTPClient overrides transport (nil = default).
	HTTPClient *http.Client
	// ProxyURL is the [cf] proxy for pro version traffic (PR80;
	// empty = direct). Ignored when HTTPClient is set.
	ProxyURL string
	// Logger receives the transport/persist diagnostics (PR85; nil =
	// discard — never slog.Default).
	Logger *slog.Logger
}

func (o ProVersionOptions) downloadBase() string {
	return resolveDownloadBase(o.DownloadBase)
}

// resolveDownloadBase resolves the download-base override chain:
// explicit > $CLOAKBROWSER_DOWNLOAD_URL > the public download API.
func resolveDownloadBase(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv(EnvDownloadURL); v != "" {
		return v
	}
	return defaultDownloadBase
}

func (o ProVersionOptions) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	hc, err := DownloadHTTPClient(o.ProxyURL, 10*time.Second)
	if err != nil {
		o.logger().Warn("cfbrowser: [cf] proxy transport unavailable; falling back to direct", "error", err)
		return &http.Client{Timeout: 10 * time.Second}
	}
	return hc
}

// logger resolves the diagnostics sink: nil degrades to discard —
// never slog.Default (PR85).
func (o ProVersionOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return discardLogger()
}

// ResolveProVersion resolves the newest pro-channel version for the
// platform tag. The marker cache answers within
// proVersionMarkerInterval without touching the API; past the
// interval (or with no marker) the API is consulted and the marker
// refreshed. API failures propagate to the caller (the updater
// defers, install fails loud).
func ResolveProVersion(ctx context.Context, tag string, opts ProVersionOptions) (string, error) {
	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return "", err
	}
	markerPath := filepath.Join(cacheDir, proMarkerName(tag))
	if raw, rerr := os.ReadFile(markerPath); rerr == nil { //nolint:gosec // app-owned cache path
		var m proVersionMarker
		if json.Unmarshal(raw, &m) == nil && m.Version != "" {
			if checked, perr := time.Parse(time.RFC3339, m.CheckedAt); perr == nil &&
				time.Since(checked) < proVersionMarkerInterval {
				return m.Version, nil
			}
		}
	}

	vctx, cancel := context.WithTimeout(ctx, licenseRequestTimeout)
	defer cancel()
	url := opts.downloadBase() + "/api/download/version"
	req, err := http.NewRequestWithContext(vctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: pro version request: %w", err)
	}
	req.Header.Set("X-Platform", tag)
	resp, err := opts.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: pro version check: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cfbrowser: pro version check: status %s", resp.Status)
	}
	var payload struct {
		Version          string `json:"version"`
		RequestedChannel string `json:"requested_channel"`
		ResolvedChannel  string `json:"resolved_channel"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("cfbrowser: decode pro version response: %w", err)
	}
	if err := validateVersion(payload.Version); err != nil {
		return "", fmt.Errorf("cfbrowser: pro version response: %w", err)
	}

	// Best-effort marker refresh: a failed write only costs an extra
	// API call on the next resolve.
	if raw, merr := json.Marshal(proVersionMarker{
		Version: payload.Version, CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}); merr == nil {
		if merr := os.MkdirAll(cacheDir, 0o750); merr != nil {
			return payload.Version, nil
		}
		if werr := os.WriteFile(markerPath, raw, 0o644); werr != nil { //nolint:gosec // non-secret bookkeeping
			opts.logger().Warn("cfbrowser: persist pro version marker", "error", werr)
		}
	}
	return payload.Version, nil
}

// proDownloadRequest scopes one pro archive download.
type proDownloadRequest struct {
	// Version is the exact version to fetch.
	Version string
	// Key is the license key (bearer token).
	Key string
	// Tag is the platform tag (X-Platform).
	Tag string
	// DownloadBase overrides the download API base (empty = env >
	// default).
	DownloadBase string
	// HTTPClient overrides transport (nil = long-timeout default:
	// archives are large).
	HTTPClient *http.Client
}

// proVersionNotFoundError reports the pro download API answering 404
// for a version — the pinned-version rung's trigger to fall through
// to the GitHub free tag.
type proVersionNotFoundError struct {
	version string
}

// Error implements error.
func (e *proVersionNotFoundError) Error() string {
	return fmt.Sprintf("cfbrowser: pro download %s: версия отсутствует в pro-канале (404)", e.version)
}

// proDownloadArchive streams the pro archive for req.Version into w
// while SHA-256 hashing and reporting progress (integer percent);
// returns the "sha256:<hex>" digest. The Authorization and
// X-Platform headers mirror the upstream contract exactly.
func proDownloadArchive(ctx context.Context, req proDownloadRequest, w io.Writer, progress func(pct int)) (string, error) {
	base := resolveDownloadBase(req.DownloadBase)
	hc := req.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}

	// No extra context deadline here: the caller's context governs
	// cancellation, and the client timeout bounds the whole transfer
	// (header phase + body) exactly like the free asset path.
	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/api/download/"+req.Version, nil)
	if err != nil {
		return "", fmt.Errorf("cfbrowser: pro download request: %w", err)
	}
	dreq.Header.Set("Authorization", "Bearer "+req.Key)
	dreq.Header.Set("X-Platform", req.Tag)
	resp, err := hc.Do(dreq)
	if err != nil {
		return "", &OfflineError{Cause: fmt.Errorf("pro download %s: %w", req.Version, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusNotFound {
			// Typed: the pinned-version rung treats this as a tier
			// signal (fall through to the GitHub free tag).
			return "", &proVersionNotFoundError{version: req.Version}
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("cfbrowser: pro download %s: статус %s — лицензионный ключ отклонён",
				req.Version, resp.Status)
		}
		return "", fmt.Errorf("cfbrowser: pro download %s: статус %s", req.Version, resp.Status)
	}

	total := resp.ContentLength
	hasher := sha256.New()
	var written int64
	nextPct := 1
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := w.Write(chunk); err != nil {
				return "", fmt.Errorf("cfbrowser: store pro archive: %w", err)
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
			return "", fmt.Errorf("cfbrowser: read pro archive: %w", rerr)
		}
	}
	if progress != nil {
		progress(100)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}
