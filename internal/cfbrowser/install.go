package cfbrowser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Upstream cache-contract environment variables.
const (
	EnvCacheDir   = "CLOAKBROWSER_CACHE_DIR"
	EnvBinaryPath = "CLOAKBROWSER_BINARY_PATH"
)

// Binary channels reported by BinaryInfo.
const (
	// channelUser marks an explicit $CLOAKBROWSER_BINARY_PATH override.
	channelUser = "user"
	// channelFree marks a binary from the free GitHub release line.
	channelFree = "free"
	// channelPro marks a binary resolved through the license-keyed
	// pro channel (downloaded or cached under a valid license).
	channelPro = "pro"

	// planFree is the server-reported plan name for free-tier
	// license keys: valid keys whose plan is "free" are force-served
	// the latest build (upstream drops their version pin).
	planFree = "free"
)

// Exported BinaryInfo.Channel values (display and CLI comparisons).
const (
	// ChannelUser is the $CLOAKBROWSER_BINARY_PATH override line.
	ChannelUser = channelUser
	// ChannelFree is the free GitHub release line.
	ChannelFree = channelFree
	// ChannelPro is the license-keyed pro download line.
	ChannelPro = channelPro
)

// channelMarkerFile is the install-line marker written inside every
// installed chromium-<version> directory: its content is the channel
// name ("pro"/"free"). Both channels share the same directory
// naming, so the marker is the only durable record of which line a
// directory came from. Directories without the marker (installs
// predating it, including every pro install from PR15) resolve as
// the free line — a one-time corrective re-download moves them onto
// the marked pro line.
const channelMarkerFile = ".channel"

// OfflineError reports that installation needs the network but none
// is reachable (or the API answered with a transport failure).
type OfflineError struct {
	// Cause is the underlying transport error.
	Cause error
}

// Error implements error with the manual-download hint.
func (e *OfflineError) Error() string {
	return fmt.Sprintf("cfbrowser: offline, cannot reach the CloakBrowser release API: %v "+
		"(download manually from %s into the cache dir, or set $%s)", e.Cause, ManualReleasesURL, EnvBinaryPath)
}

// Unwrap exposes the transport error for errors.Is/As.
func (e *OfflineError) Unwrap() error { return e.Cause }

// MissingDigestError reports a release asset that carries no SHA-256
// digest — neither the signed manifest nor the API digest fallback —
// so its bytes cannot be verified. Installing them anyway is refused.
type MissingDigestError struct {
	// TagName is the release whose asset is unverifiable.
	TagName string
	// AssetName is the asset that carried no digest.
	AssetName string
}

// Error implements error with the manual-download hint.
func (e *MissingDigestError) Error() string {
	return fmt.Sprintf("cfbrowser: release %s asset %s provides no SHA-256 digest "+
		"(signed manifests unreachable, API digest absent) — refusing to install unverified bytes; "+
		"retry later, download manually from %s, or set $%s",
		e.TagName, e.AssetName, ManualReleasesURL, EnvBinaryPath)
}

// BinaryInfo describes the resolved browser binary.
type BinaryInfo struct {
	// Path is the absolute executable path.
	Path string
	// Dir is the containing chromium-<version> directory ("" for the
	// user override channel).
	Dir string
	// Version is the dotted browser version ("override" for the user
	// channel).
	Version string
	// Channel is channelUser, channelFree or channelPro.
	Channel string
}

// InstallOptions parameterizes Install; zero values resolve from the
// environment and the running platform (tests inject everything).
type InstallOptions struct {
	// CacheDir overrides the cache directory resolution.
	CacheDir string
	// BinaryPath overrides $CLOAKBROWSER_BINARY_PATH.
	BinaryPath string
	// APIBase overrides the GitHub API base URL.
	APIBase string
	// DownloadBase overrides the pro download API + manifest origin-1
	// base (empty = $CLOAKBROWSER_DOWNLOAD_URL > cloakbrowser.dev).
	DownloadBase string
	// LicenseAPIBase overrides the license-validate API base (empty =
	// $CLOAKBROWSER_API_URL > cloakbrowser.dev).
	LicenseAPIBase string
	// Version pins an exact browser version ($CLOAKBROWSER_VERSION
	// semantics; empty = the env).
	Version string
	// Platform overrides the running platform (zero = current).
	Platform PlatformSpec
	// Channel selects the install ladder: "auto" (default, "" incl.)
	// = free base with a best-effort license-keyed pro upgrade;
	// "free" = the pro channel is never consulted; "pro" = the
	// license-keyed pro ladder exactly as before PR73. Config
	// ([cf] channel) validates the value at load time; an unknown
	// value here fails loud as well.
	Channel string
	// Logger receives progress lines (nil = discard — never slog.Default, PR85).
	Logger *slog.Logger
	// OnProgress, when set, receives integer download percents (0-100,
	// monotone, ~5% granularity) plus the version being downloaded —
	// the CLI's colored pre-TUI progress line (PR80). Nil = ignored.
	OnProgress func(pct int, version string)
	// ProxyURL is the [cf] proxy for download/update traffic (PR80):
	// free GitHub fetches, pro version/download calls, license checks
	// and manifest verification. Empty = direct. It never touches the
	// stealth browser's page traffic.
	ProxyURL string
	// HTTPClient overrides the download/API client (nil = default).
	HTTPClient *http.Client
}

