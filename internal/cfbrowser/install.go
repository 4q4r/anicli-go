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
	// Channel is channelUser or channelFree.
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

// Install resolves the stealth-Chromium binary for the platform,
// following the ladder: explicit user override ($CLOAKBROWSER_BINARY_PATH
// or BinaryPath) > newest complete chromium-*/ cache directory > free
// GitHub release download (SHA-256 verified, unpacked into the cache).
// Existing binaries are always reused — no re-download.
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

	// 2. Existing cache.
	cacheDir, err := ResolveCacheDir(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	if bin, ok := scanCache(cacheDir, spec); ok {
		logger.Info("cfbrowser: reusing cached stealth chromium",
			"path", bin.Path, "version", bin.Version)
		return bin, nil
	}

	// 3. Download from the free release line.
	gh := NewGitHubClient(opts.APIBase, opts.HTTPClient)
	rel, err := gh.LatestFreeRelease(ctx, spec)
	if err != nil {
		var unavailable *AssetUnavailableError
		if errors.As(err, &unavailable) {
			return nil, err
		}
		return nil, &OfflineError{Cause: err}
	}
	return downloadAndInstall(ctx, gh, rel, spec, cacheDir, logger)
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
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil, false
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
	// Newest first.
	sort.Slice(dirs, func(i, j int) bool {
		vi, _ := VersionFromDirName(dirs[i])
		vj, _ := VersionFromDirName(dirs[j])
		return CompareVersions(vi, vj) > 0
	})
	for _, name := range dirs {
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

// downloadAndInstall streams the release asset into a cache-local
// work directory, verifies the SHA-256 digest, unpacks, relocates the
// payload into chromium-<version>/ and returns the BinaryInfo. Every
// failure cleans the work directory and leaves no partial
// chromium-<version> directory behind.
func downloadAndInstall(ctx context.Context, gh *GitHubClient, rel *FreeRelease, spec PlatformSpec, cacheDir string, logger *slog.Logger) (*BinaryInfo, error) {
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

	// SHA-256 gate: mismatch deletes and fails loud.
	if rel.Asset.Digest != "" && !strings.EqualFold(digest, rel.Asset.Digest) {
		return nil, fmt.Errorf("cfbrowser: SHA-256 mismatch for %s: downloaded %s, release says %s — "+
			"archive discarded (retry, or fetch manually from %s)",
			rel.Asset.Name, digest, rel.Asset.Digest, ManualReleasesURL)
	}

	unpacked := filepath.Join(work, "unpacked")
	if err := os.MkdirAll(unpacked, 0o750); err != nil {
		return nil, fmt.Errorf("cfbrowser: create unpack dir: %w", err)
	}
	archiveFile, err := os.Open(archivePath) //nolint:gosec // path = work dir + separator-validated asset name
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
	targetDir := filepath.Join(cacheDir, VersionDirName(rel.Version))
	if err := relocatePayload(unpacked, execPath, targetDir); err != nil {
		return nil, err
	}

	finalExec, err := locateExecutable(targetDir, spec.ExecName)
	if err != nil {
		return nil, fmt.Errorf("cfbrowser: verify installed tree: %w", err)
	}
	logger.Info("cfbrowser: stealth chromium installed",
		"version", rel.Version, "path", finalExec)
	return &BinaryInfo{Path: finalExec, Dir: targetDir, Version: rel.Version, Channel: channelFree}, nil
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
