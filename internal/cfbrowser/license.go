package cfbrowser

// License channel (PR15): the upstream CloakBrowser cache contract
// grows a Pro tier — a license key validated against the
// cloakbrowser.dev API unlocks the pro binary channel. This file
// mirrors the upstream semantics: the key lives (trimmed) at
// ~/.cloakbrowser/license.key or in $CLOAKBROWSER_LICENSE_KEY;
// validation POSTs {"license_key": K} to
// {base}/api/license/validate with a 10s budget; successful
// validations are cached at ~/.cloakbrowser/.license_cache keyed by
// sha256(key) so a different key never reads another key's answer;
// and a network failure falls back to the stale cache (offline
// grace) as long as the license has not expired by its own date.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// License-channel constants (upstream cache contract).
const (
	// EnvLicenseKey overrides the license.key file
	// ($CLOAKBROWSER_LICENSE_KEY).
	EnvLicenseKey = "CLOAKBROWSER_LICENSE_KEY"
	// EnvAPIURL overrides the license-validate API base
	// ($CLOAKBROWSER_API_URL) — the mirror/test escape hatch, same
	// pattern as $CLOAKBROWSER_DOWNLOAD_URL.
	EnvAPIURL = "CLOAKBROWSER_API_URL"
	// EnvVersion pins an exact browser version
	// ($CLOAKBROWSER_VERSION) — upstream ensureBinary precedence.
	EnvVersion = "CLOAKBROWSER_VERSION"

	licenseKeyFile   = "license.key"
	licenseCacheFile = ".license_cache"

	// licenseRequestTimeout bounds one validate round-trip.
	licenseRequestTimeout = 10 * time.Second
	// licenseCacheTTL is how long a cached validation stays fresh;
	// older entries are revalidated (and only reused when the
	// network itself fails — the stale fallback).
	licenseCacheTTL = 24 * time.Hour

	defaultLicenseAPIBase = "https://cloakbrowser.dev"
)

// LicenseStatus mirrors the validate endpoint payload.
type LicenseStatus struct {
	// Valid is the effective validity: the server's verdict AND the
	// expiry date still being in the future.
	Valid bool
	// Plan is the server-reported plan name ("pro", …).
	Plan string
	// Expires is the raw server expiry date (display + staleness).
	Expires string
}

// LicenseReport is a resolved license state: what the tier logic and
// `cf status` consume.
type LicenseReport struct {
	// Status is the effective license status.
	Status LicenseStatus
	// Cached reports the answer served from .license_cache.
	Cached bool
	// Stale marks a cache answer served past its TTL because the
	// network was unreachable (offline grace).
	Stale bool
}

// LicenseOptions scopes license resolution; zero values resolve from
// the environment (tests inject everything).
type LicenseOptions struct {
	// CacheDir overrides the cache directory.
	CacheDir string
	// APIBase overrides the validate API base (empty = env > default).
	APIBase string
	// HTTPClient overrides transport (nil = default).
	HTTPClient *http.Client
	// ProxyURL is the [cf] proxy for license check traffic (PR80;
	// empty = direct). Ignored when HTTPClient is set.
	ProxyURL string
	// Logger receives the transport fallback diagnostics (PR85; nil =
	// discard — never slog.Default).
	Logger *slog.Logger
}

// licenseCacheEntry is the .license_cache JSON shape.
type licenseCacheEntry struct {
	KeySHA256 string `json:"key_sha256"`
	Valid     bool   `json:"valid"`
	Plan      string `json:"plan"`
	Expires   string `json:"expires"`
	FetchedAt string `json:"fetched_at"`
}

// licenseKeyHash keys the cache: sha256(key) in hex.
func licenseKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// apiBase resolves the license API base: explicit > $CLOAKBROWSER_API_URL
// > the public cloakbrowser.dev API.
func (o LicenseOptions) apiBase() string {
	if o.APIBase != "" {
		return o.APIBase
	}
	if v := os.Getenv(EnvAPIURL); v != "" {
		return v
	}
	return defaultLicenseAPIBase
}

// logger resolves the diagnostics sink: nil degrades to discard —
// never slog.Default (PR85).
func (o LicenseOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return discardLogger()
}

func (o LicenseOptions) httpClient() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	// [cf] proxy (PR80): config load validated the scheme, so a build
	// failure here is a loud-warned fallback to direct, never silent.
	hc, err := DownloadHTTPClient(o.ProxyURL, licenseRequestTimeout)
	if err != nil {
		o.logger().Warn("cfbrowser: [cf] proxy transport unavailable; falling back to direct", "error", err)
		return &http.Client{Timeout: licenseRequestTimeout}
	}
	return hc
}

// ResolveLicenseKey returns the license key: $CLOAKBROWSER_LICENSE_KEY
// (exact) > the trimmed contents of cacheDir/license.key. Empty when
// neither is present (the free tier needs no key).
func ResolveLicenseKey(cacheDir string) string {
	if v := os.Getenv(EnvLicenseKey); v != "" {
		return v
	}
	dir, err := ResolveCacheDir(cacheDir)
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, licenseKeyFile)) //nolint:gosec // app-owned cache path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// CheckLicense resolves the license state for the configured key:
// fresh cache hit → cached report (no network); otherwise validate
// against the API and prime the cache. A network failure falls back
// to the stale cache while the license has not expired by date.
// No key configured → (nil, nil) — the caller's free tier. A
// definitive server answer (HTTP 200 with valid:false) is a report,
// not an error; only transport failures without a usable cache
// error out.
func CheckLicense(ctx context.Context, opts LicenseOptions) (*LicenseReport, error) {
	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	key := ResolveLicenseKey(cacheDir)
	if key == "" {
		return nil, nil
	}

	if entry, ok := readLicenseCache(cacheDir, key); ok && entry.age() < licenseCacheTTL {
		return &LicenseReport{Status: entry.effectiveStatus(), Cached: true}, nil
	}

	status, err := validateLicenseHTTP(ctx, key, opts)
	if err == nil {
		if status.Valid {
			writeLicenseCache(cacheDir, key, status)
		} else {
			removeLicenseCache(cacheDir)
		}
		return &LicenseReport{Status: status}, nil
	}
	// Network failure: stale cache is the offline grace. The entry's
	// effective status is re-derived at read time, so a license that
	// expired by its own date reports invalid (not pro) even offline.
	if entry, ok := readLicenseCache(cacheDir, key); ok {
		return &LicenseReport{Status: entry.effectiveStatus(), Cached: true, Stale: true}, nil
	}
	return nil, fmt.Errorf("cfbrowser: validate license: %w", err)
}