// downloadHTTPClient resolves the effective download/update transport:
// the explicit HTTPClient override wins (tests); otherwise the [cf]
// proxy transport (PR80); otherwise direct. A proxy build failure
// (impossible after config validation) degrades loud-warned to direct.
func (o InstallOptions) downloadHTTPClient(timeout time.Duration) *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	hc, err := DownloadHTTPClient(o.ProxyURL, timeout)
	if err != nil {
		o.logger().Warn("cfbrowser: [cf] proxy transport unavailable; falling back to direct", "error", err)
		return &http.Client{Timeout: timeout}
	}
	return hc
}

// logger resolves the effective slog logger.
func (o InstallOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return discardLogger()
}

// platform resolves the effective platform spec.
func (o InstallOptions) platform() (PlatformSpec, error) {
	if o.Platform.Asset != "" {
		return o.Platform, nil
	}
	return CurrentPlatform()
}

// pinnedVersion resolves the pinned-version override.
func (o InstallOptions) pinnedVersion() string {
	if o.Version != "" {
		return o.Version
	}
	return os.Getenv(EnvVersion)
}

// licenseOptions maps the install options onto license resolution.
func (o InstallOptions) licenseOptions() LicenseOptions {
	return LicenseOptions{CacheDir: o.CacheDir, APIBase: o.LicenseAPIBase, HTTPClient: o.HTTPClient, ProxyURL: o.ProxyURL, Logger: o.Logger}
}

// ResolveCacheDir resolves the CloakBrowser cache directory:
// explicit > $CLOAKBROWSER_CACHE_DIR > ~/.cloakbrowser.
func ResolveCacheDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if v := os.Getenv(EnvCacheDir); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cfbrowser: resolve home for cache dir: %w", err)
	}
	return filepath.Join(home, ".cloakbrowser"), nil
}

// Install resolves the stealth-Chromium binary for the platform. The
// channel (opts.Channel — config [cf] channel) selects the ladder:
//
//   - free: the pro channel is never consulted — no license API, no
//     pro version lookup, no pro download, even with a configured
//     key;
//   - auto (default, "" included): free is the base; a valid license
//     key pulls the pro line as a best-effort upgrade — but only a
//     chromedp-compatible one, and any pro failure degrades loud to
//     the working free base (never break the app for the upgrade);
//   - pro: the pre-PR73 ladder exactly — license-keyed pro line,
//     failures loud, no silent free downgrade.
//
// Shared precedence inside a ladder:
//
//  1. explicit user override ($CLOAKBROWSER_BINARY_PATH or BinaryPath);
//  2. pinned version ($CLOAKBROWSER_VERSION): installed → use, else
//     download — free: straight from the GitHub free tag; auto/pro:
//     the tier the version resolves to (pro first with a valid
//     license, the free tag on a pro 404; a free-plan license drops
//     the pin, upstream force-serves latest);
//  3. the channel ladder: auto/pro with a valid license resolve the
//     pro latest version (marker-cached API) and reuse the cache only
//     when THAT exact pro-resolved version is installed as a
//     pro-marked directory; otherwise they download pro (Ed25519-
//     verified). Otherwise the free rung: newest cached binary >
//     latest free GitHub release.
//
// The free rung of the free and auto channels never serves a binary
// the verdict store freshly rejects (PR76: the probe, not a hardcoded
// bound, decides): pro-marked dirs never qualify for the free scan,
// fresh-bad majors are skipped while a usable dir exists, and a
// totally unusable line fails with the typed CompatError naming every
// attempt — after the last-known-good rung had its say. Every
// downloaded archive — either channel — passes the pinned Ed25519
// signed-manifest verification (verify.go); the free channel alone
// keeps the GitHub API digest field as a documented fallback for
// releases whose manifests are absent from both origins. A freshly
// downloaded candidate proves itself with the bounded launch probe
// before it is served (probe-bad → recorded and rejected; the bytes
// stay cached so the +7d re-probe costs a launch, not a download).
func Install(ctx context.Context, opts InstallOptions) (*BinaryInfo, error) {
	spec, err := opts.platform()
	if err != nil {
		return nil, err
	}
	logger := opts.logger()
	channel, err := normalizeChannel(opts.Channel)
	if err != nil {
		return nil, err
	}

	// 1. User override.
	override := opts.BinaryPath
	if override == "" {
		override = os.Getenv(EnvBinaryPath)
	}
	if override != "" {
		return resolveOverride(override)
	}

	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return nil, err
	}

	// channel=free: the pro line does not exist for this run — no
	// license API traffic, no pro resolution, no pro downloads.
	if channel == channelFree {
		return installFreeChannel(ctx, opts, spec, cacheDir, logger)
	}

	// License state gates the tiers (auto and pro). Unprovable
	// (offline, no cache) fails open to the free tier — public and
	// signed — while a provably-valid license routes every download
	// through pro.
	licRep, licErr := CheckLicense(ctx, opts.licenseOptions())
	if licErr != nil {
		logger.Warn("cfbrowser: license check failed; resolving as free tier", "error", licErr)
	}
	licenseValid := licRep != nil && licRep.Status.Valid
	if licenseValid {
		logger.Info("cfbrowser: license valid — pro channel", "plan", licRep.Status.Plan, "expires", licRep.Status.Expires)
	} else if licRep != nil {
		// A definitively rejected key resolves as the free tier —
		// loudly. The never-downgrade rule guards VERIFICATION
		// failures; an invalid key is a configuration signal the
		// user must see, not a silent tier switch.
		logger.Warn("cfbrowser: license key rejected — resolving as free tier", "plan", licRep.Status.Plan)
	}

	// 2. Pinned version.
	pinned := opts.pinnedVersion()
	if pinned != "" && licenseValid && licRep.Status.Plan == planFree {
		// Upstream parity: the server force-serves the latest build
		// to free-plan keys, so fetching the pinned version's signed
		// manifest would mismatch the served bytes. Drop the pin and
		// resolve latest (paid keys keep pinning/rollback).
		logger.Warn("cfbrowser: free-plan license ignores the version pin — the server force-serves the latest build",
			"pinned", pinned)
		pinned = ""
	}
	if pinned != "" {
		if err := validateVersion(pinned); err != nil {
			return nil, err
		}
		if bin, ok := scanCacheVersion(cacheDir, spec, pinned); ok {
			logger.Info("cfbrowser: reusing pinned stealth chromium", "path", bin.Path, "version", bin.Version)
			return bin, nil
		}
		return installPinned(ctx, opts, spec, cacheDir, pinned, licenseValid, licRep, logger)
	}

	// 3. Pro tier. The pro-resolved version decides reuse — never the
	// mere existence of a cached dir (a free-line dir, whatever its
	// version, must not satisfy the pro tier).
	if licenseValid {
		if channel == channelAuto {
			// Auto: the pro pull is an upgrade, never a requirement.
			bin, err := upgradeToProBestEffort(ctx, opts, spec, cacheDir, licRep, logger)
			if err != nil {
				logger.Warn("cfbrowser: pro upgrade failed — работаем на free", "error", err)
			} else if bin != nil {
				return bin, nil
			}
		} else {
			bin, err := installProLatest(ctx, opts, spec, cacheDir, licRep, logger)
			if err != nil {
				// PR76: before the loud failure, the last-known-good
				// rung gets its say (pro filter; the emergency serve).
				if lkg, lkgOk := lastKnownGoodRung(cacheDir, channelPro, logger); lkgOk {
					return lkg, nil
				}
				return nil, err
			}
			return bin, nil
		}
	}

	// 4. Free base. auto: the filtered free ladder (pro-marked dirs
	// never qualify; fresh-bad verdicts skip). pro without a valid
	// license keeps the pre-PR73 degraded rung verbatim (unfiltered
	// scan, factual channel reporting).
	if channel == channelPro {
		return cachedOrLatestLadder(ctx, opts, spec, cacheDir, channelAuto, scanCacheOrdered(cacheDir, spec), logger)
	}
	return cachedOrLatestLadder(ctx, opts, spec, cacheDir, channelFree, freeLineCandidates(cacheDir, spec), logger)
}

