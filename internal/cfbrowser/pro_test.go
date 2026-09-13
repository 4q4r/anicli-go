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

// proFixture serves the pro API surface:
//
//	GET /api/download/version          (X-Platform)
//	GET /api/download/{version}        (Authorization + X-Platform)
//
// with request capture for header assertions.
type proFixture struct {
	srv          *httptest.Server
	versionHi    atomic.Int64
	downloadHi   atomic.Int64
	lastAuth     atomic.Value
	lastPlatform atomic.Value
	archive      atomic.Value // []byte served for /api/download/{version}
}

func newProFixture(t *testing.T, version string) *proFixture {
	t.Helper()
	fx := &proFixture{}
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.lastAuth.Store(r.Header.Get("Authorization"))
		fx.lastPlatform.Store(r.Header.Get("X-Platform"))
		switch {
		case r.URL.Path == "/api/download/version":
			fx.versionHi.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"` + version + `","requested_channel":"stable","resolved_channel":"stable"}`))
		case strings.HasPrefix(r.URL.Path, "/api/download/"):
			fx.downloadHi.Add(1)
			archive, _ := fx.archive.Load().([]byte)
			if archive == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fx.srv.Close)
	return fx
}

func proVersionOpts(t *testing.T, baseURL, cacheDir string) ProVersionOptions {
	t.Helper()
	return ProVersionOptions{DownloadBase: baseURL, CacheDir: cacheDir}
}

func TestResolveProVersionFetchesAndWritesMarker(t *testing.T) {
	fx := newProFixture(t, "151.0.7922.108.6")
	cache := t.TempDir()

	v, err := ResolveProVersion(context.Background(), "linux-x64", proVersionOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if v != "151.0.7922.108.6" {
		t.Errorf("version = %q", v)
	}
	if got := fx.lastPlatform.Load().(string); got != "linux-x64" {
		t.Errorf("X-Platform = %q, want linux-x64", got)
	}
	raw, err := os.ReadFile(filepath.Join(cache, ".last_pro_version_check_linux-x64")) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("marker must be written: %v", err)
	}
	var m proVersionMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("marker json: %v", err)
	}
	if m.Version != "151.0.7922.108.6" {
		t.Errorf("marker version = %q", m.Version)
	}
}

func TestResolveProVersionMarkerWithinIntervalSkipsAPI(t *testing.T) {
	fx := newProFixture(t, "151.0.7922.108.6")
	cache := t.TempDir()

	// Prime the marker with a fresh check and an older version: the
	// interval window must serve the marker without the API.
	writeProMarker(t, cache, "linux-x64", "150.0.0.0.1", time.Now())
	v, err := ResolveProVersion(context.Background(), "linux-x64", proVersionOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if v != "150.0.0.0.1" {
		t.Errorf("version = %q, want the marker value", v)
	}
	if fx.versionHi.Load() != 0 {
		t.Errorf("marker within interval must skip the API (hits=%d)", fx.versionHi.Load())
	}
}

func TestResolveProVersionMarkerExpiredRefetches(t *testing.T) {
	fx := newProFixture(t, "151.0.7922.108.6")
	cache := t.TempDir()

	writeProMarker(t, cache, "linux-x64", "150.0.0.0.1", time.Now().Add(-2*proVersionMarkerInterval))
	v, err := ResolveProVersion(context.Background(), "linux-x64", proVersionOpts(t, fx.srv.URL, cache))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if v != "151.0.7922.108.6" {
		t.Errorf("version = %q, want the refreshed value", v)
	}
	if fx.versionHi.Load() != 1 {
		t.Errorf("expired marker must refetch (hits=%d)", fx.versionHi.Load())
	}
	// Marker refreshed.
	m := readProMarker(t, cache, "linux-x64")
	if m.Version != "151.0.7922.108.6" {
		t.Errorf("marker version = %q, want refreshed", m.Version)
	}
}

func TestProDownloadSendsAuthAndPlatformHeaders(t *testing.T) {
	fx := newProFixture(t, "151.0.7922.108.6")
	archive := []byte("FAKE-PRO-ARCHIVE-BYTES")
	fx.archive.Store(archive)

	var got []byte
	digest, err := proDownloadArchive(context.Background(), proDownloadRequest{
		Version: "151.0.7922.108.6", Key: "KEY-1", Tag: "linux-x64",
		DownloadBase: fx.srv.URL,
	}, &byteWriter{&got}, nil)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != string(archive) {
		t.Errorf("body round-trip failed")
	}
	if digest != sha256Hex(archive) {
		t.Errorf("digest = %q, want %q", digest, sha256Hex(archive))
	}
	if auth := fx.lastAuth.Load().(string); auth != "Bearer KEY-1" {
		t.Errorf("Authorization = %q, want Bearer KEY-1", auth)
	}
	if p := fx.lastPlatform.Load().(string); p != "linux-x64" {
		t.Errorf("X-Platform = %q, want linux-x64", p)
	}
}

func TestProDownloadNon200FailsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	var sink []byte
	_, err := proDownloadArchive(context.Background(), proDownloadRequest{
		Version: "151.0.7922.108.6", Key: "KEY-1", Tag: "linux-x64",
		DownloadBase: srv.URL,
	}, &byteWriter{&sink}, nil)
	if err == nil {
		t.Fatal("403 must fail the download")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error must name the status: %v", err)
	}
}

// writeProMarker writes a marker entry with the given check time.
func writeProMarker(t *testing.T, cacheDir, tag, version string, checkedAt time.Time) {
	t.Helper()
	raw, _ := json.Marshal(proVersionMarker{
		Version: version, CheckedAt: checkedAt.UTC().Format(time.RFC3339),
	})
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, proMarkerName(tag)), raw, 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}
}

func readProMarker(t *testing.T, cacheDir, tag string) proVersionMarker {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cacheDir, proMarkerName(tag))) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatal(err)
	}
	var m proVersionMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
