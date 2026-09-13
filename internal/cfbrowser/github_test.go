package cfbrowser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// releaseFixture renders a GitHub releases-listing JSON payload for the
// given tags; assets maps tag -> asset names (with fake digests).
func releaseFixture(baseURL string, tags []string, assets map[string][]ghAsset) string {
	var b strings.Builder
	b.WriteString("[")
	for i, tag := range tags {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"tag_name":"` + tag + `","prerelease":false,"assets":[`)
		for j, a := range assets[tag] {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"name":"` + a.Name + `","size":` + strconv.FormatInt(a.Size, 10) +
				`,"digest":"` + a.Digest + `","browser_download_url":"` + baseURL + `/dl/` + tag + `/` + a.Name + `"}`)
		}
		b.WriteString("]}")
	}
	b.WriteString("]")
	return b.String()
}

const linuxX64Asset = "cloakbrowser-linux-x64.tar.gz"

func newFixtureServer(t *testing.T, tags []string, assets map[string][]ghAsset, bodies map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/CloakHQ/cloakbrowser/releases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixture("http://"+r.Host, tags, assets)))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := bodies[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	// Fixture asset URLs point at this httptest host; route downloads
	// through the documented override so the host allowlist accepts it.
	t.Setenv(EnvDownloadURL, srv.URL)
	return srv
}

func TestLatestFreeReleaseSkipsProLine(t *testing.T) {
	proAsset := ghAsset{Name: "SHA256SUMS", Size: 300, Digest: "sha256:" + strings.Repeat("a", 64)}
	srv := newFixtureServer(t,
		[]string{"chromium-v151.0.7922.108.6-pro", "chromium-v148.0.7778.215.5-pro", "chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v151.0.7922.108.6-pro": {proAsset},
			"chromium-v148.0.7778.215.5-pro": {proAsset},
			"chromium-v146.0.7680.177.5":     {{linuxX64Asset, 1000, "sha256:" + strings.Repeat("b", 64), ""}},
		},
		nil,
	)

	gh := NewGitHubClient(srv.URL, nil)
	spec, err := platformAssetFor("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	rel, err := gh.LatestFreeRelease(context.Background(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rel.TagName != "chromium-v146.0.7680.177.5" {
		t.Errorf("tag = %q, want chromium-v146.0.7680.177.5", rel.TagName)
	}
	if rel.Version != "146.0.7680.177.5" {
		t.Errorf("version = %q", rel.Version)
	}
	if rel.Asset.Name != linuxX64Asset {
		t.Errorf("asset = %q", rel.Asset.Name)
	}
	if !strings.HasPrefix(rel.Asset.Digest, "sha256:") {
		t.Errorf("digest = %q", rel.Asset.Digest)
	}
}

func TestLatestFreeReleaseNoAssetForPlatform(t *testing.T) {
	srv := newFixtureServer(t,
		[]string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{linuxX64Asset, 1000, "sha256:x", ""}},
		},
		nil,
	)
	gh := NewGitHubClient(srv.URL, nil)
	spec, err := platformAssetFor("darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	_, err = gh.LatestFreeRelease(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for platform without asset")
	}
	var missing *AssetUnavailableError
	if !errors.As(err, &missing) {
		t.Fatalf("want *AssetUnavailableError, got %T: %v", err, err)
	}
	if missing.Platform.Asset != "cloakbrowser-darwin-arm64.tar.gz" {
		t.Errorf("error platform asset = %q", missing.Platform.Asset)
	}
	if !strings.Contains(missing.Error(), "github.com/CloakHQ/cloakbrowser/releases") {
		t.Errorf("error must carry the manual URL hint: %v", missing)
	}
}

func TestLatestFreeReleaseUnreachable(t *testing.T) {
	srv := newFixtureServer(t, nil, nil, nil)
	url := srv.URL
	srv.Close() // dead listener

	gh := NewGitHubClient(url, &http.Client{Timeout: 2 * time.Second})
	spec, _ := platformAssetFor("linux", "amd64")
	_, err := gh.LatestFreeRelease(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for unreachable API")
	}
	if !strings.Contains(err.Error(), "CloakHQ/cloakbrowser/releases") {
		t.Errorf("unreachable error must hint the manual releases URL: %v", err)
	}
}

func TestDownloadAssetStreamsBody(t *testing.T) {
	payload := strings.Repeat("cloakbrowser-bytes", 64) // 1152 bytes
	srv := newFixtureServer(t,
		[]string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{linuxX64Asset, int64(len(payload)), "sha256:whatever", ""}},
		},
		map[string]string{linuxX64Asset: payload},
	)
	gh := NewGitHubClient(srv.URL, nil)

	var buf bytes.Buffer
	pcts := []int{}
	digest, err := gh.DownloadAsset(context.Background(), ghAsset{
		Name:   linuxX64Asset,
		Size:   int64(len(payload)),
		Digest: "sha256:ignored-here",
		URL:    srv.URL + "/dl/chromium-v146.0.7680.177.5/" + linuxX64Asset,
	}, func(pct int) { pcts = append(pcts, pct) }, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if buf.String() != payload {
		t.Errorf("body round-trip mismatch: %d bytes", buf.Len())
	}
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Errorf("progress must end at 100%%, got %v", pcts)
	}
	sum := sha256.Sum256([]byte(payload))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if digest != want {
		t.Errorf("digest = %q, want %q", digest, want)
	}
}

