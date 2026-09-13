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
)

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
	// Logger receives progress lines (nil = slog.Default()).
	Logger *slog.Logger
	// HTTPClient overrides the download/API client (nil = default).
	HTTPClient *http.Client
}

// logger resolves the effective slog logger.
func (o InstallOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
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
	return LicenseOptions{CacheDir: o.CacheDir, APIBase: o.LicenseAPIBase, HTTPClient: o.HTTPClient}
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

// Install resolves the stealth-Chromium binary for the platform
// following the upstream ensureBinary precedence:
//
//  1. explicit user override ($CLOAKBROWSER_BINARY_PATH or BinaryPath);
//  2. pinned version ($CLOAKBROWSER_VERSION): installed → use, else
//     download via the tier the version resolves to (pro first with
//     a valid license, the GitHub free tag otherwise);
//  3. pro tier (valid license): newest cached binary reported as pro;
//     none cached → pro latest download (Ed25519-verified). Pro
//     failures are LOUD — never a silent free downgrade;
//  4. free tier: newest cached binary > latest free GitHub release.
//
// Every downloaded archive — either channel — passes the pinned
// Ed25519 signed-manifest verification (verify.go); the free channel
// alone keeps the GitHub API digest field as a documented fallback
// for releases whose manifests are absent from both origins.
func Install(ctx context.Context, opts InstallOptions) (*BinaryInfo, error) {
	spec, err := opts.platform()
	if err != nil {
		return nil, err
	}
	logger := opts.logger()

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

	// License state gates the tiers. Unprovable (offline, no cache)
	// fails open to the free tier — public and signed — while a
	// provably-valid license routes every download through pro.
	licRep, licErr := CheckLicense(ctx, opts.licenseOptions())
	if licErr != nil {
		logger.Warn("cfbrowser: license check failed; resolving as free tier", "error", licErr)
	}
	licenseValid := licRep != nil && licRep.Status.Valid
	channel := tierChannel(licenseValid)
	if licenseValid {
		logger.Info("cfbrowser: license valid — pro channel", "plan", licRep.Status.Plan, "expires", licRep.Status.Expires)
	}

	// 2. Pinned version.
	if pinned := opts.pinnedVersion(); pinned != "" {
		if err := validateVersion(pinned); err != nil {
			return nil, err
		}
		if bin, ok := scanCacheVersion(cacheDir, spec, pinned); ok {
			bin.Channel = channel
			logger.Info("cfbrowser: reusing pinned stealth chromium", "path", bin.Path, "version", bin.Version)
			return bin, nil
		}
		return installPinned(ctx, opts, spec, cacheDir, pinned, licenseValid, licRep, logger)
	}

	// 3./4. Cached binary reuse (tier reported from the license).
	if bin, ok := scanCache(cacheDir, spec); ok {
		bin.Channel = channel
		logger.Info("cfbrowser: reusing cached stealth chromium",
			"path", bin.Path, "version", bin.Version, "channel", channel)
		return bin, nil
	}

	if licenseValid {
		// Pro: never a silent free downgrade on any failure.
		return installProLatest(ctx, opts, spec, cacheDir, licRep, logger)
	}
	return installFreeLatest(ctx, opts, spec, cacheDir, logger)
}

// tierChannel renders the resolution channel for a license state.
func tierChannel(licenseValid bool) string {
	if licenseValid {
		return channelPro
	}
	return channelFree
}

// installFreeLatest is the classic free ladder rung: latest release
// carrying the platform asset, downloaded and verified.
func installFreeLatest(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir string, logger *slog.Logger) (*BinaryInfo, error) {
	gh := NewGitHubClient(opts.APIBase, opts.HTTPClient)
	rel, err := gh.LatestFreeRelease(ctx, spec)
	if err != nil {
		var unavailable *AssetUnavailableError
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &OfflineError{Cause: err}
	}
	return downloadAndInstall(ctx, gh, rel, spec, cacheDir, opts.downloadBase(), logger)
}

// installPinned downloads the pinned version via the tier it
// resolves to: with a valid license the pro download API is tried
// first and a 404 falls through to the GitHub free tag; without one
// the free tag is fetched directly.
func installPinned(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir, pinned string, licenseValid bool, licRep *LicenseReport, logger *slog.Logger) (*BinaryInfo, error) {
	if licenseValid {
		info, err := installProVersion(ctx, opts, spec, cacheDir, pinned, licRep, logger)
		if err == nil {
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
	gh := NewGitHubClient(opts.APIBase, opts.HTTPClient)
	rel, err := gh.FreeReleaseForVersion(ctx, spec, pinned)
	if err != nil {
		var unavailable *AssetUnavailableError
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &OfflineError{Cause: err}
	}
	return downloadAndInstall(ctx, gh, rel, spec, cacheDir, opts.downloadBase(), logger)
}

// isProNotFound reports whether err is the pro API answering 404 for
// a version it does not carry (the free-tag fallback trigger).
func isProNotFound(err error) bool {
	var notFound *proVersionNotFoundError
	return errors.As(err, &notFound)
}

// installProLatest resolves the newest pro version and installs it.
func installProLatest(ctx context.Context, opts InstallOptions, spec PlatformSpec, cacheDir string, licRep *LicenseReport, logger *slog.Logger) (*BinaryInfo, error) {
	tag := spec.Tag()
	version, err := ResolveProVersion(ctx, tag, ProVersionOptions{
		CacheDir:     opts.CacheDir,
		DownloadBase: opts.DownloadBase,
		HTTPClient:   opts.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: pro channel (лицензия действует, откат на free не выполняется): %w", err)
	}
	return installProVersion(ctx, opts, spec, cacheDir, version, licRep, logger)
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
		HTTPClient:   opts.HTTPClient,
	}, f, func(pct int) {
		if pct-lastPct >= 5 {
			logger.Info("cfbrowser: download progress", "pct", pct, "version", version)
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

	// The pro channel verifies against the signed manifests — no
	// digest fallback, no downgrade. Manifest unavailability is a
	// fetch failure (loud, retryable), distinct from verification
	// failure (BinaryVerificationError).
	verified, err := VerifyArchiveWithSignedManifests(ctx, VerifyManifestsRequest{
		DownloadBase:  opts.downloadBase(),
		Version:       version,
		ArchiveName:   spec.Asset,
		ArchiveDigest: digest,
	}, opts.HTTPClient)
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
func scanCache(cacheDir string, spec PlatformSpec) (*BinaryInfo, bool) {
	for _, name := range cachedVersions(cacheDir) {
		dir := filepath.Join(cacheDir, name)
		execPath, err := locateExecutable(dir, spec.ExecName)
		if err != nil {
			continue // incomplete install: skip
		}
		version, _ := VersionFromDirName(name)
		return &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: channelFree}, true
	}
	return nil, false
}

// scanCacheVersion resolves one exact cached version (the pinned
// rung), or ok=false.
func scanCacheVersion(cacheDir string, spec PlatformSpec, version string) (*BinaryInfo, bool) {
	dir := filepath.Join(cacheDir, VersionDirName(version))
	execPath, err := locateExecutable(dir, spec.ExecName)
	if err != nil {
		return nil, false
	}
	return &BinaryInfo{Path: execPath, Dir: dir, Version: version, Channel: channelFree}, true
}

// downloadAndInstall streams the free release asset into a
// cache-local work directory, verifies it (signed manifest primary;
// the GitHub API digest field only as the documented fallback when
// no manifests exist at either origin), unpacks, relocates the
// payload into chromium-<version>/ and returns the BinaryInfo.
// Every failure cleans the work directory and leaves no partial
// chromium-<version> directory behind.
func downloadAndInstall(ctx context.Context, gh *GitHubClient, rel *FreeRelease, spec PlatformSpec, cacheDir, downloadBase string, logger *slog.Logger) (*BinaryInfo, error) {
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
	// non-bypassable; only when no manifests exist at either origin
	// does the GitHub API digest field verify the bytes. An empty
	// digest never means "skip verification".
	verified, err := VerifyArchiveWithSignedManifests(ctx, VerifyManifestsRequest{
		DownloadBase:  downloadBase,
		Version:       rel.Version,
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
	if err := relocatePayload(unpacked, execPath, targetDir); err != nil {
		return nil, err
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
func pruneCacheDirs(cacheDir string, keep int) {
	if keep < 1 {
		keep = 1
	}
	dirs := cachedVersions(cacheDir)
	for _, dir := range dirs[min(keep, len(dirs)):] {
		if err := os.RemoveAll(filepath.Join(cacheDir, dir)); err != nil {
			slog.Warn("cfbrowser: prune old chromium dir", "dir", dir, "error", err)
		} else {
			slog.Info("cfbrowser: pruned old chromium dir", "dir", dir)
		}
	}
}
