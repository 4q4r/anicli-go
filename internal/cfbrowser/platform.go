// Package cfbrowser embeds a Cloudflare-bypass capability into anicli:
// it manages prebuilt CloakBrowser stealth-Chromium binaries (download
// from the free GitHub release line, SHA-256 verification, unpack into
// the ~/.cloakbrowser cache), drives them over CDP via chromedp to
// harvest cf_clearance cookies, and feeds those clearances back into
// the shared netclient HTTP layer (challenge ladder).
//
// Cache contract (mirrors the upstream CloakBrowser layout):
//
//	~/.cloakbrowser/                     ($CLOAKBROWSER_CACHE_DIR)
//	  chromium-<version>/chrome          linux layout (+chromedriver)
//	  chromium-<version>/chrome.exe      windows layout
//	  chromium-<ver>/Chromium.app/...    darwin layout
//	  license.key                        pro license key (trimmed text)
//	  .license_cache                     last validation (sha256(key)-keyed)
//	  .last_pro_version_check_<tag>      pro version marker cache
//	  update-status.json                 auto-update bookkeeping
//
// $CLOAKBROWSER_BINARY_PATH overrides resolution with a user-supplied
// binary; $CLOAKBROWSER_VERSION pins an exact version;
// $CLOAKBROWSER_LICENSE_KEY overrides license.key;
// $CLOAKBROWSER_AUTO_UPDATE=false disables the auto-updater.
package cfbrowser

import (
	"fmt"
	"runtime"
)

// archiveKind selects the unpack routine for a release asset.
type archiveKind string

const (
	archiveTarGz archiveKind = "tar.gz"
	archiveZip   archiveKind = "zip"
)

// PlatformSpec describes the release asset and on-disk executable for
// one GOOS/GOARCH pair. Asset names are verified against the live
// CloakHQ/cloakbrowser release metadata (free line, e.g.
// chromium-v146.0.7680.177.5); darwin assets exist on the release line
// only intermittently — absence surfaces as a clear typed error.
type PlatformSpec struct {
	// GOOS/GOARCH the spec resolves.
	GOOS, GOARCH string
	// Asset is the exact GitHub release asset file name.
	Asset string
	// Archive is the container format of Asset.
	Archive archiveKind
	// ExecName is the browser executable relative to the unpacked
	// chromium-<version>/ directory.
	ExecName string
}

// UnsupportedPlatformError reports a GOOS/GOARCH pair with no free
// CloakBrowser release asset.
type UnsupportedPlatformError struct {
	GOOS, GOARCH string
}

// Error implements error.
func (e *UnsupportedPlatformError) Error() string {
	return fmt.Sprintf("cfbrowser: no CloakBrowser build for %s/%s "+
		"(free line covers linux/amd64, linux/arm64, windows/amd64, darwin/amd64, darwin/arm64)", e.GOOS, e.GOARCH)
}

// platformAssetFor maps a GOOS/GOARCH pair onto its release spec.
func platformAssetFor(goos, goarch string) (PlatformSpec, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		return PlatformSpec{goos, goarch, "cloakbrowser-linux-x64.tar.gz", archiveTarGz, "chrome"}, nil
	case "linux/arm64":
		return PlatformSpec{goos, goarch, "cloakbrowser-linux-arm64.tar.gz", archiveTarGz, "chrome"}, nil
	case "windows/amd64":
		return PlatformSpec{goos, goarch, "cloakbrowser-windows-x64.zip", archiveZip, "chrome.exe"}, nil
	case "darwin/amd64":
		return PlatformSpec{goos, goarch, "cloakbrowser-darwin-x64.tar.gz", archiveTarGz,
			"Chromium.app/Contents/MacOS/Chromium"}, nil
	case "darwin/arm64":
		return PlatformSpec{goos, goarch, "cloakbrowser-darwin-arm64.tar.gz", archiveTarGz,
			"Chromium.app/Contents/MacOS/Chromium"}, nil
	default:
		return PlatformSpec{}, &UnsupportedPlatformError{GOOS: goos, GOARCH: goarch}
	}
}

// CurrentPlatform resolves the spec for the running binary's platform.
func CurrentPlatform() (PlatformSpec, error) {
	return platformAssetFor(runtime.GOOS, runtime.GOARCH)
}

// Tag returns the upstream download-API platform tag (X-Platform
// header, marker-cache key): linux-x64, linux-arm64, windows-x64,
// darwin-arm64, darwin-x64.
func (s PlatformSpec) Tag() string {
	arch := s.GOARCH
	if s.GOARCH == "amd64" {
		arch = "x64"
	}
	return s.GOOS + "-" + arch
}
