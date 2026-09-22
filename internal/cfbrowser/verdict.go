package cfbrowser

// PR76 — the adaptive chromium version mechanism («щадящий»): try any
// version; when it fails, force the last one that worked. The static
// maxKnownGoodChromiumMajor bound of PR73 is retired wholesale: PR75
// proved the major→works mapping is kernel/state-dependent in BOTH
// directions (151 died on one kernel and passed on the next), so a
// hardcoded ceiling wrongly refuses healed majors and wrongly admits
// broken ones. The replacement classifier is a per-version VERDICT
// from a real probe launch, persisted in
// <cacheDir>/compat-verdicts.json and keyed by the chromedp module
// version (the module, not the upstream release, decides what the
// driver can control — a module bump invalidates every verdict).
//
// Verdicts:
//   - "good": the probe launched the binary, navigated a real page
//     and read its title. Persists until the chromedp module version
//     changes. Also refreshes the last-known-good record on every
//     success.
//   - "bad": the probe failed, with a typed reason. Carries a
//     re-probe-after timestamp (default +7d): kernel swaps and other
//     environment heals resurrect broken majors, so bad verdicts
//     expire and the next resolution re-probes.
//
// The probe is classified honest: a launch failure or a crashed
// target is a BAD verdict; a navigation-level network error (the
// browser started and rendered an error page) is INCONCLUSIVE —
// offline must never poison the store.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

// verdictStoreFile is the verdict store inside the cache directory
// (default ~/.cloakbrowser/compat-verdicts.json).
const verdictStoreFile = "compat-verdicts.json"

const (
	// badVerdictTTL is how long a bad verdict binds before the next
	// resolution re-probes: the environment can heal (PR75's kernel
	// swap resurrected 151).
	badVerdictTTL = 7 * 24 * time.Hour
	// goodVerdictTTL is how long a good verdict binds before the next
	// resolution re-probes it. A good verdict is earned by TWO
	// consecutive probe passes (one lucky launch must never lock a
	// flaky major in), and even then it decays after a day: a major
	// that turned flaky must surface again instead of being trusted
	// forever (PR76 review blocker).
	goodVerdictTTL = 24 * time.Hour
	// probeBudget bounds one probe (launch + navigate + title).
	probeBudget = 20 * time.Second
	// probeTargetURL is the probe navigation target: a minimal real
	// page over the network stack — the exact shape of the PR75
	// failure (launch OK, first real navigation dead).
	probeTargetURL = "https://example.com"
)

// verdictEntry is one chromium major's classification.
type verdictEntry struct {
	// Verdict is "good" or "bad".
	Verdict string `json:"verdict"`
	// CheckedAt is when the probe decided.
	CheckedAt time.Time `json:"checked_at"`
	// ReProbeAfter is when the verdict stops binding: bad after
	// badVerdictTTL, good after goodVerdictTTL. Nil/absent (legacy
	// entries) reads as expired — the entry self-heals on re-probe.
	ReProbeAfter *time.Time `json:"re_probe_after,omitempty"`
	// Reason is the typed probe failure (bad only).
	Reason string `json:"reason,omitempty"`
}

// lastKnownGood records the newest binary a probe-navigation
// actually verified working.
type lastKnownGood struct {
	Version   string    `json:"version"`
	Channel   string    `json:"channel"`
	Path      string    `json:"path"`
	Major     int       `json:"major"`
	Chromedp  string    `json:"chromedp"`
	CheckedAt time.Time `json:"checked_at"`
}

// verdictBucket holds one chromedp module version's state.
type verdictBucket struct {
	Verdicts      map[string]verdictEntry `json:"verdicts"`
	LastKnownGood *lastKnownGood          `json:"last_known_good,omitempty"`
}

// verdictStore is the in-memory verdict store; loadVerdictStore reads
// it, save persists it atomically.
type verdictStore struct {
	path     string
	chromedp string
	buckets  map[string]*verdictBucket
	// logger receives the persist diagnostics (PR85: nil = discard —
	// never slog.Default inside the TUI).
	logger *slog.Logger
}