// validateLicenseHTTP performs the POST and decodes the payload.
func validateLicenseHTTP(ctx context.Context, key string, opts LicenseOptions) (LicenseStatus, error) {
	vctx, cancel := context.WithTimeout(ctx, licenseRequestTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"license_key": key})
	if err != nil {
		return LicenseStatus{}, err
	}
	req, err := http.NewRequestWithContext(vctx, http.MethodPost,
		opts.apiBase()+"/api/license/validate", bytes.NewReader(body))
	if err != nil {
		return LicenseStatus{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := opts.httpClient().Do(req)
	if err != nil {
		return LicenseStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return LicenseStatus{}, fmt.Errorf("license API status %s", resp.Status)
	}
	var payload struct {
		Valid   bool   `json:"valid"`
		Plan    string `json:"plan"`
		Expires string `json:"expires"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return LicenseStatus{}, fmt.Errorf("decode license response: %w", err)
	}
	return LicenseStatus{
		Valid:   payload.Valid && !licenseExpired(payload.Expires, time.Now()),
		Plan:    payload.Plan,
		Expires: payload.Expires,
	}, nil
}

// readLicenseCache loads the cache entry for key (hash mismatch or
// unreadable file = miss).
func readLicenseCache(cacheDir, key string) (licenseCacheEntry, bool) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, licenseCacheFile)) //nolint:gosec // app-owned cache path
	if err != nil {
		return licenseCacheEntry{}, false
	}
	var lc licenseCacheEntry
	if json.Unmarshal(raw, &lc) != nil || lc.KeySHA256 != licenseKeyHash(key) {
		return licenseCacheEntry{}, false
	}
	return lc, true
}

// writeLicenseCache persists a successful validation (best effort —
// cache failures degrade to revalidation, never fail the check).
func writeLicenseCache(cacheDir, key string, st LicenseStatus) {
	entry := licenseCacheEntry{
		KeySHA256: licenseKeyHash(key),
		Valid:     st.Valid,
		Plan:      st.Plan,
		Expires:   st.Expires,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(cacheDir, licenseCacheFile), raw, 0o644) //nolint:gosec // non-secret bookkeeping
}

// removeLicenseCache drops a stale/invalid cache file (best effort).
func removeLicenseCache(cacheDir string) {
	_ = os.Remove(filepath.Join(cacheDir, licenseCacheFile))
}

// age returns how long ago the entry was fetched (zero time on parse
// failure → effectively always stale, i.e. revalidated).
func (lc licenseCacheEntry) age() time.Duration {
	fetched, err := time.Parse(time.RFC3339, lc.FetchedAt)
	if err != nil {
		return time.Duration(1) << 62 // far past TTL
	}
	return time.Since(fetched)
}

// effectiveStatus re-derives the effective validity at read time: a
// cached "valid" whose expiry date has since passed is not valid.
func (lc licenseCacheEntry) effectiveStatus() LicenseStatus {
	return LicenseStatus{
		Valid:   lc.Valid && !licenseExpired(lc.Expires, time.Now()),
		Plan:    lc.Plan,
		Expires: lc.Expires,
	}
}

// licenseExpired reports whether an expiry date parses and lies in
// the past; unparseable/empty dates never expire (the server verdict
// stands).
func licenseExpired(expires string, now time.Time) bool {
	if expires == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, expires); err == nil {
			return t.Before(now)
		}
	}
	return false
}

// Login validates key against the API and, when it is valid, saves it
// (trimmed) to cacheDir/license.key and primes the validation cache.
// An invalid key is an error and is never saved.
func Login(ctx context.Context, key string, opts LicenseOptions) (*LicenseStatus, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("cfbrowser: пустой лицензионный ключ")
	}
	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	// Always hit the API on login: a saved key must be known-good.
	status, err := validateLicenseHTTP(ctx, key, opts)
	if err != nil {
		return nil, err
	}
	if !status.Valid {
		return nil, fmt.Errorf("cfbrowser: лицензионный ключ недействителен (план %q, истекает %q)",
			status.Plan, status.Expires)
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	// 0600: the key is a credential.
	if err := os.WriteFile(filepath.Join(cacheDir, licenseKeyFile), []byte(key+"\n"), 0o600); err != nil { //nolint:gosec // credential file, tight perms
		return nil, fmt.Errorf("cfbrowser: сохранить license.key: %w", err)
	}
	writeLicenseCache(cacheDir, key, status)
	return &status, nil
}

// Logout removes the license key and its validation cache from the
// cache directory (idempotent).
func Logout(opts LicenseOptions) error {
	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return err
	}
	for _, name := range []string{licenseKeyFile, licenseCacheFile} {
		if err := os.Remove(filepath.Join(cacheDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cfbrowser: удалить %s: %w", name, err)
		}
	}
	return nil
}
