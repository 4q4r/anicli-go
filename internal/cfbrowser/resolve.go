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
// access: $CLOAKBROWSER_BINARY_PATH > newest complete cache
// chromium-*/ directory. It is the registry-build-time check and the
// solver's lazy-launch resolution, so auto-updated binaries are
// picked up on the next solve. A missing binary fails with
// BinaryMissingError (carrying the `anicli cf install` hint).
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
	if bin, ok := scanCache(cacheDir, spec); ok {
		return bin, nil
	}
	return nil, &BinaryMissingError{}
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