func TestDownloadAssetRejectsForeignHost(t *testing.T) {
	gh := NewGitHubClient("", &http.Client{Timeout: 5 * time.Second})
	var buf bytes.Buffer
	_, err := gh.DownloadAsset(context.Background(), ghAsset{
		Name: linuxX64Asset,
		Size: 16,
		URL:  "https://evil-github.example.com/dl/" + linuxX64Asset,
	}, nil, &buf)
	if err == nil {
		t.Fatal("expected a foreign-host download to be rejected")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("rejection must name the allowlist violation: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("nothing may be fetched from a rejected host, wrote %d bytes", buf.Len())
	}
}

func TestDownloadAssetEnvOverrideRoutesDownloads(t *testing.T) {
	payload := "override-payload"
	const assetPath = "/CloakHQ/cloakbrowser/releases/download/chromium-v146.0.7680.177.5/" + linuxX64Asset
	var gotPath atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == assetPath {
			gotPath.Add(1)
			_, _ = w.Write([]byte(payload))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CLOAKBROWSER_DOWNLOAD_URL", srv.URL)

	gh := NewGitHubClient("", &http.Client{Timeout: 5 * time.Second})
	var buf bytes.Buffer
	_, err := gh.DownloadAsset(context.Background(), ghAsset{
		Name: linuxX64Asset,
		Size: int64(len(payload)),
		URL:  "https://github.com" + assetPath,
	}, nil, &buf)
	if err != nil {
		t.Fatalf("override-host download must be allowed and routed: %v", err)
	}
	if buf.String() != payload {
		t.Errorf("body round-trip mismatch: %d bytes", buf.Len())
	}
	if gotPath.Load() != 1 {
		t.Errorf("download must hit the override server exactly once, hits = %d", gotPath.Load())
	}
}

func TestDownloadURLValidation(t *testing.T) {
	const name = linuxX64Asset
	cases := []struct {
		name        string
		url         string
		env         string
		wantErr     bool
		wantHostErr bool
	}{
		{"github.com allowed", "https://github.com/CloakHQ/cloakbrowser/releases/download/t/" + name, "", false, false},
		{"objects cdn allowed", "https://objects.githubusercontent.com/gh-asset/" + name, "", false, false},
		{"api host allowed", "https://api.github.com/repos/CloakHQ/cloakbrowser/releases/assets/1", "", false, false},
		{"release-assets cdn allowed", "https://release-assets.githubusercontent.com/CloakHQ/cloakbrowser/releases/download/t/" + name, "", false, false},
		{"cdn subdomain suffix-matches", "https://codeload.objects.githubusercontent.com/x/" + name, "", false, false},
		{"port on allowed host", "https://github.com:443/CloakHQ/cloakbrowser/" + name, "", false, false},
		{"evil lookalike rejected", "https://evil-github.com/" + name, "", true, true},
		{"host suffix attack rejected", "https://github.com.evil.example/" + name, "", true, true},
		{"cdn suffix attack rejected", "https://objects.githubusercontent.com.evil.example/" + name, "", true, true},
		{"userinfo trick rejected", "https://github.com@evil.example/" + name, "", true, true},
		{"empty url rejected", "", "", true, false},
		{"scheme-less url rejected", "github.com/CloakHQ/cloakbrowser/" + name, "", true, false},
		{"override host allowed", "https://mirror.example/gh/" + name, "https://mirror.example", false, false},
		{"garbage override fails loud", "https://github.com/CloakHQ/cloakbrowser/" + name, "not-a-url", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOAKBROWSER_DOWNLOAD_URL", tc.env)
			got, err := downloadAssetURL(ghAsset{Name: name, URL: tc.url})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got %q", got)
				}
				if tc.wantHostErr {
					var hostErr *DownloadHostError
					if !errors.As(err, &hostErr) {
						t.Fatalf("want *DownloadHostError, got %T: %v", err, err)
					}
					if hostErr.Asset != name {
						t.Errorf("error asset = %q, want %q", hostErr.Asset, name)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}
}

func TestDownloadURLRewritesOntoOverrideBase(t *testing.T) {
	t.Setenv("CLOAKBROWSER_DOWNLOAD_URL", "https://mirror.example/gh")
	got, err := downloadAssetURL(ghAsset{
		Name: "x.tar.gz",
		URL:  "https://github.com/CloakHQ/cloakbrowser/releases/download/t/x.tar.gz?sig=1",
	})
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	want := "https://mirror.example/gh/CloakHQ/cloakbrowser/releases/download/t/x.tar.gz?sig=1"
	if got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}
