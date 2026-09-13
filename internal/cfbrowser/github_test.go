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
