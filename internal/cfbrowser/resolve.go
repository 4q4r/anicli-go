package cfbrowser

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/an0nx/anicli-go/internal/config"
)

// InstallHint is the action line every missing-binary error carries
// (PR86: the manual `anicli cf install` command is removed — the
// startup auto-download covers the install when online).
const InstallHint = "установка выполнится автоматически при следующем запуске с интернетом"

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
		return fmt.Sprintf("cfbrowser: stealth chromium not installed (%v) — %s",
			e.Cause, InstallHint)
	}
	return fmt.Sprintf("cfbrowser: stealth chromium not installed — %s", InstallHint)
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
	// "" incl.) prefers the newest verdict-usable pro-marked dir
	// under a valid cached license, then the filtered free scan;
	// "free" never consults the pro line; "pro" keeps the pre-PR73
	// order (newest pro-marked dir, then the unfiltered scan).
	Channel string
	// NoProbe skips the launch probe (verdicts only): a candidate
	// without a verdict is served tentatively with a loud note,
	// nothing persisted. For the advisory surfaces (`cf status`)
	// where a browser launch would be disproportionate; the solve
	// path always probes.
	NoProbe bool
	// Logger receives the pinned-bypass warning (nil = discard — never slog.Default, PR85).
	Logger *slog.Logger
}

// logger resolves the effective slog logger: an explicit Logger wins;
// nil degrades to the package discard logger — never stderr (PR85).
func (o ResolveOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return discardLogger()
}

// ResolveCurrentBinary resolves the browser binary for the solve
// path: $CLOAKBROWSER_BINARY_PATH > pinned $CLOAKBROWSER_VERSION
// (cache only) > the PR76 verdict walk, per channel —
//
//   - auto: with a valid cached license, the pro-marked candidate
//     list > the filtered free candidate list; with no license, the
//     free list only;
//   - free: the filtered free candidate list only;
//   - pro: with a valid cached license the pro-marked list; without
//     one the unfiltered scan (pre-PR73 degraded rung).
//
// Within a walk: a good verdict short-circuits, a fresh bad verdict
// skips, a missing (or expired) verdict is decided by the bounded
// launch probe (~20s, the walk's one network touch — the resolve
// itself stays download-free); exhausted candidates fall to the
// last-known-good rung and a total failure fails loud and typed
// (CompatError inside BinaryMissingError). The reported channel is
// the resolved directory's factual install line (its .channel
// marker), never the license tier: a valid key over a free-only
// cache honestly reports the free line (the next online
// install/update lands pro). It is the registry-build-time check and
// the solver's lazy-launch resolution, so auto-updated binaries are
// picked up on the next solve. A missing binary fails with
// BinaryMissingError (carrying the auto-install hint).
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
	logger := opts.logger()
	if pinned := os.Getenv(EnvVersion); pinned != "" {
		if bin, ok := scanCacheVersion(cacheDir, spec, pinned); ok {
			logger.Warn(pinnedBypassNote(pinned))
			return bin, nil
		}
		return nil, &BinaryMissingError{
			Cause: fmt.Errorf("pinned version %s is not installed ($%s)", pinned, EnvVersion),
		}
	}
	// The candidate lists, in the channel's posture order (see the
	// doc comment); the walk evaluates them back to back with ONE
	// last-known-good rung at the very end.
	var groups [][]*BinaryInfo
	filter := channel
	switch {
	case channel == channelFree:
		groups = append(groups, freeLineCandidates(cacheDir, spec))
	case channel == channelPro && cachedLicenseValid(cacheDir):
		groups = append(groups, proLineCandidates(cacheDir, spec))
	case channel == channelPro:
		// Pre-PR73 degraded rung: the unfiltered scan, factual line.
		filter = channelAuto
		groups = append(groups, scanCacheOrdered(cacheDir, spec))
	default: // auto
		if cachedLicenseValid(cacheDir) {
			groups = append(groups, proLineCandidates(cacheDir, spec))
		}
		groups = append(groups, freeLineCandidates(cacheDir, spec))
	}
	flat := make([]*BinaryInfo, 0, 8)
	for _, g := range groups {
		flat = append(flat, g...)
	}
	bin, _, err := evaluateCandidates(context.Background(), cacheDir, filter, opts.NoProbe, logger, flat)
	if err != nil {
		return nil, &BinaryMissingError{Cause: err}
	}
	return bin, nil
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