// installFreeChannel is the channel=free ladder: the pinned rung
// downloads from the free tag only; otherwise the filtered free
// ladder runs. The pro channel is never consulted.
func installFreeChannel(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir string, logger *slog.Logger) (*BinaryInfo, error) {
	if pinned := opts.pinnedVersion(); pinned != "" {
		if err := validateVersion(pinned); err != nil {
			return nil, err
		}
		if bin, ok := scanCacheVersion(cacheDir, spec, pinned); ok {
			logger.Warn(pinnedBypassNote(pinned))
			logger.Info("cfbrowser: reusing pinned stealth chromium", "path", bin.Path, "version", bin.Version)
			return bin, nil
		}
		return installPinnedFree(ctx, opts, spec, cacheDir, pinned, logger)
	}
	return cachedOrLatestLadder(ctx, opts, spec, cacheDir, channelFree, freeLineCandidates(cacheDir, spec), logger)
}

// cachedOrLatestLadder is the shared free-rung tail of Install: the
// verdict walk over the caller's candidate list (the channel posture
// is encoded in the list + filter) > the latest free GitHub release,
// which must pass the launch probe after install > a re-walk (the
// fresh verdicts recorded above steer it to the last working
// candidate or the last-known-good rung) > the loud typed failure.
func cachedOrLatestLadder(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir, filter string, cands []*BinaryInfo, logger *slog.Logger) (*BinaryInfo, error) {
	if bin, _, err := evaluateCandidates(ctx, cacheDir, filter, false, logger, cands); err == nil {
		logger.Info("cfbrowser: reusing cached stealth chromium",
			"path", bin.Path, "version", bin.Version, "channel", bin.Channel)
		return bin, nil
	}
	bin, err := installFreeLatest(ctx, opts, spec, cacheDir, logger)
	if err != nil {
		// The latest was probed-bad (or the download failed): walk
		// again — verdicts recorded above steer this walk to the last
		// working candidate, the last-known-good rung included.
		if bin2, _, walkErr := evaluateCandidates(ctx, cacheDir, filter, false, logger, cands); walkErr == nil {
			logger.Warn("cfbrowser: последняя версия отклонена — работаем на последней проверенной",
				"version", bin2.Version, "error", err)
			return bin2, nil
		}
		return nil, err
	}
	return bin, nil
}

