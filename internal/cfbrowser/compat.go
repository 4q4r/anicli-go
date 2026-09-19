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

// chromedpDriverLabel names the pinned chromedp driver generation the
// compatibility bound is verified against (log and error wording).
const chromedpDriverLabel = "chromedp v0.16"

// maxKnownGoodChromiumMajor is the newest chromium major the pinned
// chromedp driver is verified to control: 146 works (live-verified,
// PR73); 151 kills every session on the first Navigate (PR71). A
// chromedp bump raises it. Test-overridable by assignment (the
// cfbrowser test suite runs sequentially — no t.Parallel).
var maxKnownGoodChromiumMajor = 146

// CompatError reports a resolution blocked by the chromedp
// compatibility bound: the newest available chromium build exceeds
// the major the pinned driver can control. It is loud and typed on
// purpose — the fix (a chromedp bump shipping in a new anicli) must
// be named, never papered over with a knowingly-broken binary or a
// silent channel switch.
type CompatError struct {
	// Newest is the newest rejected version.
	Newest string
	// Bound is the known-good major ceiling at rejection time.
	Bound int
}

// Error implements error, naming the rejected version and the fix.
func (e *CompatError) Error() string {
	return fmt.Sprintf(
		"cfbrowser: chromium %s несовместим с драйвером %s (проверенный максимум: major ≤ %d) — "+
			"обновите anicli: обновление chromedp поднимет лимит "+
			"(точечный обход: $%s или $%s)",
		e.Newest, chromedpDriverLabel, e.Bound, EnvBinaryPath, EnvVersion)
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
