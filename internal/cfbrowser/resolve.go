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
}

// ResolveCurrentBinary resolves the browser binary WITHOUT network
// access: $CLOAKBROWSER_BINARY_PATH > pinned $CLOAKBROWSER_VERSION
// (cache only) > with a valid cached license, the newest pro-marked
// cache directory > the newest complete cache chromium-*/
// directory. The reported channel is the resolved directory's
// factual install line (its .channel marker), never the license
// tier: a valid key over a free-only cache honestly reports the
// free line (the next online install/update lands pro). It is the
// registry-build-time check and the solver's lazy-launch
// resolution, so auto-updated binaries are picked up on the next
// solve. A missing binary fails with BinaryMissingError (carrying
// the `anicli cf install` hint).
func ResolveCurrentBinary(opts ResolveOptions) (*BinaryInfo, error) {
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
	// Pro preference under a valid cached license: the newest
	// pro-marked directory outranks the generic free scan, so solve
	// sessions launch the pro line whenever it is installed. With no
	// pro directory installed the free scan keeps solves working.
	if cachedLicenseValid(cacheDir) {
		if bin, ok := scanProCache(cacheDir, spec); ok {
			return bin, nil
		}
	}
	if bin, ok := scanCache(cacheDir, spec); ok {
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
