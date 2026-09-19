package cfbrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// EnvAutoUpdate toggles the auto-updater ($CLOAKBROWSER_AUTO_UPDATE,
// upstream cache contract).
const (
	EnvAutoUpdate    = "CLOAKBROWSER_AUTO_UPDATE"
	updateStatusFile = "update-status.json"

	// DefaultUpdateInterval is the periodic retry cadence.
	DefaultUpdateInterval = 30 * time.Minute
	// probeTimeout bounds the reachability HEAD request.
	probeTimeout = 3 * time.Second
	// updateCheckTimeout bounds one full CheckAndMaybeInstall cycle.
	updateCheckTimeout = 15 * time.Minute
	// closeGrace bounds how long Close waits for an in-flight periodic
	// check to wind down after its context was cancelled.
	closeGrace = 5 * time.Second
)

// UpdaterConfig parameterizes the auto-updater; zero values resolve
// to the documented defaults.
type UpdaterConfig struct {
	// Enabled gates every check (config [cf] auto_update; the
	// CLOAKBROWSER_AUTO_UPDATE env can force it off — see
	// envAutoUpdateEnabled).
	Enabled bool
	// Interval is the periodic retry ticker (default 30m).
	Interval time.Duration
	// ProbeURL is the reachability HEAD target (default the GitHub
	// API base).
	ProbeURL string
	// APIBase overrides the GitHub API base URL (tests).
	APIBase string
	// DownloadBase overrides the pro download API + manifest origin-1
	// base (tests; empty = env > cloakbrowser.dev).
	DownloadBase string
	// LicenseAPIBase overrides the license-validate API base (tests).
	LicenseAPIBase string
	// CacheDir overrides the cache directory (tests).
	CacheDir string
	// BinaryPath mirrors $CLOAKBROWSER_BINARY_PATH (an explicit user
	// binary disables self-updating: the user owns that channel).
	BinaryPath string
	// Channel selects the update line (config [cf] channel): "auto"
	// (default, "" incl.) keeps the free base current and pulls pro
	// only while a valid key sees a chromedp-compatible pro latest;
	// "free" never touches pro; "pro" keeps the pre-PR73 cycle.
	Channel string
	// Logger receives outcome lines (nil = slog.Default()).
	Logger *slog.Logger
	// HTTPClient overrides transport (tests).
	HTTPClient *http.Client
}

func (c UpdaterConfig) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultUpdateInterval
	}
	return c.Interval
}

func (c UpdaterConfig) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// UpdateStatus is the persisted outcome of the last check
// (cache/update-status.json) and the surface Status() reports.
type UpdateStatus struct {
	// CheckedAt is the last completed check.
	CheckedAt time.Time `json:"checked_at"`
	// LatestVersion is the newest free-release version seen.
	LatestVersion string `json:"latest_version"`
	// InstalledVersion is the tier line's installed version at check
	// time (pro checks: the newest pro-marked dir; free checks: the
	// cache's newest complete dir; "" when the line has none).
	InstalledVersion string `json:"installed_version"`
	// Deferred marks an update that is waiting for connectivity.
	Deferred bool `json:"deferred"`
	// UpdatedTo is the version the last successful install landed.
	UpdatedTo string `json:"updated_to,omitempty"`
	// LastError carries the last failure (deferred causes).
	LastError string `json:"last_error,omitempty"`
}

// Updater keeps the cached stealth Chromium current: it compares the
// installed version against the latest free GitHub release and runs
// the full install flow when a newer one exists — never on the solve
// path's critical section, always single-flighted, offline failures
// only mark the update deferred until one of the retry triggers
// fires (next check, periodic ticker, or the pre-solve kick).
type Updater struct {
	cfg UpdaterConfig
	gh  *GitHubClient

	mu     sync.Mutex
	status UpdateStatus
	// probeURL is read-mostly; SetProbeURL swaps it (tests, and
	// nothing else — the production value never changes).
	probeMu  sync.RWMutex
	probeURL string

	flight singleflight.Group

	startOnce   sync.Once
	loopStarted atomic.Bool
	stopOnce    sync.Once
	stop        chan struct{}
	done        chan struct{}
}