// installFreeLatest is the classic free ladder rung: latest release
// carrying the platform asset, downloaded and verified — then proven
// by the bounded launch probe (PR76): a probe-bad candidate is
// recorded fresh-bad and rejected with the typed CompatError; an
// inconclusive (network-level) probe serves the verified bytes with a
// loud note and persists no verdict.
func installFreeLatest(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir string, logger *slog.Logger) (*BinaryInfo, error) {
	gh := NewGitHubClient(opts.APIBase, opts.downloadHTTPClient(10*time.Minute))
	rel, err := gh.LatestFreeRelease(ctx, spec)
	if err != nil {
		var unavailable *AssetUnavailableError
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &OfflineError{Cause: err}
	}
	bin, err := downloadAndInstall(ctx, gh, rel, spec, cacheDir, opts.downloadBase(), logger, opts.OnProgress)
	if err != nil {
		return nil, err
	}
	if out := probeAndRecord(ctx, cacheDir, bin, logger); !out.ok && !out.inconclusive {
		return nil, &CompatError{Newest: bin.Version, Reason: out.reason}
	}
	return bin, nil
}

// installPinned downloads the pinned version via the tier it
// resolves to: with a valid license the pro download API is tried
// first and a 404 falls through to the GitHub free tag; without one
// the free tag is fetched directly.
func installPinned(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir, pinned string, licenseValid bool, licRep *LicenseReport, logger *slog.Logger) (*BinaryInfo, error) {
	if licenseValid {
		info, err := installProVersion(ctx, opts, spec, cacheDir, pinned, licRep, logger)
		if err == nil {
			probePinnedLenient(ctx, cacheDir, info, logger)
			return info, nil
		}
		var offline *OfflineError
		if errors.As(err, &offline) {
			return nil, err // transport failure is not a tier signal: fail
		}
		if !isProNotFound(err) {
			// Verification or auth failures on the pro channel are
			// loud: no silent downgrade.
			return nil, err
		}
		logger.Info("cfbrowser: pinned version not on the pro channel; trying the free tag", "version", pinned)
	}
	return installPinnedFree(ctx, opts, spec, cacheDir, pinned, logger)
}

// probePinnedLenient probes a pinned install and records the verdict
// (a working pin refreshes last-known-good) but never gates serving:
// the pin is explicit user intent. A failed probe logs loud — the
// user asked for this exact binary and gets it, eyes open.
func probePinnedLenient(ctx context.Context, cacheDir string, bin *BinaryInfo, logger *slog.Logger) {
	if out := probeAndRecord(ctx, cacheDir, bin, logger); !out.ok && !out.inconclusive {
		logger.Warn("cfbrowser: пин установлен, но проверку запуска не прошёл — "+
			"версия помечена bad в хранилище вердиктов", "version", bin.Version, "reason", out.reason)
	}
}

// installPinnedFree downloads the pinned version straight from the
// GitHub free tag (the no-license tier and the channel=free rung:
// the pro API is never consulted). The pinned rung is a documented
// exemption from the launch-verdict mechanism — explicit user intent —
// and the bypass is logged loud when it fires. The probe still runs
// after install and records its verdict, but never gates the pin.
func installPinnedFree(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir, pinned string, logger *slog.Logger) (*BinaryInfo, error) {
	if _, bad := freshBadVerdict(cacheDir, pinned); bad {
		logger.Warn(pinnedBypassNote(pinned) + " — действует вердикт bad")
	} else {
		logger.Warn(pinnedBypassNote(pinned))
	}
	gh := NewGitHubClient(opts.APIBase, opts.downloadHTTPClient(10*time.Minute))
	rel, err := gh.FreeReleaseForVersion(ctx, spec, pinned)
	if err != nil {
		var unavailable *AssetUnavailableError
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &OfflineError{Cause: err}
	}
	bin, err := downloadAndInstall(ctx, gh, rel, spec, cacheDir, opts.downloadBase(), logger, opts.OnProgress)
	if err != nil {
		return nil, err
	}
	probePinnedLenient(ctx, cacheDir, bin, logger)
	return bin, nil
}

// isProNotFound reports whether err is the pro API answering 404 for
// a version it does not carry (the free-tag fallback trigger).
func isProNotFound(err error) bool {
	var notFound *proVersionNotFoundError
	return errors.As(err, &notFound)
}

