package cfbrowser

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// manifestPair is one origin-1 manifest payload.
type manifestPair struct {
	sums []byte
	sig  string // base64; may be signed by a wrong key on purpose
}

// proInstallFixture is the all-in-one fake upstream: license API,
// pro download API, origin-1 manifests and the GitHub free line, all
// on one httptest host. Every piece is optional (unset → 404).
type proInstallFixture struct {
	srv *httptest.Server
	mu  sync.Mutex

	licenseValid bool
	licensePlan  string                  // "" → defaults to "pro" in the validate answer
	proVersion   string                  // "" → /api/download/version 404s
	proArchives  map[string][]byte       // version → archive bytes
	manifests    map[string]manifestPair // tag → free-line manifest ({base}/chromium-v{tag}/…)
	proManifests map[string]manifestPair // version → pro-line manifest ({base}/releases/pro/chromium-v{v}/…)
	githubTags   []string
	githubAssets map[string][]ghAsset
	githubBodies map[string]string

	proVersionHits  int
	proDownloadHits int
	licenseHits     int
	freeLineHits    int // probes against the FREE manifest line (/chromium-v*/SHA256SUMS*)
}

func newProInstallFixture(t *testing.T) *proInstallFixture {
	t.Helper()
	fx := &proInstallFixture{
		proArchives:  map[string][]byte{},
		manifests:    map[string]manifestPair{},
		proManifests: map[string]manifestPair{},
		githubAssets: map[string][]ghAsset{},
		githubBodies: map[string]string{},
	}
	fx.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		switch {
		case r.URL.Path == "/api/license/validate":
			fx.licenseHits++
			if fx.licenseValid {
				plan := fx.licensePlan
				if plan == "" {
					plan = "pro"
				}
				_, _ = w.Write([]byte(`{"valid":true,"plan":"` + plan + `","expires":"2099-01-01"}`))
			} else {
				_, _ = w.Write([]byte(`{"valid":false,"plan":"","expires":""}`))
			}
		case r.URL.Path == "/api/download/version":
			fx.proVersionHits++
			if fx.proVersion == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"version":"` + fx.proVersion + `","requested_channel":"stable","resolved_channel":"stable"}`))
		case strings.HasPrefix(r.URL.Path, "/api/download/"):
			fx.proDownloadHits++
			version := strings.TrimPrefix(r.URL.Path, "/api/download/")
			archive, ok := fx.proArchives[version]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(archive)
		case isProManifestPath(r.URL.Path):
			version := proManifestVersion(r.URL.Path)
			pair, ok := fx.proManifests[version]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, ".sig") {
				_, _ = w.Write([]byte(pair.sig))
			} else {
				_, _ = w.Write(pair.sums)
			}
		case isOrigin1ManifestPath(r.URL.Path):
			fx.freeLineHits++
			tag := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
			pair, ok := fx.manifests[tag]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, ".sig") {
				_, _ = w.Write([]byte(pair.sig))
			} else {
				_, _ = w.Write(pair.sums)
			}
		case r.URL.Path == "/repos/CloakHQ/cloakbrowser/releases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(releaseFixture("http://"+r.Host, fx.githubTags, fx.githubAssets)))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := fx.githubBodies[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fx.srv.Close)
	t.Setenv(EnvDownloadURL, fx.srv.URL)
	return fx
}

func isOrigin1ManifestPath(p string) bool {
	return strings.HasPrefix(p, "/chromium-v") &&
		(strings.HasSuffix(p, "/"+sumsAssetName) || strings.HasSuffix(p, "/"+sumsAssetName+".sig"))
}

// isProManifestPath matches the distinct pro release line the
// upstream pro verifier fetches ({base}/releases/pro/chromium-v{v}/…).
func isProManifestPath(p string) bool {
	return strings.HasPrefix(p, "/releases/pro/"+tagPrefix) &&
		(strings.HasSuffix(p, "/"+sumsAssetName) || strings.HasSuffix(p, "/"+sumsAssetName+".sig"))
}

// proManifestVersion extracts {v} from /releases/pro/chromium-v{v}/SHA256SUMS[.sig].
func proManifestVersion(p string) string {
	v := strings.TrimPrefix(p, "/releases/pro/"+tagPrefix)
	return strings.TrimSuffix(strings.TrimSuffix(v, "/"+sumsAssetName+".sig"), "/"+sumsAssetName)
}

func (fx *proInstallFixture) opts(t *testing.T, cacheDir string) InstallOptions {
	t.Helper()
	return InstallOptions{
		CacheDir:       cacheDir,
		APIBase:        fx.srv.URL,
		DownloadBase:   fx.srv.URL,
		LicenseAPIBase: fx.srv.URL,
		Platform:       linuxSpec(t),
		// These tests pin the pre-PR73 license-keyed ladder — now the
		// explicit pro channel. (Auto, the default, degrades a failed
		// pro pull to the free base; the auto-specific matrix lives in
		// install_channel_test.go.)
		Channel: channelPro,
		Logger:  testLogger(t),
	}
}

// signWithManifest registers an origin-1 manifest for tag, signed by
// the given key over the archive's true digest (nil signer → empty
// sig bytes). The version= binding line carries the tag's version.
func (fx *proInstallFixture) signWithManifest(tag, archiveName string, archive []byte, priv ed25519.PrivateKey) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	version, err := ParseVersionFromTag(tag)
	if err != nil {
		panic("signWithManifest: bad tag " + tag + ": " + err.Error())
	}
	sums := manifestBody(version, archiveName, digestHexOf(archive))
	sig := ""
	if priv != nil {
		sig = signManifest(priv, sums)
	}
	fx.manifests[tag] = manifestPair{sums: sums, sig: sig}
}

// signWithProManifest registers a manifest on the pro release line
// ({base}/releases/pro/chromium-v{version}/…), signed over the
// archive's true digest with the version binding line.
func (fx *proInstallFixture) signWithProManifest(version, archiveName string, archive []byte, priv ed25519.PrivateKey) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	sums := manifestBody(version, archiveName, digestHexOf(archive))
	sig := ""
	if priv != nil {
		sig = signManifest(priv, sums)
	}
	fx.proManifests[version] = manifestPair{sums: sums, sig: sig}
}

func (fx *proInstallFixture) addFreeRelease(tag, version string, archive []byte) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.githubTags = append(fx.githubTags, tag)
	fx.githubAssets[tag] = []ghAsset{
		{Name: linuxX64Asset, Size: int64(len(archive)), Digest: sha256Hex(archive),
			URL: fx.srv.URL + "/dl/" + tag + "/" + linuxX64Asset},
	}
	fx.githubBodies[linuxX64Asset] = string(archive)
}

func proArchive(t *testing.T) []byte {
	t.Helper()
	return buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		"chromium-151.0.7922.108.6/chrome": {0o755, "ELF-PRO"},
	})
}

func freeArchive(t *testing.T, version string) []byte {
	t.Helper()
	return buildTarGz(t, map[string]struct {
		mode os.FileMode
		data string
	}{
		VersionDirName(version) + "/chrome": {0o755, "ELF"},
	})
}

func TestInstallProDownloadVerifiesAndInstalls(t *testing.T) {
	probeAlways(t)
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, priv)

	cache := t.TempDir()
	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Version != "151.0.7922.108.6" || info.Channel != channelPro {
		t.Errorf("info = %+v, want pro 151.0.7922.108.6", info)
	}
	if !strings.HasSuffix(info.Path, filepath.Join("chromium-151.0.7922.108.6", "chrome")) {
		t.Errorf("path = %q", info.Path)
	}
	if _, err := os.Stat(filepath.Join(cache, proMarkerName("linux-x64"))); err != nil {
		t.Errorf("pro version marker must be written: %v", err)
	}
}

func TestInstallProVerifiesAgainstProManifestOrigin(t *testing.T) {
	probeAlways(t)
	// Production reality (upstream download.py:584): pro manifests
	// live ONLY on the distinct pro release line. With the pro line
	// as the sole manifest source, the pro install must verify — and
	// the FREE manifest line must never be probed for a pro archive.
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, priv)

	cache := t.TempDir()
	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("pro install must verify against the pro manifest origin: %v", err)
	}
	if info.Version != "151.0.7922.108.6" || info.Channel != channelPro {
		t.Errorf("info = %+v, want pro 151.0.7922.108.6", info)
	}
	if fx.freeLineHits != 0 {
		t.Errorf("the pro channel must never probe the free manifest line (hits=%d)", fx.freeLineHits)
	}
}

// swapManifestKey installs a test verification key for one test
// (restored on cleanup). Same-package seam only — production code
// paths always verify against the pinned key.
func swapManifestKey(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	old := manifestPublicKey
	manifestPublicKey = pub
	t.Cleanup(func() { manifestPublicKey = old })
}

func TestInstallPinnedVersionUsesCache(t *testing.T) {
	fx := newProInstallFixture(t)
	cache := t.TempDir()
	existing := fakeInstalledBinary(t, cache, "147.0.0.0.1")
	t.Setenv(EnvVersion, "147.0.0.0.1")

	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != existing || info.Channel != channelFree {
		t.Errorf("info = %+v, want cached %q channel free", info, existing)
	}
	if fx.proDownloadHits != 0 || fx.licenseHits != 0 {
		t.Errorf("pinned cache hit must not touch the network (pro=%d lic=%d)", fx.proDownloadHits, fx.licenseHits)
	}
}

func TestInstallPinnedVersionDownloadsFreeTagWithoutLicense(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = false
	archive := freeArchive(t, "147.0.0.0.1")
	fx.addFreeRelease("chromium-v147.0.0.0.1", "147.0.0.0.1", archive)

	cache := t.TempDir()
	t.Setenv(EnvVersion, "147.0.0.0.1")

	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Version != "147.0.0.0.1" || info.Channel != channelFree {
		t.Errorf("info = %+v, want 147.0.0.0.1 free", info)
	}
	if fx.proDownloadHits != 0 {
		t.Errorf("no license: the pro API must stay untouched (hits=%d)", fx.proDownloadHits)
	}
}

func TestInstallProValidLicenseFreeCachedDownloadsPro(t *testing.T) {
	probeAlways(t)
	// The live-verified bug: a valid license must NOT reuse the
	// newest cached dir merely because it exists — that dir came from
	// the FREE line. Pro-cached activation means "the pro-resolved
	// version matches an installed dir", so with only a free 146
	// cached the install must resolve pro latest (151) and download
	// it, leaving the free dir untouched and unused.
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, priv)

	cache := t.TempDir()
	freePath := fakeInstalledBinary(t, cache, "146.0.7680.177.5") // free-line cache

	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Version != "151.0.7922.108.6" || info.Channel != channelPro {
		t.Errorf("info = %+v, want pro 151.0.7922.108.6 (free cache must not satisfy the pro tier)", info)
	}
	if info.Path == freePath {
		t.Errorf("path = %q: the free 146 binary must not be returned to a pro user", info.Path)
	}
	if fx.proDownloadHits == 0 {
		t.Errorf("the pro archive must have been fetched (hits=%d)", fx.proDownloadHits)
	}
	if data, rerr := os.ReadFile(freePath); rerr != nil || string(data) != "fake-elf" { //nolint:gosec // test-owned temp path
		t.Errorf("the free cache dir must stay untouched: %q, %v", data, rerr)
	}
}

func TestInstallProProCachedExactVersionReusesWithoutDownload(t *testing.T) {
	probeAlways(t)
	// Corrected reuse semantics: the pro-resolved version matches a
	// PRO-installed dir → activate it, zero archive traffic.
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	cache := t.TempDir()
	existing := fakeInstalledBinary(t, cache, "146.0.7680.177.5")
	if err := os.WriteFile(filepath.Join(cache, VersionDirName("146.0.7680.177.5"), ".channel"), []byte("pro"), 0o644); err != nil { //nolint:gosec // test-owned temp path
		t.Fatal(err)
	}
	fx.mu.Lock()
	fx.proVersion = "146.0.7680.177.5"
	fx.mu.Unlock()

	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != existing {
		t.Errorf("path = %q, want cached %q", info.Path, existing)
	}
	if info.Channel != channelPro {
		t.Errorf("channel = %q, want pro (pro-resolved exact version is pro-installed)", info.Channel)
	}
	if fx.proDownloadHits != 0 {
		t.Errorf("pro-cached activation must not re-download (hits=%d)", fx.proDownloadHits)
	}
}

func TestInstallInvalidLicenseReusesFreeCache(t *testing.T) {
	probeAlways(t)
	// An invalid key keeps the classic free semantics: the newest
	// cached binary is reused, reported as the free line.
	fx := newProInstallFixture(t)
	fx.licenseValid = false
	t.Setenv(EnvLicenseKey, "KEY-REJECTED")
	cache := t.TempDir()
	existing := fakeInstalledBinary(t, cache, "146.0.7680.177.5")

	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Path != existing || info.Channel != channelFree {
		t.Errorf("info = %+v, want the cached binary labeled free", info)
	}
	if fx.proDownloadHits != 0 {
		t.Errorf("an invalid key must never fetch pro archives (hits=%d)", fx.proDownloadHits)
	}
}

func TestInstallProNonPinnedManifestFailsVerification(t *testing.T) {
	// The pinned Ed25519 key is the only trust root; a manifest
	// signed by any other key must brick the pro install.
	_, priv := manifestTestKey(t)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, priv)

	cache := t.TempDir()
	_, err := Install(context.Background(), fx.opts(t, cache))
	var verification *BinaryVerificationError
	if !errors.As(err, &verification) {
		t.Fatalf("pro install with a non-pinned manifest signature must fail as BinaryVerificationError, got %v", err)
	}
	if fx.proDownloadHits == 0 {
		t.Errorf("the pro archive must have been fetched")
	}
	if _, err := os.Stat(filepath.Join(cache, "chromium-151.0.7922.108.6")); !os.IsNotExist(err) {
		t.Errorf("unverifiable pro archive must not install")
	}
}

func TestInstallProManifestUnavailableFailsLoudNoFreeDowngrade(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	// A perfectly good free release sits ready on the same host…
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", freeArchive(t, "146.0.7680.177.5"))
	// …but no manifests anywhere: the pro install must fail loud and
	// must NOT silently fall back to the free binary.

	cache := t.TempDir()
	_, err := Install(context.Background(), fx.opts(t, cache))
	if err == nil {
		t.Fatal("pro manifests unreachable must fail the install")
	}
	if !strings.Contains(err.Error(), "pro") {
		t.Errorf("error must name the pro channel: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "chromium-146.0.7680.177.5")); !os.IsNotExist(statErr) {
		t.Errorf("no free downgrade may install")
	}
}

func TestInstallFreeBadManifestBeatsGoodDigest(t *testing.T) {
	// Free path: origin-1 serves SUMS+sig signed by a WRONG key while
	// the GitHub API digest field is CORRECT. The signed manifest is
	// primary: the install must fail, proving the digest field was
	// not consulted while manifests were present.
	_, wrongSigner := manifestTestKey(t)
	archive := freeArchive(t, "146.0.7680.177.5")
	fx := newProInstallFixture(t)
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", archive)
	fx.signWithManifest("chromium-v146.0.7680.177.5", linuxX64Asset, archive, wrongSigner)

	cache := t.TempDir()
	_, err := Install(context.Background(), fx.opts(t, cache))
	var verification *BinaryVerificationError
	if !errors.As(err, &verification) {
		t.Fatalf("free install must honor the (bad) signed manifest over the good API digest, got %v", err)
	}
}

func TestInstallFreeDigestFallbackOnlyWithoutManifests(t *testing.T) {
	probeAlways(t)
	// Manifests absent everywhere (origin1 404, origin2 404 via env
	// rewrite): the documented digest-field fallback installs.
	archive := freeArchive(t, "146.0.7680.177.5")
	srv := newFixtureServer(t,
		[]string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{linuxX64Asset, int64(len(archive)), sha256Hex(archive), ""}},
		},
		map[string]string{linuxX64Asset: string(archive)},
	)
	cache := t.TempDir()
	info, err := Install(context.Background(), InstallOptions{
		CacheDir:     cache,
		APIBase:      srv.URL,
		DownloadBase: srv.URL,
		Platform:     linuxSpec(t),
		Logger:       testLogger(t),
	})
	if err != nil {
		t.Fatalf("digest fallback must install when no SUMS exist anywhere: %v", err)
	}
	if info.Version != "146.0.7680.177.5" || info.Channel != channelFree {
		t.Errorf("info = %+v", info)
	}
}

func TestInstallLicenseResolutionFailureFallsToFree(t *testing.T) {
	probeAlways(t)
	// A key holder whose validate API is unreachable (no cache)
	// fails open to the FREE tier — public, signed — instead of
	// bricking the install.
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()
	t.Setenv(EnvLicenseKey, "KEY-1")

	archive := freeArchive(t, "146.0.7680.177.5")
	srv := newFixtureServer(t,
		[]string{"chromium-v146.0.7680.177.5"},
		map[string][]ghAsset{
			"chromium-v146.0.7680.177.5": {{linuxX64Asset, int64(len(archive)), sha256Hex(archive), ""}},
		},
		map[string]string{linuxX64Asset: string(archive)},
	)
	cache := t.TempDir()
	info, err := Install(context.Background(), InstallOptions{
		CacheDir:       cache,
		APIBase:        srv.URL,
		DownloadBase:   srv.URL,
		LicenseAPIBase: deadURL,
		Platform:       linuxSpec(t),
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Channel != channelFree {
		t.Errorf("channel = %q, want free (license unprovable)", info.Channel)
	}
}

func TestInstallPinnedVersionProFirstWithLicense(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	t.Setenv(EnvVersion, "151.0.7922.108.6")

	cache := t.TempDir()
	_, err := Install(context.Background(), fx.opts(t, cache))
	// The pro download happens even though /api/download/version is
	// 404 (pinned skips "latest"); with no manifests the install
	// fails loud — but the PRO endpoint must have been the one hit.
	if fx.proDownloadHits == 0 {
		t.Fatalf("pinned version with a valid license must try the pro channel first")
	}
	if err == nil {
		t.Fatal("manifests unreachable must still fail loud")
	}
}

func TestInstallPinnedVersionPro404FallsToFreeTag(t *testing.T) {
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	t.Setenv(EnvLicenseKey, "KEY-1")
	// Pro has no 147.0.0.0.1 (proArchives empty → 404); the free tag exists.
	archive := freeArchive(t, "147.0.0.0.1")
	fx.addFreeRelease("chromium-v147.0.0.0.1", "147.0.0.0.1", archive)
	t.Setenv(EnvVersion, "147.0.0.0.1")

	cache := t.TempDir()
	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Version != "147.0.0.0.1" || info.Channel != channelFree {
		t.Errorf("info = %+v, want the free tag fallback", info)
	}
	if fx.proDownloadHits == 0 {
		t.Errorf("the pro channel must have been tried before the free fallback")
	}
}

func TestInstallFreePlanDropsVersionPin(t *testing.T) {
	probeAlways(t)
	// Upstream parity (download.py): a VALID license on plan "free"
	// has its version pin dropped — the server force-serves the
	// latest build to free keys, so fetching the pinned version's
	// manifest would mismatch the served bytes. The install must
	// resolve pro LATEST, never the pin.
	pub, priv := manifestTestKey(t)
	swapManifestKey(t, pub)
	fx := newProInstallFixture(t)
	fx.licenseValid = true
	fx.licensePlan = "free"
	t.Setenv(EnvLicenseKey, "KEY-1")
	archive := proArchive(t)
	fx.mu.Lock()
	fx.proVersion = "151.0.7922.108.6"
	fx.proArchives["151.0.7922.108.6"] = archive
	fx.mu.Unlock()
	fx.signWithProManifest("151.0.7922.108.6", linuxX64Asset, archive, priv)
	t.Setenv(EnvVersion, "150.0.0.0.1") // pin ≠ the served latest

	cache := t.TempDir()
	info, err := Install(context.Background(), fx.opts(t, cache))
	if err != nil {
		t.Fatalf("free-plan license must install the force-served latest, not the pin: %v", err)
	}
	if info.Version != "151.0.7922.108.6" || info.Channel != channelPro {
		t.Errorf("info = %+v, want pro 151.0.7922.108.6 (pin dropped)", info)
	}
	if fx.proDownloadHits == 0 {
		t.Errorf("the pro download must have run (via latest resolution)")
	}
}

func TestInstallRejectedLicenseKeyWarnsAndFallsToFree(t *testing.T) {
	probeAlways(t)
	// A definitively rejected key (valid:false) resolves as the free
	// tier — loudly. The never-downgrade rule guards VERIFICATION
	// failures; an invalid key is a configuration signal the user
	// must see in the logs.
	fx := newProInstallFixture(t)
	fx.licenseValid = false
	t.Setenv(EnvLicenseKey, "KEY-REJECTED")
	archive := freeArchive(t, "146.0.7680.177.5")
	fx.addFreeRelease("chromium-v146.0.7680.177.5", "146.0.7680.177.5", archive)

	var logs lockedBuffer
	cache := t.TempDir()
	opts := fx.opts(t, cache)
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	info, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if info.Channel != channelFree {
		t.Errorf("channel = %q, want free (rejected key)", info.Channel)
	}
	if !strings.Contains(logs.String(), "rejected") {
		t.Errorf("a rejected key must be logged loudly before free resolution; logs:\n%s", logs.String())
	}
}

// lockedBuffer is a concurrency-safe io.Writer for slog capture.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