// NewUpdater builds an idle updater; Start launches the periodic
// ticker, Close stops it. Construction never touches the network.
func NewUpdater(cfg UpdaterConfig) *Updater {
	if cfg.ProbeURL == "" {
		cfg.ProbeURL = githubAPIBase
	}
	if cfg.APIBase == "" {
		cfg.APIBase = githubAPIBase
	}
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = os.Getenv(EnvBinaryPath)
	}
	return &Updater{
		cfg:      cfg,
		gh:       NewGitHubClient(cfg.APIBase, cfg.HTTPClient),
		probeURL: cfg.ProbeURL,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start launches the periodic retry ticker. Idempotent; safe to call
// on a disabled updater (the loop exits immediately). Start after
// Close is a no-op — Close is terminal.
func (u *Updater) Start() {
	u.startOnce.Do(func() {
		u.loopStarted.Store(true)
		go u.loop()
	})
}

// Close stops the ticker, cancels an in-flight periodic check and
// waits a bounded grace (5s) for it to wind down; a check overrunning
// the grace keeps running against its cancelled context in the
// background and still lands its outcome in the status file. Close
// before Start is legal.
func (u *Updater) Close() {
	u.stopOnce.Do(func() {
		// Synchronize with a concurrent Start: the startOnce noop
		// either observes the launched loop or claims Start forever.
		u.startOnce.Do(func() {})
		close(u.stop)
		if !u.loopStarted.Load() {
			close(u.done)
			return
		}
		select {
		case <-u.done:
		case <-time.After(closeGrace):
			// Bounded join: abandon the wind-down rather than stall
			// the caller — the check's context is already cancelled.
		}
	})
}

// loop ticks until stopped (defer close(done) on every exit path).
// Tick checks run detached from the loop goroutine with a context
// derived from the loop lifetime: stopping cancels an in-flight check
// (bounded by closeGrace on both sides) instead of letting it pin
// Close open for its full 15m budget.
func (u *Updater) loop() {
	defer close(u.done)
	if !u.cfg.Enabled {
		return
	}
	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	ticker := time.NewTicker(u.cfg.interval())
	defer ticker.Stop()

	// checkDone is non-nil while a tick check runs; the nil-channel
	// select case below is simply ignored while idle.
	var checkDone chan struct{}
	for {
		select {
		case <-u.stop:
			lcancel() // abort an in-flight tick check
			if checkDone != nil {
				select {
				case <-checkDone:
				case <-time.After(closeGrace):
					// Abandoned but bounded: the check keeps winding
					// down against its cancelled context.
				}
			}
			return
		case <-checkDone:
			checkDone = nil
		case <-ticker.C:
			if checkDone != nil {
				// Previous tick still in flight: skip — the next tick
				// retries (singleflight would fold us into the running
				// check anyway).
				continue
			}
			done := make(chan struct{})
			checkDone = done
			go func() {
				defer close(done)
				ctx, cancel := context.WithTimeout(lctx, updateCheckTimeout)
				defer cancel()
				_ = u.CheckAndMaybeInstall(ctx)
			}()
		}
	}
}

// SetProbeURL swaps the reachability probe target (test seam for the
// deferred-then-online path).
func (u *Updater) SetProbeURL(url string) {
	u.probeMu.Lock()
	u.probeURL = url
	u.probeMu.Unlock()
}

// PreSolveKick is the pre-solve retry trigger: fire-and-forget. When
// an update is deferred and the network is back, this starts the
// check on a background context; it never blocks or delays the solve
// (if the probe or download is slow, the solve simply proceeds on the
// current binary while the update continues in the background).
func (u *Updater) PreSolveKick() {
	if !u.cfg.Enabled {
		return
	}
	u.mu.Lock()
	deferred := u.status.Deferred
	u.mu.Unlock()
	if !deferred {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		defer cancel()
		if err := u.CheckAndMaybeInstall(ctx); err != nil {
			u.cfg.logger().Warn("cfbrowser: deferred update retry failed", "error", err)
		}
	}()
}

// CheckAndMaybeInstall runs one network-gated update cycle:
// reachability probe first (offline → mark deferred, no download),
// then release comparison, then the full verified install flow. It is
// collapsed by a singleflight group, so concurrent triggers (ticker +
// pre-solve kick + explicit call) perform at most one API walk.
// Deferred states are NOT errors: the caller only sees real failures
// (and even those never touch the solve path — this method is always
// off it).
func (u *Updater) CheckAndMaybeInstall(ctx context.Context) error {
	if !u.cfg.Enabled {
		return nil
	}
	// An explicit user binary owns its own channel: never update.
	if u.cfg.BinaryPath != "" {
		return nil
	}
	// A pinned $CLOAKBROWSER_VERSION owns the channel exactly the
	// same way: the updater must not move off the user's pin.
	if os.Getenv(EnvVersion) != "" {
		return nil
	}
	_, err, _ := u.flight.Do("check", func() (any, error) {
		return nil, u.check(ctx)
	})
	return err
}

// check performs the gated cycle; the singleflight wrapper already
// serialized concurrent callers. The channel (config [cf] channel)
// selects the cycle:
//
//   - pro: the pre-PR73 cycle verbatim — a valid license updates
//     within the pro channel (failures defer, never downgrade to
//     free); no license keeps the classic free flow.
//   - free: the bound-checked free flow only — the license API and
//     the pro channel are never consulted.
//   - auto (default): the free-base cycle — the filtered free line's
//     bookkeeping (pro-marked dirs never satisfy it, the chromedp
//     compat bound blocks incompatible pulls), plus the pro upgrade
//     attempt while a valid license resolves to a compatible pro
//     latest. While the pro line serves, the free base's check is
//     done for the cycle; when the pro pull is skipped or fails, a
//     loud note is logged and the free flow keeps the base current.
func (u *Updater) check(ctx context.Context) error {
	logger := u.cfg.logger()
	cacheDir, err := ResolveCacheDir(u.cfg.CacheDir)
	if err != nil {
		return err
	}
	spec, err := CurrentPlatform()
	if err != nil {
		return err
	}
	channel, err := normalizeChannel(u.cfg.Channel)
	if err != nil {
		return err
	}

	// Network gate: 3s HEAD probe. Offline → defer.
	u.probeMu.RLock()
	probeURL := u.probeURL
	u.probeMu.RUnlock()
	if !reachable(ctx, probeURL, u.cfg.HTTPClient) {
		u.record(cacheDir, UpdateStatus{
			InstalledVersion: u.baselineVersion(cacheDir, spec, channel), Deferred: true,
			LastError: "probe: network unreachable",
		})
		logger.Info("cfbrowser: auto-update deferred (offline)")
		return nil
	}

	if channel == channelPro {
		return u.checkProChannel(ctx, cacheDir, spec, logger)
	}
	return u.checkFreeBase(ctx, cacheDir, spec, channel, logger)
}

// baselineVersion reports the working line's installed version for
// the status bookkeeping: pro — the newest cache dir (pre-PR73
// posture); free/auto — the newest free-line dir (pro-marked dirs
// never satisfy the free line).
func (u *Updater) baselineVersion(cacheDir string, spec PlatformSpec, channel string) string {
	if channel == channelPro {
		bin, _ := scanCache(cacheDir, spec)
		if bin != nil {
			return bin.Version
		}
		return ""
	}
	bin, _, _ := scanCacheFree(cacheDir, spec)
	if bin != nil {
		return bin.Version
	}
	return ""
}

// checkFreeBase is the free/auto update cycle. The free line's
// comparison baseline is the filtered free scan (a pro-marked dir
// must never gate a free update), the compat bound gates both the
// cached-major verdict and the latest release, and — under auto — a
// valid license offers the cycle to the pro upgrade first.
func (u *Updater) checkFreeBase(ctx context.Context, cacheDir string, spec PlatformSpec, channel string, logger *slog.Logger) error {
	freeBin, _, compatErr := scanCacheFree(cacheDir, spec)
	installedVersion := ""
	if freeBin != nil {
		installedVersion = freeBin.Version
	}
	if compatErr != nil {
		// The whole free line is unusable for the pinned chromedp:
		// record and name the fix. A retry cannot move a bound.
		u.record(cacheDir, UpdateStatus{
			InstalledVersion: installedVersion, LastError: compatErr.Error(),
		})
		logger.Warn("cfbrowser: auto-update blocked by the chromedp compatibility bound", "error", compatErr)
		return compatErr
	}

	if channel == channelAuto {
		licRep, licErr := CheckLicense(ctx, LicenseOptions{
			CacheDir:   u.cfg.CacheDir,
			APIBase:    u.cfg.LicenseAPIBase,
			HTTPClient: u.cfg.HTTPClient,
		})
		if licErr != nil {
			// License unprovable after the probe said "online": a
			// configured key defers (the tier is unknown, not free);
			// a key-less setup keeps the free channel going.
			if ResolveLicenseKey(cacheDir) != "" {
				u.record(cacheDir, UpdateStatus{
					InstalledVersion: installedVersion, Deferred: true,
					LastError: licErr.Error(),
				})
				return nil
			}
			licRep = nil
		}
		if licRep != nil && licRep.Status.Valid {
			if u.tryProUpgradeCycle(ctx, cacheDir, spec, licRep, logger) {
				return nil // the pro line serves; the base is covered
			}
			// Pro pull skipped or failed: the notes are loud; keep
			// the free base current below.
		}
	}

	return u.freeFlow(ctx, cacheDir, spec, installedVersion, logger)
}

// freeFlow is the shared free-release tail of both update cycles:
// latest release → chromedp compat bound → compare against the
// caller's baseline → forced verified install. The bound applies on
// every channel — it guards the driver, not a tier.
func (u *Updater) freeFlow(ctx context.Context, cacheDir string, spec PlatformSpec, installedVersion string, logger *slog.Logger) error {
	rel, err := u.gh.LatestFreeRelease(ctx, spec)
	if err != nil {
		// Asset gaps (darwin on some tags) are terminal for this
		// platform, not transient: record, do not re-defer forever.
		u.record(cacheDir, UpdateStatus{
			LatestVersion: installedVersion, InstalledVersion: installedVersion,
			LastError: err.Error(),
		})
		return err
	}
	if err := checkChromiumCompat(rel.Version); err != nil {
		u.record(cacheDir, UpdateStatus{
			LatestVersion: rel.Version, InstalledVersion: installedVersion,
			LastError: err.Error(),
		})
		logger.Warn("cfbrowser: auto-update blocked by the chromedp compatibility bound", "error", err)
		return err
	}

	if CompareVersions(rel.Version, installedVersion) <= 0 {
		u.record(cacheDir, UpdateStatus{
			LatestVersion: rel.Version, InstalledVersion: installedVersion,
		})
		return nil
	}

	// Newer release: forced install of exactly this release (the
	// generic Install ladder would reuse the cached older binary).
	info, err := downloadAndInstall(ctx, u.gh, rel, spec, cacheDir, resolveDownloadBase(u.cfg.DownloadBase), logger)
	if err != nil {
		u.record(cacheDir, UpdateStatus{
			LatestVersion: rel.Version, InstalledVersion: installedVersion,
			Deferred: true, LastError: err.Error(),
		})
		return fmt.Errorf("cfbrowser: auto-update install %s: %w", rel.Version, err)
	}
	pruneCacheDirs(cacheDir, 2) // newest + rollback
	u.record(cacheDir, UpdateStatus{
		LatestVersion: rel.Version, InstalledVersion: info.Version,
		UpdatedTo: info.Version,
	})
	logger.Info("cfbrowser: stealth chromium updated",
		"from", installedVersion, "to", info.Version, "path", info.Path, "channel", channelFree)
	return nil
}

// tryProUpgradeCycle is auto mode's pro upgrade: with the license
// already validated, resolve the newest pro version and update the
// pro line — but only while that version stays within the chromedp
// compatibility bound. Reports whether the pro line is confirmed
// serving (updated now or already current); false hands the cycle
// back to the free base, with the reason logged loud.
func (u *Updater) tryProUpgradeCycle(ctx context.Context, cacheDir string, spec PlatformSpec, licRep *LicenseReport, logger *slog.Logger) bool {
	version, err := ResolveProVersion(ctx, spec.Tag(), ProVersionOptions{
		CacheDir:     u.cfg.CacheDir,
		DownloadBase: u.cfg.DownloadBase,
		HTTPClient:   u.cfg.HTTPClient,
	})
	if err != nil {
		logger.Warn("cfbrowser: pro upgrade skipped — работаем на free", "error", err)
		return false
	}
	if !chromiumMajorKnownGood(version) {
		logger.Warn(proIncompatNote(version))
		return false
	}
	if err := u.checkPro(ctx, cacheDir, spec, licRep, logger, channelAuto); err != nil {
		logger.Warn("cfbrowser: pro update failed — работаем на free", "error", err)
		return false
	}
	return true
}

// checkProChannel is the pre-PR73 updater cycle, regression-pinned
// for channel=pro: license-gated pro updates with the classic free
// flow as the no-valid-license fallback.
func (u *Updater) checkProChannel(ctx context.Context, cacheDir string, spec PlatformSpec, logger *slog.Logger) error {
	installed, _ := scanCache(cacheDir, spec)
	installedVersion := ""
	if installed != nil {
		installedVersion = installed.Version
	}

	licRep, licErr := CheckLicense(ctx, LicenseOptions{
		CacheDir:   u.cfg.CacheDir,
		APIBase:    u.cfg.LicenseAPIBase,
		HTTPClient: u.cfg.HTTPClient,
	})
	if licErr != nil {
		// License unprovable after the probe said "online": treat as
		// a transient failure and defer (no free downgrade while a
		// key is configured — the user's tier is unknown, not free).
		if ResolveLicenseKey(cacheDir) != "" {
			u.record(cacheDir, UpdateStatus{
				InstalledVersion: installedVersion, Deferred: true,
				LastError: licErr.Error(),
			})
			return nil
		}
		licRep = nil // key-less: the free channel proceeds
	}
	if licRep != nil && licRep.Status.Valid {
		return u.checkPro(ctx, cacheDir, spec, licRep, logger, channelPro)
	}

	// Free channel: the shared free flow (the compat bound rides it —
	// it guards the driver, not a tier).
	return u.freeFlow(ctx, cacheDir, spec, installedVersion, logger)
}

// checkPro runs the pro-channel update cycle: marker-gated latest
// version, comparison against the PRO-installed line, verified pro
// install. The comparison baseline is the newest pro-marked cache
// directory only — a free-installed dir (same or newer version)
// must never satisfy a pro update check; with no pro dir installed
// the resolved version always downloads. Under auto the baseline is
// compat-filtered (an incompatible pro dir never gates a compatible
// update). Failures defer (the network-gated retry semantics are
// identical to the free channel); a free download is NEVER
// substituted.
func (u *Updater) checkPro(ctx context.Context, cacheDir string, spec PlatformSpec, licRep *LicenseReport, logger *slog.Logger, channel string) error {
	// The pro line's baseline: pro-marked dirs only ("" when none).
	scanBaseline := scanProCache
	if channel == channelAuto {
		scanBaseline = scanProCacheCompat
	}
	proInstalled, _ := scanBaseline(cacheDir, spec)
	installedVersion := ""
	if proInstalled != nil {
		installedVersion = proInstalled.Version
	}

	version, err := ResolveProVersion(ctx, spec.Tag(), ProVersionOptions{
		CacheDir:     u.cfg.CacheDir,
		DownloadBase: u.cfg.DownloadBase,
		HTTPClient:   u.cfg.HTTPClient,
	})
	if err != nil {
		// Mirrors the free channel's listing-failure posture: record
		// without re-defer (the next cycle retries anyway).
		u.record(cacheDir, UpdateStatus{
			LatestVersion: installedVersion, InstalledVersion: installedVersion,
			LastError: err.Error(),
		})
		return err
	}
	if proInstalled != nil && CompareVersions(version, installedVersion) <= 0 {
		u.record(cacheDir, UpdateStatus{
			LatestVersion: version, InstalledVersion: installedVersion,
		})
		return nil
	}

	info, err := installProVersion(ctx, InstallOptions{
		CacheDir:       u.cfg.CacheDir,
		DownloadBase:   u.cfg.DownloadBase,
		LicenseAPIBase: u.cfg.LicenseAPIBase,
		Platform:       spec,
		Logger:         logger,
		HTTPClient:     u.cfg.HTTPClient,
	}, spec, cacheDir, version, licRep, logger)
	if err != nil {
		u.record(cacheDir, UpdateStatus{
			LatestVersion: version, InstalledVersion: installedVersion,
			Deferred: true, LastError: err.Error(),
		})
		return fmt.Errorf("cfbrowser: auto-update install %s (pro): %w", version, err)
	}
	pruneCacheDirs(cacheDir, 2) // newest + rollback
	u.record(cacheDir, UpdateStatus{
		LatestVersion: version, InstalledVersion: info.Version,
		UpdatedTo: info.Version,
	})
	logger.Info("cfbrowser: stealth chromium updated",
		"from", installedVersion, "to", info.Version, "path", info.Path, "channel", channelPro)
	return nil
}

// record folds a status snapshot under the updater lock and persists
// it to cache/update-status.json (best effort: persistence failures
// are logged, never fail the check).
func (u *Updater) record(cacheDir string, st UpdateStatus) {
	st.CheckedAt = time.Now()
	u.mu.Lock()
	u.status = st
	u.mu.Unlock()
	path := filepath.Join(cacheDir, updateStatusFile)
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil { //nolint:gosec // non-secret bookkeeping
		_ = os.Rename(tmp, path)
	} else {
		u.cfg.logger().Warn("cfbrowser: persist update status", "error", err)
	}
}

// Status returns the last recorded cycle outcome.
func (u *Updater) Status() UpdateStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.status
}

// reachable probes url with a bounded HEAD request; any HTTP response
// (even a 4xx/5xx) proves reachability.
func reachable(ctx context.Context, url string, hc *http.Client) bool {
	if hc == nil {
		hc = &http.Client{}
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// envAutoUpdateEnabled layers $CLOAKBROWSER_AUTO_UPDATE over the
// config value: an explicitly falsy env ("false", "0", "no", "off")
// disables; anything else keeps the config decision. An unset/empty
// env keeps config as-is.
func envAutoUpdateEnabled(configEnabled bool) bool {
	v := os.Getenv(EnvAutoUpdate)
	if v == "" {
		return configEnabled
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	switch v {
	case "yes", "on", "enable", "enabled":
		return true
	case "no", "off", "disable", "disabled":
		return false
	}
	return configEnabled
}

// AutoUpdateFromConfig resolves the effective auto-update switch for
// a config [cf].auto_update value (env override honored).
func AutoUpdateFromConfig(configEnabled bool) bool {
	return envAutoUpdateEnabled(configEnabled)
}