// chromedpModuleVersion resolves the chromedp module version from the
// build info — the verdict store's bucket key. A var so tests can pin
// it (the cfbrowser suite runs sequentially — no t.Parallel).
var chromedpModuleVersion = detectChromedpModuleVersion

// detectChromedpModuleVersion reads the linked chromedp module
// version; "unknown" when the build info carries no deps (stripped
// builds) — a consistent bucket of its own.
func detectChromedpModuleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range bi.Deps {
		if dep != nil && dep.Path == "github.com/chromedp/chromedp" {
			return dep.Version
		}
	}
	return "unknown"
}

// loadVerdictStore reads the store for the current chromedp module
// version. Absent or corrupt files read as an empty store — a lost
// verdict only costs one probe, never an error path.
func loadVerdictStore(cacheDir string) *verdictStore {
	s := &verdictStore{
		path:     filepath.Join(cacheDir, verdictStoreFile),
		chromedp: chromedpModuleVersion(),
		buckets:  map[string]*verdictBucket{},
		logger:   discardLogger(),
	}
	raw, err := os.ReadFile(s.path) //nolint:gosec // app-owned cache path
	if err != nil {
		return s
	}
	var all map[string]*verdictBucket
	if json.Unmarshal(raw, &all) != nil {
		return s
	}
	s.buckets = all
	if s.buckets == nil {
		s.buckets = map[string]*verdictBucket{}
	}
	return s
}

// bucket returns the current chromedp version's bucket, creating it
// on demand (only the current bucket is ever consulted or written).
func (s *verdictStore) bucket() *verdictBucket {
	b, ok := s.buckets[s.chromedp]
	if !ok || b == nil {
		b = &verdictBucket{Verdicts: map[string]verdictEntry{}}
		s.buckets[s.chromedp] = b
	}
	if b.Verdicts == nil {
		b.Verdicts = map[string]verdictEntry{}
	}
	return b
}

// verdictFor reports the verdict for a major, honoring the verdict
// TTL: a verdict whose re-probe horizon is absent or past reads as no
// verdict so a re-probe decides again (bad decays after 7d, good
// after 24h; legacy entries without a horizon read as expired and
// self-heal).
func (s *verdictStore) verdictFor(major int) (verdictEntry, bool) {
	e, ok := s.bucket().Verdicts[fmt.Sprint(major)]
	if !ok {
		return verdictEntry{}, false
	}
	if e.ReProbeAfter == nil || !time.Now().Before(*e.ReProbeAfter) {
		return verdictEntry{}, false
	}
	return e, true
}

// recordGood persists a good verdict (with its ~goodVerdictTTL
// re-probe horizon) and refreshes last-known-good.
func (s *verdictStore) recordGood(major int, lkg lastKnownGood) {
	horizon := time.Now().Add(goodVerdictTTL)
	s.bucket().Verdicts[fmt.Sprint(major)] = verdictEntry{
		Verdict: "good", CheckedAt: time.Now(), ReProbeAfter: &horizon,
	}
	lkg.Chromedp = s.chromedp
	lkg.Major = major
	lkg.CheckedAt = time.Now()
	s.bucket().LastKnownGood = &lkg
	s.save()
}

// recordBad persists a bad verdict with its typed reason and the
// default re-probe horizon (+badVerdictTTL).
func (s *verdictStore) recordBad(major int, reason string) {
	horizon := time.Now().Add(badVerdictTTL)
	s.bucket().Verdicts[fmt.Sprint(major)] = verdictEntry{
		Verdict: "bad", CheckedAt: time.Now(),
		ReProbeAfter: &horizon, Reason: reason,
	}
	s.save()
}

// lastKnownGoodFor returns the current bucket's record, if any.
func (s *verdictStore) lastKnownGoodFor() *lastKnownGood {
	return s.bucket().LastKnownGood
}

