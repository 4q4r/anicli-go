package cfbrowser

import (
	"fmt"
	"os"

	"github.com/an0nx/anicli-go/internal/config"
)

// InstallHint is the action line every missing-binary error carries.
const InstallHint = "anicli cf install"

// BinaryMissingError reports that no stealth-Chromium binary resolves
// locally and none can be fetched right now (offline or not asked
// to). Carries the install command.
type BinaryMissingError struct {
	// Cause is the underlying resolution failure (offline probe…).
	Cause error
}

// Error implements error with the install hint.
func (e *BinaryMissingError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("cfbrowser: stealth chromium not installed (%v) — выполните: %s",
			e.Cause, InstallHint)
	}
	return fmt.Sprintf("cfbrowser: stealth chromium not installed — выполните: %s", InstallHint)
}

// Unwrap exposes the resolution failure.
func (e *BinaryMissingError) Unwrap() error { return e.Cause }

// ResolveOptions scopes binary resolution (tests inject cache dirs).
type ResolveOptions struct {
	// CacheDir overrides the cache directory.
	CacheDir string
	// BinaryPath overrides $CLOAKBROWSER_BINARY_PATH.
	BinaryPath string
	// Channel selects the offline resolution order: "auto" (default,
	// "" incl.) prefers the newest chromedp-compatible pro-marked dir
	// under a valid cached license, then the filtered free scan;
	// "free" never consults the pro line; "pro" keeps the pre-PR73
	// order (newest pro-marked dir, then the unfiltered scan).
	Channel string
}

// ResolveCurrentBinary resolves the browser binary WITHOUT network
// access: $CLOAKBROWSER_BINARY_PATH > pinned $CLOAKBROWSER_VERSION
// (cache only) > per channel —
//
//   - auto: with a valid cached license, the newest
//     chromedp-compatible pro-marked cache directory > the filtered
//     free scan (pro-marked dirs never qualify; incompatible majors
//     are skipped, an only-incompatible cache fails with a
//     CompatError);
//   - free: the filtered free scan only;
//   - pro: with a valid cached license the newest pro-marked
//     directory > the newest complete cache chromium-*/ directory
//     (pre-PR73 behavior).
//
// The reported channel is the resolved directory's factual install
// line (its .channel marker), never the license tier: a valid key
// over a free-only cache honestly reports the free line (the next
// online install/update lands pro). It is the registry-build-time
// check and the solver's lazy-launch resolution, so auto-updated
// binaries are picked up on the next solve. A missing binary fails
// with BinaryMissingError (carrying the `anicli cf install` hint).
func ResolveCurrentBinary(opts ResolveOptions) (*BinaryInfo, error) {
	channel, err := normalizeChannel(opts.Channel)
	if err != nil {
		return nil, err
	}
	override := opts.BinaryPath
	if override == "" {
		override = os.Getenv(EnvBinaryPath)
	}
	if override != "" {
		return resolveOverride(override)
	}
	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return nil, &BinaryMissingError{Cause: err}
	}
	spec, err := CurrentPlatform()
	if err != nil {
		return nil, err
	}
	if pinned := os.Getenv(EnvVersion); pinned != "" {
		if bin, ok := scanCacheVersion(cacheDir, spec, pinned); ok {
			return bin, nil
		}
		return nil, &BinaryMissingError{
			Cause: fmt.Errorf("pinned version %s is not installed ($%s)", pinned, EnvVersion),
		}
	}
	// Pro preference under a valid cached license (auto and pro): the
	// newest pro-marked directory outranks the generic free scan, so
	// solve sessions launch the pro line whenever it is installed.
	// auto applies the chromedp bound — an incompatible pro build
	// stays untouched and the free line serves; with no usable pro
	// directory the free scan keeps solves working.
	if channel != channelFree && cachedLicenseValid(cacheDir) {
		scan := scanProCache
		if channel == channelAuto {
			scan = scanProCacheCompat
		}
		if bin, ok := scan(cacheDir, spec); ok {
			return bin, nil
		}
	}
	if channel == channelPro {
		if bin, ok := scanCache(cacheDir, spec); ok {
			return bin, nil
		}
		return nil, &BinaryMissingError{}
	}
	bin, ok, err := scanCacheFree(cacheDir, spec)
	if err != nil {
		return nil, err // CompatError: loud, names the fix
	}
	if ok {
		return bin, nil
	}
	return nil, &BinaryMissingError{}
}

// cachedLicenseValid reports the offline license state: a cache
// entry matching the configured key whose effective status is valid
// (fresh or stale — no network either way).
func cachedLicenseValid(cacheDir string) bool {
	key := ResolveLicenseKey(cacheDir)
	if key == "" {
		return false
	}
	entry, ok := readLicenseCache(cacheDir, key)
	return ok && entry.effectiveStatus().Valid
}

// defaultProfileDir resolves DataDir()/cfprofile — the persistent
// browser profile shared by every solve.
func defaultProfileDir() (string, error) {
	base, err := config.DataDir()
	if err != nil {
		return "", fmt.Errorf("cfbrowser: resolve profile dir: %w", err)
	}
	return base + string(os.PathSeparator) + "cfprofile", nil
}