// installProLatest resolves the newest pro version and installs it.
// Cache reuse follows the upstream rule: the pro-resolved version
// must match an installed PRO-marked directory (scanCacheVersionPro)
// — a free-installed dir of the same version does not qualify. The
// candidate must vouch for itself through the verdict store (a fresh
// bad verdict refuses the rung loud — re-downloading identical bytes
// cannot heal it) or, with no verdict yet, through the launch probe.
// Any failure is loud: no silent free downgrade. (channel=pro only —
// auto mode upgrades through upgradeToProBestEffort instead.)
func installProLatest(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir string, licRep *LicenseReport, logger *slog.Logger) (*BinaryInfo, error) {
	tag := spec.Tag()
	version, err := ResolveProVersion(ctx, tag, ProVersionOptions{
		CacheDir:     opts.CacheDir,
		DownloadBase: opts.DownloadBase,
		HTTPClient:   opts.downloadHTTPClient(10 * time.Minute),
	})
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: pro channel (лицензия действует, откат на free не выполняется): %w", err)
	}
	if bin, ok := scanCacheVersionPro(cacheDir, spec, version); ok {
		got, _, evalErr := evaluateCandidates(ctx, cacheDir, channelPro, false, logger, []*BinaryInfo{bin})
		if evalErr == nil && got != nil {
			logger.Info("cfbrowser: reusing cached pro stealth chromium",
				"path", got.Path, "version", got.Version, "channel", channelPro)
			return got, nil
		}
		return nil, fmt.Errorf("cfbrowser: pro %s установлен, но проверку запуска не проходит — "+
			"переустановка идентичных байт не поможет: %w", version, evalErr)
	}
	info, err := installProVersion(ctx, opts, spec, cacheDir, version, licRep, logger)
	if err != nil {
		return nil, err
	}
	if out := probeAndRecord(ctx, cacheDir, info, logger); !out.ok && !out.inconclusive {
		return nil, &CompatError{Newest: version, Reason: out.reason}
	}
	return info, nil
}

// upgradeToProBestEffort is auto mode's pro pull: resolve the newest
// pro version and reuse or download it — but only while it proves
// itself (PR76: a fresh bad verdict refuses the pull without
// downloading; an unknown candidate is downloaded and must pass the
// launch probe, which records the verdict). Any pro failure returns
// the error for the caller to log loudly while the free base serves;
// a rejected newest pro returns (nil, nil) after the loud note.
// Never a panic, never a silent downgrade: the log line and the
// caller's factual channel reporting carry the story.
func upgradeToProBestEffort(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir string, licRep *LicenseReport, logger *slog.Logger) (*BinaryInfo, error) {
	version, err := ResolveProVersion(ctx, spec.Tag(), ProVersionOptions{
		CacheDir:     opts.CacheDir,
		DownloadBase: opts.DownloadBase,
		HTTPClient:   opts.downloadHTTPClient(10 * time.Minute),
	})
	if err != nil {
		return nil, err
	}
	if e, bad := freshBadVerdict(cacheDir, version); bad {
		// The store freshly rejected this build: the pull is skipped,
		// both facts named (rejected pro; working free).
		logger.Warn(proBadVerdictNote(version, e.Reason))
		return nil, nil
	}
	if bin, ok := scanCacheVersionPro(cacheDir, spec, version); ok {
		got, _, evalErr := evaluateCandidates(ctx, cacheDir, channelPro, false, logger, []*BinaryInfo{bin})
		if evalErr == nil && got != nil {
			logger.Info("cfbrowser: reusing cached pro stealth chromium",
				"path", got.Path, "version", got.Version, "channel", channelPro)
			return got, nil
		}
		// The cached pro build does not vouch (fresh-bad or probe-bad):
		// stay on the free base, loudly.
		logger.Warn(proBadVerdictNote(version, evalErr.Error()))
		return nil, nil
	}
	info, err := installProVersion(ctx, opts, spec, cacheDir, version, licRep, logger)
	if err != nil {
		return nil, err
	}
	if out := probeAndRecord(ctx, cacheDir, info, logger); !out.ok && !out.inconclusive {
		logger.Warn(proBadVerdictNote(version, out.reason))
		return nil, nil
	}
	return info, nil
}

// proBadVerdictNote is the loud auto-mode note that the newest pro
// build carries a fresh bad verdict (probe-failed), so the free base
// stays in service. The verdict expires (badVerdictTTL) and the pull
// is retried then — kernel swaps can resurrect broken builds (PR75).
func proBadVerdictNote(version, reason string) string {
	return fmt.Sprintf("cfbrowser: pro %s заблокирован вердиктом bad (%s) — "+
		"работаем на free; перепроверка при истечении TTL вердикта", version, reason)
}

