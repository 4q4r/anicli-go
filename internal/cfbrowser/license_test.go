package cfbrowser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// licenseFixture serves the /api/license/validate endpoint with a
// mutable response and a request counter.
type licenseFixture struct {
	srv  *httptest.Server
	hits atomic.Int64
	body atomic.Value // string JSON body
}

func newLicenseFixture(t *testing.T, body string) *licenseFixture {
	t.Helper()
	fx := &licenseFixture{}
	fx.body.Store(body)
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/license/validate" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		fx.hits.Add(1)
		var req struct {
			LicenseKey string `json:"license_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.LicenseKey == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fx.body.Load().(string)))
	}))
	t.Cleanup(fx.srv.Close)
	return fx
}

func (fx *licenseFixture) setBody(s string) { fx.body.Store(s) }

func licenseOpts(t *testing.T, apiURL, cacheDir string) LicenseOptions {
	t.Helper()
	return LicenseOptions{APIBase: apiURL, CacheDir: cacheDir}
}

func writeLicenseKeyFile(t *testing.T, cacheDir, content string) {
	t.Helper()
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "license.key"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckLicenseValid(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":true,"plan":"pro","expires":"2099-01-01"}`)
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-1\n")

	rep, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rep.Status.Valid {
		t.Errorf("valid = false, want true (plan %q)", rep.Status.Plan)
	}
	if rep.Status.Plan != "pro" {
		t.Errorf("plan = %q, want pro", rep.Status.Plan)
	}
	if rep.Stale {
		t.Errorf("fresh validation must not be marked stale")
	}
	// Cache primed with the key hash.
	raw, err := os.ReadFile(filepath.Join(cache, ".license_cache")) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("cache file must be written: %v", err)
	}
	var lc licenseCacheEntry
	if err := json.Unmarshal(raw, &lc); err != nil {
		t.Fatalf("cache json: %v", err)
	}
	if lc.KeySHA256 != licenseKeyHash("KEY-1") {
		t.Errorf("cache keyed %q, want sha256(KEY-1)", lc.KeySHA256)
	}
}

func TestCheckLicenseInvalidNotCached(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":false,"plan":"","expires":""}`)
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-BAD")

	rep, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Status.Valid {
		t.Errorf("valid = true, want false")
	}
	if _, err := os.Stat(filepath.Join(cache, ".license_cache")); !os.IsNotExist(err) {
		t.Errorf("invalid responses must not prime the cache")
	}
}

func TestCheckLicenseExpiredByDate(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":true,"plan":"pro","expires":"2001-01-01"}`)
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-OLD")

	rep, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Status.Valid {
		t.Errorf("expired license must report valid=false (expires in the past)")
	}
}

func TestCheckLicenseFreshCacheSkipsNetwork(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":true,"plan":"pro","expires":"2099-01-01"}`)
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-1")

	if _, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache)); err != nil {
		t.Fatal(err)
	}
	if fx.hits.Load() != 1 {
		t.Fatalf("setup: expected 1 API hit, got %d", fx.hits.Load())
	}
	// Second check within TTL: served from cache, no network.
	rep, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if fx.hits.Load() != 1 {
		t.Errorf("fresh cache must skip the network (hits=%d)", fx.hits.Load())
	}
	if !rep.Status.Valid || !rep.Cached {
		t.Errorf("rep = %+v, want cached valid", rep)
	}
}

func TestCheckLicenseStaleCacheFallbackOnNetworkFail(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":true,"plan":"pro","expires":"2099-01-01"}`)
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-1")

	if _, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache)); err != nil {
		t.Fatal(err)
	}
	// Kill the network, then age the cache past its TTL.
	url := fx.srv.URL
	fx.srv.Close()
	makeLicenseCacheStale(t, cache)

	rep, err := CheckLicense(context.Background(), licenseOpts(t, url, cache))
	if err != nil {
		t.Fatalf("stale fallback must serve the cache on network failure: %v", err)
	}
	if !rep.Status.Valid {
		t.Errorf("stale cache said invalid")
	}
	if !rep.Stale {
		t.Errorf("fallback report must be marked stale")
	}
}

func TestCheckLicenseStaleExpiredByDateRejected(t *testing.T) {
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-1")
	// Hand-write a cache entry whose expiry date has passed.
	entry := licenseCacheEntry{
		KeySHA256: licenseKeyHash("KEY-1"), Valid: true, Plan: "pro",
		Expires: "2001-01-01", FetchedAt: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
	}
	raw, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(cache, ".license_cache"), raw, 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}

	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	rep, err := CheckLicense(context.Background(), licenseOpts(t, deadURL, cache))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Status.Valid {
		t.Errorf("stale entry expired by date must not report valid")
	}
}

