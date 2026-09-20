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

// CompatError reports a resolution where no candidate chromium could
// be verified to work (PR76: the verdict store, the probe and the
// last-known-good rung all came up empty). It is loud and typed on
// purpose: the real state — every attempt named, with the typed probe
// reason — must be visible, never papered over with a
// knowingly-broken binary or a silent channel switch.
type CompatError struct {
	// Newest is the newest candidate that was evaluated.
	Newest string
	// Reason is the typed failure of the last attempt.
	Reason string
	// Attempts records every candidate evaluation, in order.
	Attempts []candidateAttempt
	// Fallback is the last-known-good version served instead ("" when
	// nothing could be served).
	Fallback string
}

// Error implements error, naming the rejected version, the reason and
// the escape hatches. PR75 honesty: it never promises a chromedp fix
// — the "incompatibility" was kernel-state, not an API defect.
func (e *CompatError) Error() string {
	var b strings.Builder
	if e.Fallback != "" {
		fmt.Fprintf(&b, "cfbrowser: chromium %s не прошёл проверку запуска (%s) — "+
			"используется последняя работавшая %s", e.Newest, e.Reason, e.Fallback)
	} else {
		fmt.Fprintf(&b, "cfbrowser: рабочая chromium не найдена (последняя попытка %s: %s)",
			e.Newest, e.Reason)
	}
	if len(e.Attempts) > 0 {
		b.WriteString("; попытки: ")
		for i, a := range e.Attempts {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%s", a.Version, a.Outcome)
		}
	}
	fmt.Fprintf(&b, " (точечный обход: $%s или $%s)", EnvBinaryPath, EnvVersion)
	return b.String()
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

// pinnedBypassNote is the loud pinned-run warning: $CLOAKBROWSER_VERSION
// bypasses the verdict mechanism (no probe gates an explicit user
// pin) — the exemption is announced, never silent.
func pinnedBypassNote(version string) string {
	return fmt.Sprintf("cfbrowser: pinned version %s bypasses the launch-verdict mechanism — "+
		"явное пользовательское исключение (без проверки запуска)", version)
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