// installProVersion downloads one exact pro version. The archive
// MUST pass the pinned Ed25519 signed-manifest verification — there
// is no digest fallback on this channel — and any failure is a loud
// error, never a free downgrade.
func installProVersion(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir, version string, licRep *LicenseReport, logger *slog.Logger) (*BinaryInfo, error) {
	key := ResolveLicenseKey(opts.CacheDir)
	if key == "" {
		return nil, fmt.Errorf("cfbrowser: pro download %s: лицензионный ключ не найден", version)
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("cfbrowser: create cache dir %s: %w", cacheDir, err)
	}
	work, err := os.MkdirTemp(cacheDir, ".install-")
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: create work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	logger.Info("cfbrowser: downloading pro stealth chromium",
		"version", version, "dir", cacheDir, "plan", licRepStatusPlan(licRep))

	archivePath := filepath.Join(work, spec.Asset)
	f, err := os.Create(archivePath) //nolint:gosec // work dir + table-derived asset name
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: create archive file: %w", err)
	}
	lastPct := -5
	digest, dlErr := proDownloadArchive(ctx, proDownloadRequest{
		Version:      version,
		Key:          key,
		Tag:          spec.Tag(),
		DownloadBase: opts.DownloadBase,
		HTTPClient:   opts.downloadHTTPClient(10 * time.Minute),
	}, f, func(pct int) {
		if pct-lastPct >= 5 {
			logger.Info("cfbrowser: download progress", "pct", pct, "version", version)
			if opts.OnProgress != nil {
				opts.OnProgress(pct, version)
			}
			lastPct = pct
		}
	})
	closeErr := f.Close()
	if dlErr != nil {
		return nil, dlErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("cfbrowser: close archive file: %w", closeErr)
	}

	// The pro channel verifies against the signed manifests on the
	// DISTINCT pro release line ({base}/releases/pro/…) — no digest
	// fallback, no downgrade, no GitHub free mirror. Manifest
	// unavailability is a fetch failure (loud, retryable), distinct
	// from verification failure (BinaryVerificationError).
	verified, err := VerifyArchiveWithSignedManifests(ctx, VerifyManifestsRequest{
		DownloadBase:  opts.downloadBase(),
		Version:       version,
		Channel:       channelPro,
		ArchiveName:   spec.Asset,
		ArchiveDigest: digest,
	}, opts.downloadHTTPClient(manifestFetchTimeout))
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, fmt.Errorf("cfbrowser: pro download %s: подписанные манифесты (SHA256SUMS + SHA256SUMS.sig) "+
			"недоступны ни с одного источника — установка без проверки невозможна, откат на free не выполняется", version)
	}

	return unpackAndFinalize(work, archivePath, spec, version, cacheDir, channelPro, logger)
}

// licRepStatusPlan renders the plan for progress lines ("" safe).
func licRepStatusPlan(rep *LicenseReport) string {
	if rep == nil {
		return ""
	}
	return rep.Status.Plan
}

// downloadBase resolves the download-base override chain:
// explicit > $CLOAKBROWSER_DOWNLOAD_URL > cloakbrowser.dev.
func (o InstallOptions) downloadBase() string {
	return resolveDownloadBase(o.DownloadBase)
}

// resolveOverride stats the user-supplied binary path, failing loud
// when it does not exist.
func resolveOverride(path string) (*BinaryInfo, error) {
	fi, err := os.Stat(path) //nolint:gosec // the path IS the explicit user override
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: $%s %q: %w", EnvBinaryPath, path, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("cfbrowser: $%s %q: expected the browser executable, got a directory",
			EnvBinaryPath, path)
	}
	return &BinaryInfo{Path: path, Dir: "", Version: "override", Channel: channelUser}, nil
}

// scanCache returns the newest complete chromium-<version> directory
// for the platform (containing the expected executable), or ok=false.
// The reported channel is the directory's factual install line
// (dirChannel), not the resolving tier.
func scanCache(cacheDir string, spec PlatformSpec) (*BinaryInfo, bool) {
	for _, name := range cachedVersions(cacheDir) {
		dir := filepath.Join(cacheDir, name)
		execPath, err := locateExecutable(dir, spec.ExecName)
		if err != nil {
			continue // incomplete install: skip
		}
		version, _ := VersionFromDirName(name)
		return &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: dirChannel(dir)}, true
	}
	return nil, false
}

// scanCacheVersion resolves one exact cached version (the pinned
// rung), or ok=false. The channel is the dir's factual line.
func scanCacheVersion(cacheDir string, spec PlatformSpec, version string) (*BinaryInfo, bool) {
	dir := filepath.Join(cacheDir, VersionDirName(version))
	execPath, err := locateExecutable(dir, spec.ExecName)
	if err != nil {
		return nil, false
	}
	return &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: dirChannel(dir)}, true
}

// scanCacheVersionPro resolves one exact cached version like
// scanCacheVersion but only when the directory is pro-marked: the
// pro-resolved activation rule — the version must have been
// installed BY the pro channel, not merely exist in the cache.
func scanCacheVersionPro(cacheDir string, spec PlatformSpec, version string) (*BinaryInfo, bool) {
	bin, ok := scanCacheVersion(cacheDir, spec, version)
	if !ok || bin.Channel != channelPro {
		return nil, false
	}
	return bin, true
}

// scanProCache returns the newest complete pro-marked
// chromium-<version> directory — the offline pro line (solver
// resolution under a valid cached license).
func scanProCache(cacheDir string, spec PlatformSpec) (*BinaryInfo, bool) {
	for _, name := range cachedVersions(cacheDir) {
		dir := filepath.Join(cacheDir, name)
		if dirChannel(dir) != channelPro {
			continue
		}
		execPath, err := locateExecutable(dir, spec.ExecName)
		if err != nil {
			continue // incomplete install: skip
		}
		version, _ := VersionFromDirName(name)
		return &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: channelPro}, true
	}
	return nil, false
}