// save persists the store atomically (write-temp + rename). A
// persistence failure is logged, never fatal: verdicts are an
// optimization over probing again.
func (s *verdictStore) save() {
	raw, err := json.MarshalIndent(s.buckets, "", "  ")
	if err != nil {
		s.logger.Warn("cfbrowser: marshal compat verdicts", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		s.logger.Warn("cfbrowser: persist compat verdicts", "error", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		s.logger.Warn("cfbrowser: persist compat verdicts", "error", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		s.logger.Warn("cfbrowser: persist compat verdicts", "error", err)
	}
}

// probeOutcome is one probe launch's classification.
type probeOutcome struct {
	// ok: launched, navigated, title rendered.
	ok bool
	// reason is the typed failure (bad and inconclusive).
	reason string
	// inconclusive marks a browser that started but could not
	// navigate (network-level): no verdict is persisted — offline
	// must not poison the store.
	inconclusive bool
}

// probeBinary is the probe seam (tests inject fakes; production is
// probeWithChromedp). Var, not param: the resolution and install
// ladders share it and the cfbrowser suite runs sequentially.
var probeBinary = probeWithChromedp

// probeWithChromedp launches binaryPath through the production driver
// posture (the exact launch path real solves use — headless, own
// process group, ephemeral profile, NO proxy: the probe must reflect
// binary health, not proxy state), navigates probeTargetURL and
// asserts the page title renders.
func probeWithChromedp(ctx context.Context, binaryPath string) probeOutcome {
	profile, err := os.MkdirTemp("", "cfprobe-")
	if err != nil {
		return probeOutcome{reason: fmt.Sprintf("probe profile dir: %v", err)}
	}
	defer func() { _ = os.RemoveAll(profile) }()

	pctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	nav, err := chromedpDriver(LaunchOptions{BinaryPath: binaryPath, UserDataDir: profile})
	if err != nil {
		return probeOutcome{reason: fmt.Sprintf("start chromium: %v", err)}
	}
	defer func() { _ = nav.Close() }()

	st, navErr := nav.Navigate(pctx, probeTargetURL)
	if navErr != nil {
		// The browser process answered with a page-load error
		// (net:: …): it started and rendered — the network path
		// failed, not the binary. Everything else (crashed target,
		// deadline, dead process) is a browser-side failure.
		if strings.HasPrefix(navErr.Error(), "page load error ") {
			return probeOutcome{inconclusive: true, reason: navErr.Error()}
		}
		// The probe context is only ever cancelled by chromedp itself
		// (the probe owns its deadline): a surfaced context.Canceled
		// means the target/renderer died mid-navigation — the exact
		// seccomp-crash shape. Name it for what it is.
		if errors.Is(navErr, context.Canceled) {
			return probeOutcome{reason: fmt.Sprintf(
				"browser process died during navigation of %s (renderer crash): %v", probeTargetURL, navErr)}
		}
		return probeOutcome{reason: fmt.Sprintf("navigate %s: %v", probeTargetURL, navErr)}
	}
	if strings.TrimSpace(st.Title) == "" {
		return probeOutcome{reason: fmt.Sprintf("navigate %s: page rendered with an empty title", probeTargetURL)}
	}
	return probeOutcome{ok: true}
}

// probeAndRecord runs the probe and persists its verdict (good also
// refreshes last-known-good; inconclusive persists nothing). A verdict
// good requires TWO CONSECUTIVE passes — each pass is a fresh launch,
// so a single lucky launch (the reviewer's live-reproduced flaky 151)
// can never lock a good in. A first-pass failure short-circuits: one
// launch spent. Shared by the resolution evaluator and the
// install/update call sites.
func probeAndRecord(ctx context.Context, cacheDir string, bin *BinaryInfo, logger *slog.Logger) probeOutcome {
	out := probeBinary(ctx, bin.Path)
	switch {
	case out.ok:
		// First pass alone proves nothing: demand a second
		// consecutive pass before any good is recorded.
		out2 := probeBinary(ctx, bin.Path)
		switch {
		case out2.ok:
			out = out2
			logger.Info("cfbrowser: probe passed twice — verdict good",
				"version", bin.Version, "path", bin.Path)
		case out2.inconclusive:
			// The second pass could not navigate: no verdict either
			// way — the first pass alone proves nothing.
			logger.Warn("cfbrowser: второй проход неубечный (сеть) — вердикт не записан",
				"version", bin.Version, "reason", out2.reason)
			return out2
		default:
			logger.Warn("cfbrowser: второй проход провалился после удачного первого — вердикт bad (flaky)",
				"version", bin.Version, "reason", out2.reason)
			out = probeOutcome{reason: out2.reason}
		}
	case out.inconclusive:
		logger.Warn("cfbrowser: probe inconclusive (network) — вердикт не записан",
			"version", bin.Version, "reason", out.reason)
		return out
	default:
		logger.Warn("cfbrowser: probe FAILED — вердикт bad", "version", bin.Version, "reason", out.reason)
	}

	// PR85: this path SAVES — wire the ladder's logger so the persist
	// diagnostics land on the file sink, never stderr.
	s := loadVerdictStore(cacheDir)
	s.logger = logger
	major, _ := versionMajor(bin.Version)
	if out.ok {
		s.recordGood(major, lastKnownGood{Version: bin.Version, Channel: bin.Channel, Path: bin.Path})
	} else if !out.inconclusive {
		s.recordBad(major, out.reason)
	}
	return out
}

// freshBadVerdict consults the store for one version: has is true
// when a fresh bad verdict currently binds (no probes, verdicts only).
func freshBadVerdict(cacheDir, version string) (verdictEntry, bool) {
	major, ok := versionMajor(version)
	if !ok {
		return verdictEntry{}, false
	}
	if e, has := loadVerdictStore(cacheDir).verdictFor(major); has && e.Verdict == "bad" {
		return e, true
	}
	return verdictEntry{}, false
}

// newestServingCandidate reports the newest candidate the store does
// not freshly reject. Verdicts only — never probes: this is the
// updater's comparison baseline, which must stay network-cheap.
func newestServingCandidate(cacheDir string, cands []*BinaryInfo) *BinaryInfo {
	for _, bin := range cands {
		if _, ok := versionMajor(bin.Version); !ok {
			continue // unparsable fails closed
		}
		if _, bad := freshBadVerdict(cacheDir, bin.Version); bad {
			continue // a fresh bad verdict cannot be the serving baseline
		}
		return bin
	}
	return nil
}

// candidateAttempt is one resolution step's record (the loud
// total-failure error names every attempt).
type candidateAttempt struct {
	Version string
	Outcome string
	Detail  string
}

// Outcome labels for the attempt records and logs.
const (
	outcomeGoodVerdict   = "good-verdict"
	outcomeFreshBad      = "fresh-bad-skip"
	outcomeProbeGood     = "probe-good"
	outcomeProbeBad      = "probe-bad"
	outcomeUnverified    = "unverified-offline"
	outcomeLastKnownGood = "last-known-good"
	outcomeUnparsable    = "unparsable-skip"
)

// channelHonorsFilter reports whether a binary channel satisfies a
// resolution filter (auto honors everything; free/pro their lines).
func channelHonorsFilter(filter, channel string) bool {
	switch filter {
	case channelFree:
		return channel == channelFree
	case channelPro:
		return channel == channelPro
	default:
		return true
	}
}

// lastKnownGoodRung is the emergency serving rung: the store's
// last-known-good, served only while its recorded binary is still
// installed (the record without bytes is useless). The channel filter
// normally applies; the mandate's exemption serves an off-filter LKG
// that IS installed — forcing the last working browser over channel
// bookkeeping at the exhaustion point, always with a loud note.
func lastKnownGoodRung(cacheDir, filter string, logger *slog.Logger) (*BinaryInfo, bool) {
	s := loadVerdictStore(cacheDir)
	lkg := s.lastKnownGoodFor()
	if lkg == nil {
		return nil, false
	}
	if _, err := os.Stat(lkg.Path); err != nil {
		logger.Warn("cfbrowser: last-known-good недоступен (бинарник удалён из кэша)",
			"version", lkg.Version, "path", lkg.Path)
		return nil, false
	}
	// A FRESH bad verdict on the LKG's own major is a guaranteed
	// failed launch — the emergency rung must not serve it (PR76
	// review minor). An expired bad does not block: the expiry IS the
	// decision that the old bad no longer binds.
	if e, has := s.verdictFor(lkg.Major); has && e.Verdict == "bad" {
		logger.Warn("cfbrowser: last-known-good держит свежий вердикт bad — запуск заведомо провалится",
			"version", lkg.Version, "reason", e.Reason)
		return nil, false
	}
	if !channelHonorsFilter(filter, lkg.Channel) {
		logger.Warn("cfbrowser: candidates exhausted — last-known-good вне фильтра канала, "+
			"но установлен: принудительно запускаем последнюю работавшую",
			"version", lkg.Version, "channel", lkg.Channel, "filter", filter)
	} else {
		logger.Info("cfbrowser: candidates exhausted — serving last-known-good",
			"version", lkg.Version, "channel", lkg.Channel, "path", lkg.Path)
	}
	return &BinaryInfo{
		Path: lkg.Path, Dir: filepath.Dir(lkg.Path),
		Version: lkg.Version, Channel: lkg.Channel,
	}, true
}

// evaluateCandidates walks candidate groups in order (the caller
// encodes the channel posture in the grouping: auto passes the pro
// group first, then free) under the resolution filter, and returns
// the first usable binary:
//
//  1. verdict good → use (no probe);
//  2. verdict bad and fresh → skip;
//  3. no verdict (or expired) → probe decides and persists; with
//     noProbe the candidate is served tentatively with a loud note
//     and nothing persisted (advisory surfaces);
//  4. all candidates exhausted → the store's last-known-good (see
//     lastKnownGoodRung);
//  5. nothing → a typed *CompatError naming every attempt.
//
// An offline probe (browser started, navigation died at the network
// layer) serves the candidate UNVERIFIED with a loud note and
// persists nothing: verdicts come from real navigations only.
func evaluateCandidates(ctx context.Context, cacheDir, filter string, noProbe bool, logger *slog.Logger, groups ...[]*BinaryInfo) (*BinaryInfo, []candidateAttempt, error) {
	s := loadVerdictStore(cacheDir)
	var attempts []candidateAttempt
	served := map[string]bool{}
	newest := ""

	for _, group := range groups {
		for _, bin := range group {
			if bin == nil || served[bin.Version] {
				continue
			}
			if newest == "" {
				newest = bin.Version
			}
			served[bin.Version] = true
			major, ok := versionMajor(bin.Version)
			if !ok {
				attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeUnparsable,
					Detail: "unparsable version fails closed"})
				continue
			}
			if e, has := s.verdictFor(major); has {
				switch e.Verdict {
				case "good":
					logger.Info("cfbrowser: candidate verdict good", "version", bin.Version)
					attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeGoodVerdict})
					return bin, attempts, nil
				case "bad":
					logger.Warn("cfbrowser: candidate verdict bad (fresh) — пропуск",
						"version", bin.Version, "reason", e.Reason)
					attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeFreshBad,
						Detail: e.Reason})
					continue
				}
			}
			// No verdict (or an expired one): the probe decides —
			// unless the caller runs verdicts-only (advisory).
			if noProbe {
				logger.Warn("cfbrowser: вердикта нет, probe пропущен (advisory-режим) — "+
					"версия используется без проверки", "version", bin.Version)
				attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeUnverified,
					Detail: "no verdict (probe skipped)"})
				return bin, attempts, nil
			}
			out := probeAndRecord(ctx, cacheDir, bin, logger)
			switch {
			case out.ok:
				attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeProbeGood})
				return bin, attempts, nil
			case out.inconclusive:
				logger.Warn("cfbrowser: probe невозможен (сеть) — версия используется без вердикта",
					"version", bin.Version, "reason", out.reason)
				attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeUnverified,
					Detail: out.reason})
				return bin, attempts, nil
			default:
				attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeProbeBad,
					Detail: out.reason})
			}
		}
	}

	// Candidates exhausted: the last-known-good rung.
	if bin, ok := lastKnownGoodRung(cacheDir, filter, logger); ok {
		attempts = append(attempts, candidateAttempt{Version: bin.Version, Outcome: outcomeLastKnownGood})
		return bin, attempts, nil
	}

	reason := "no working chromium found"
	if n := len(attempts); n > 0 {
		reason = attempts[n-1].Detail
	}
	return nil, attempts, &CompatError{Newest: newest, Attempts: attempts, Reason: reason}
}