func TestCheckLicenseNoKeyIsFreeNil(t *testing.T) {
	rep, err := CheckLicense(context.Background(), licenseOpts(t, "http://127.0.0.1:1", t.TempDir()))
	if err != nil {
		t.Fatalf("no key must be (nil, nil), got %v", err)
	}
	if rep != nil {
		t.Errorf("rep = %+v, want nil", rep)
	}
}

func TestCheckLicenseDifferentKeySkipsCache(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":true,"plan":"pro","expires":"2099-01-01"}`)
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-A")

	if _, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache)); err != nil {
		t.Fatal(err)
	}
	// Swap the key: the cached A entry must not answer for B.
	fx.setBody(`{"valid":false,"plan":"","expires":""}`)
	writeLicenseKeyFile(t, cache, "KEY-B")
	rep, err := CheckLicense(context.Background(), licenseOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Status.Valid {
		t.Errorf("key B must be revalidated over the network, cache is keyed by sha256(key)")
	}
	if fx.hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", fx.hits.Load())
	}
}

func TestResolveLicenseKeyEnvOverFile(t *testing.T) {
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "  KEY-FILE \n")
	t.Setenv(EnvLicenseKey, "KEY-ENV")

	if got := ResolveLicenseKey(cache); got != "KEY-ENV" {
		t.Errorf("key = %q, want env value", got)
	}
	t.Setenv(EnvLicenseKey, "")
	if got := ResolveLicenseKey(cache); got != "KEY-FILE" {
		t.Errorf("key = %q, want trimmed file value", got)
	}
	if got := ResolveLicenseKey(t.TempDir()); got != "" {
		t.Errorf("absent key = %q, want empty", got)
	}
}

func TestLoginSavesKeyAndPrimesCache(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":true,"plan":"pro","expires":"2099-01-01"}`)
	cache := t.TempDir()

	st, err := Login(context.Background(), "KEY-LOGIN", licenseOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !st.Valid || st.Plan != "pro" {
		t.Errorf("status = %+v", st)
	}
	raw, err := os.ReadFile(filepath.Join(cache, "license.key")) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("license.key must be saved: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "KEY-LOGIN" {
		t.Errorf("license.key = %q, want trimmed key", got)
	}
	if _, err := os.Stat(filepath.Join(cache, ".license_cache")); err != nil {
		t.Errorf("login must prime the cache: %v", err)
	}
}

func TestLoginInvalidKeyNotSaved(t *testing.T) {
	fx := newLicenseFixture(t, `{"valid":false,"plan":"","expires":""}`)
	cache := t.TempDir()

	_, err := Login(context.Background(), "KEY-BAD", licenseOpts(t, fx.srv.URL, cache))
	if err == nil {
		t.Fatal("invalid key must fail login")
	}
	if _, statErr := os.Stat(filepath.Join(cache, "license.key")); !os.IsNotExist(statErr) {
		t.Errorf("invalid key must not be saved")
	}
}

func TestLogoutRemovesKeyAndCache(t *testing.T) {
	cache := t.TempDir()
	writeLicenseKeyFile(t, cache, "KEY-1")
	if err := os.WriteFile(filepath.Join(cache, ".license_cache"), []byte("{}"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}

	if err := Logout(LicenseOptions{CacheDir: cache}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	for _, name := range []string{"license.key", ".license_cache"} {
		if _, err := os.Stat(filepath.Join(cache, name)); !os.IsNotExist(err) {
			t.Errorf("%s must be removed by logout", name)
		}
	}
}

// makeLicenseCacheStale rewinds the cache entry's fetched_at past the
// TTL so the next check must hit the network.
func makeLicenseCacheStale(t *testing.T, cacheDir string) {
	t.Helper()
	path := filepath.Join(cacheDir, ".license_cache")
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	var lc licenseCacheEntry
	if err := json.Unmarshal(raw, &lc); err != nil {
		t.Fatal(err)
	}
	lc.FetchedAt = time.Now().Add(-2 * licenseCacheTTL).UTC().Format(time.RFC3339)
	out, _ := json.Marshal(lc)
	if err := os.WriteFile(path, out, 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}
}