// freeLineCandidates lists the complete FREE-line (unmarked or
// free-marked) chromium-<version> directories, newest first — the
// free channel's candidate list. Pro-marked directories never qualify
// (the free-line mirror of the pro activation rule: a quiet skip by
// marker, not by verdict). Usability is NOT decided here: the verdict
// walk (evaluateCandidates) consults the store and probes.
func freeLineCandidates(cacheDir string, spec PlatformSpec) []*BinaryInfo {
	var out []*BinaryInfo
	for _, name := range cachedVersions(cacheDir) {
		dir := filepath.Join(cacheDir, name)
		if dirChannel(dir) == channelPro {
			continue // channel semantics: pro dirs never satisfy free
		}
		execPath, err := locateExecutable(dir, spec.ExecName)
		if err != nil {
			continue // incomplete install: skip
		}
		version, _ := VersionFromDirName(name)
		out = append(out, &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: channelFree})
	}
	return out
}

// proLineCandidates lists the complete PRO-marked chromium-<version>
// directories, newest first — the offline pro line's candidate list
// (solver resolution under a valid cached license; the updater's
// auto baseline through newestServingCandidate).
func proLineCandidates(cacheDir string, spec PlatformSpec) []*BinaryInfo {
	var out []*BinaryInfo
	for _, name := range cachedVersions(cacheDir) {
		dir := filepath.Join(cacheDir, name)
		if dirChannel(dir) != channelPro {
			continue
		}
		execPath, err := locateExecutable(dir, spec.ExecName)
		if err != nil {
			continue // incomplete install: skip
		}
		version, _ := VersionFromDirName(name)
		out = append(out, &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: channelPro})
	}
	return out
}

// scanCacheOrdered lists every complete chromium-<version> directory
// regardless of install line, newest first — the pre-PR73
// channel=pro degraded rung's candidate list (factual channel
// reported per directory).
func scanCacheOrdered(cacheDir string, spec PlatformSpec) []*BinaryInfo {
	var out []*BinaryInfo
	for _, name := range cachedVersions(cacheDir) {
		dir := filepath.Join(cacheDir, name)
		execPath, err := locateExecutable(dir, spec.ExecName)
		if err != nil {
			continue // incomplete install: skip
		}
		version, _ := VersionFromDirName(name)
		out = append(out, &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: dirChannel(dir)})
	}
	return out
}

// dirChannel reports the install line of a cache directory from its
// .channel marker; absent or unrecognized markers resolve as the
// free line (legacy directories predate the marker).
func dirChannel(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, channelMarkerFile)) //nolint:gosec // app-owned cache path
	if err != nil {
		return channelFree
	}
	if strings.TrimSpace(string(raw)) == channelPro {
		return channelPro
	}
	return channelFree
}

// downloadAndInstall streams the free release asset into a
// cache-local work directory, verifies it (signed manifest primary;
// the GitHub API digest field only as the documented fallback when
// no manifests exist at either origin), unpacks, relocates the
// payload into chromium-<version>/ and returns the BinaryInfo.
// Every failure cleans the work directory and leaves no partial
// chromium-<version> directory behind.
func downloadAndInstall(ctx context.Context, gh *GitHubClient, rel *FreeRelease, spec PlatformSpec, cacheDir, downloadBase string, logger *slog.Logger, onProgress func(pct int, version string)) (*BinaryInfo, error) {
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("cfbrowser: create cache dir %s: %w", cacheDir, err)
	}
	work, err := os.MkdirTemp(cacheDir, ".install-")
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: create work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	logger.Info("cfbrowser: downloading stealth chromium",
		"version", rel.Version, "asset", rel.Asset.Name,
		"bytes", rel.Asset.Size, "dir", cacheDir)

	// The local archive name must stay inside the work directory:
	// reject asset names carrying path separators (hostile/malformed
	// API data) instead of joining them blindly.
	if name := filepath.Base(rel.Asset.Name); name != rel.Asset.Name || name == "." || name == ".." {
		return nil, fmt.Errorf("cfbrowser: unsafe asset name %q", rel.Asset.Name)
	}

	// Stream to disk with progress at 5% granularity.
	archivePath := filepath.Join(work, rel.Asset.Name)
	f, err := os.Create(archivePath) //nolint:gosec // work dir + separator-validated asset name
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: create archive file: %w", err)
	}
	lastPct := -5
	digest, dlErr := gh.DownloadAsset(ctx, rel.Asset, func(pct int) {
		if pct-lastPct >= 5 {
			logger.Info("cfbrowser: download progress", "pct", pct, "version", rel.Version)
			if onProgress != nil {
				onProgress(pct, rel.Version)
			}
			lastPct = pct
		}
	}, f)
	closeErr := f.Close()
	if dlErr != nil {
		return nil, dlErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("cfbrowser: close archive file: %w", closeErr)
	}

	// Verification gate. The Ed25519-signed manifest is primary and
	// non-bypassable; only when no manifests exist at either free
	// origin does the GitHub API digest field verify the bytes. An
	// empty digest never means "skip verification".
	verified, err := VerifyArchiveWithSignedManifests(ctx, VerifyManifestsRequest{
		DownloadBase:  downloadBase,
		Version:       rel.Version,
		Channel:       channelFree,
		ArchiveName:   rel.Asset.Name,
		ArchiveDigest: digest,
	}, gh.hc)
	if err != nil {
		return nil, err
	}
	if !verified {
		if rel.Asset.Digest == "" {
			return nil, &MissingDigestError{TagName: rel.TagName, AssetName: rel.Asset.Name}
		}
		if !strings.EqualFold(digest, rel.Asset.Digest) {
			return nil, fmt.Errorf("cfbrowser: SHA-256 mismatch for %s: downloaded %s, release says %s — "+
				"archive discarded (retry, or fetch manually from %s)",
				rel.Asset.Name, digest, rel.Asset.Digest, ManualReleasesURL)
		}
	}

	return unpackAndFinalize(work, archivePath, spec, rel.Version, cacheDir, channelFree, logger)
}

