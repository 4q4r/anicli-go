package cfbrowser

import (
	"fmt"
	"strconv"
	"strings"
)

// Channel selections (config [cf] channel; PR73):
//
//   - auto (default): free is the base; a valid license key upgrades
//     installs and updates to the pro line — best-effort, never at
//     the price of a working free binary;
//   - free: the pro channel is never touched, even with a key;
//   - pro: the pre-PR73 license-keyed ladder (pro failures loud, no
//     silent free downgrade).
const (
	// channelAuto is the zero-value/default selection.
	channelAuto = "auto"
)

// ChannelAuto is the free-base, license-upgrade channel selection
// ([cf] channel = "auto") — exported for CLI comparisons.
const ChannelAuto = channelAuto

// maxKnownGoodChromiumMajor is the newest chromium major verified to
// work with the driver end-to-end on the reference machine: 146 was
// live-verified in PR73/PR75; 151 initially died on the first real
// navigation (PR75 root-caused it to the 151 binary's seccomp sandbox
// on the then-running kernel 7.2.6-zen2 — exit 76 reproducible without
// chromedp, NOT a chromedp API defect) and passed the same end-to-end
// chain live on 2026-09-20 after a kernel swap, so 151 is verified
// too. The limit moves only on live verification of the next major.
// Test-overridable by assignment (the cfbrowser test suite runs
// sequentially — no t.Parallel).
var maxKnownGoodChromiumMajor = 151

// CompatError reports a resolution blocked by the compatibility
// bound: the newest available chromium build is above the last
// verified-good major. It is loud and typed on purpose — the real
// state (unverified, known-broken in one tested case) must be named,
// never papered over with a knowingly-broken binary or a silent
// channel switch.
type CompatError struct {
	// Newest is the newest rejected version.
	Newest string
	// Bound is the known-good major ceiling at rejection time.
	Bound int
}

// Error implements error, naming the rejected version and the fix.
func (e *CompatError) Error() string {
	return fmt.Sprintf(
		"cfbrowser: chromium %s заблокирован границей совместимости (проверенный максимум: major ≤ %d) — "+
			"лимит поднимет только новый anicli с проверенной живой версией "+
			"(точечный обход: $%s или $%s)",
		e.Newest, e.Bound, EnvBinaryPath, EnvVersion)
}

// versionMajor extracts the leading major segment of a dotted
// version. ok is false for unparsable input (a value beyond a sane
// integer range included).
func versionMajor(version string) (major int, ok bool) {
	head, _, _ := strings.Cut(version, ".")
	if head == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(head, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// chromiumMajorKnownGood reports whether the version's major is within
// the chromedp compatibility bound. Unparsable majors fail closed.
func chromiumMajorKnownGood(version string) bool {
	major, ok := versionMajor(version)
	if !ok {
		return false
	}
	return major <= maxKnownGoodChromiumMajor
}

// checkChromiumCompat gates a fresh resolution (the free-latest
// download path) on the chromedp compatibility bound: serving a
// build the driver cannot control would fail at first Navigate, so
// it is rejected loud and typed instead.
func checkChromiumCompat(version string) error {
	if chromiumMajorKnownGood(version) {
		return nil
	}
	return &CompatError{Newest: version, Bound: maxKnownGoodChromiumMajor}
}

// normalizeChannel validates a configured channel value: "" and
// "auto" select auto (free base + license upgrade), "free" and "pro"
// select their literal lines; anything else is a loud error.
func normalizeChannel(v string) (string, error) {
	switch v {
	case "", channelAuto:
		return channelAuto, nil
	case channelFree:
		return channelFree, nil
	case channelPro:
		return channelPro, nil
	default:
		return "", fmt.Errorf("cfbrowser: unknown channel %q (want auto|free|pro)", v)
	}
}

// pinnedBypassNote is the loud pinned-run warning: $CLOAKBROWSER_VERSION
// is a documented exemption from the chromedp compat bound (explicit
// user intent, like the binary-path override) — but the exemption is
// announced, never silent.
func pinnedBypassNote(version string) string {
	return fmt.Sprintf("cfbrowser: pinned version %s bypasses the chromedp compat bound (major ≤ %d) — "+
		"явное пользовательское исключение", version, maxKnownGoodChromiumMajor)
}