// unpackAndFinalize unpacks the verified archive, relocates its
// payload root into chromium-<version>/ and returns the BinaryInfo.
func unpackAndFinalize(work, archivePath string, spec PlatformSpec, version, cacheDir, channel string, logger *slog.Logger) (*BinaryInfo, error) {
	unpacked := filepath.Join(work, "unpacked")
	if err := os.MkdirAll(unpacked, 0o750); err != nil {
		return nil, fmt.Errorf("cfbrowser: create unpack dir: %w", err)
	}
	archiveFile, err := os.Open(archivePath) //nolint:gosec // path = work dir + validated asset name
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: reopen archive: %w", err)
	}
	defer func() { _ = archiveFile.Close() }()
	if err := unpackArchive(spec.Archive, archiveFile, unpacked); err != nil {
		return nil, err
	}

	// Locate the executable and relocate its payload root into
	// chromium-<version>/.
	execPath, err := locateExecutable(unpacked, spec.ExecName)
	if err != nil {
		return nil, err
	}
	targetDir := filepath.Join(cacheDir, VersionDirName(version))
	// A same-version reinstall (or a channel switch onto an existing
	// dir — free and pro share the directory name) replaces the
	// previous payload wholesale: the freshly verified archive is
	// the authority.
	if _, err := os.Stat(targetDir); err == nil {
		if err := os.RemoveAll(targetDir); err != nil {
			return nil, fmt.Errorf("cfbrowser: replace existing %s: %w", targetDir, err)
		}
	}
	if err := relocatePayload(unpacked, execPath, targetDir); err != nil {
		return nil, err
	}
	// Record the install line inside the dir: cache scans (pro
	// activation, solver pro preference) key off this marker, and
	// writing it after relocation means an archive-supplied marker
	// file can never win.
	if err := os.WriteFile(filepath.Join(targetDir, channelMarkerFile), []byte(channel), 0o644); err != nil { //nolint:gosec // non-secret bookkeeping
		return nil, fmt.Errorf("cfbrowser: write channel marker: %w", err)
	}

	finalExec, err := locateExecutable(targetDir, spec.ExecName)
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: verify installed tree: %w", err)
	}
	logger.Info("cfbrowser: stealth chromium installed",
		"version", version, "path", finalExec, "channel", channel)
	return &BinaryInfo{Path: finalExec, Dir: targetDir, Version: version, Channel: channel}, nil
}

// relocatePayload moves the top-level directory holding execPath (or
// the loose files, for flat archives) from unpacked into targetDir.
func relocatePayload(unpacked, execPath, targetDir string) error {
	rel, err := filepath.Rel(unpacked, execPath)
	if err != nil {
		return fmt.Errorf("cfbrowser: relocate: %w", err)
	}
	if filepath.Dir(rel) == "." {
		// Flat archive: executable sits at the unpack root. Move every
		// root entry into the versioned directory.
		return moveAllEntries(unpacked, targetDir)
	}
	top := strings.SplitN(rel, string(filepath.Separator), 2)[0]
	src := filepath.Join(unpacked, top)
	// A same-named versioned root (chromium-<ver>/...) renames
	// directly; anything else nests inside the versioned target.
	if top == filepath.Base(targetDir) {
		return os.Rename(src, targetDir)
	}
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		return err
	}
	return os.Rename(src, filepath.Join(targetDir, top))
}

// moveAllEntries relocates every entry of src into dst.
func moveAllEntries(src, dst string) error {
	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return fmt.Errorf("cfbrowser: relocate %s: %w", e.Name(), err)
		}
	}
	return nil
}

// cachedVersions lists cache chromium-<version> directories newest
// first (incomplete ones included: pruning must still retire them).
func cachedVersions(cacheDir string) []string {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := VersionFromDirName(e.Name()); ok {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		vi, _ := VersionFromDirName(dirs[i])
		vj, _ := VersionFromDirName(dirs[j])
		return CompareVersions(vi, vj) > 0
	})
	return dirs
}

// pruneCacheDirs keeps the newest `keep` chromium-* directories
// (newest = current, next = rollback) and removes the rest. It is the
// updater's retirement policy and never touches non-chromium files.
func pruneCacheDirs(cacheDir string, keep int, logger *slog.Logger) {
	if keep < 1 {
		keep = 1
	}
	dirs := cachedVersions(cacheDir)
	for _, dir := range dirs[min(keep, len(dirs)):] {
		if err := os.RemoveAll(filepath.Join(cacheDir, dir)); err != nil {
			logger.Warn("cfbrowser: prune old chromium dir", "dir", dir, "error", err)
		} else {
			logger.Info("cfbrowser: pruned old chromium dir", "dir", dir)
		}
	}
}
