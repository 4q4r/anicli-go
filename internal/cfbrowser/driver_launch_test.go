package cfbrowser

import (
	"testing"

	"github.com/chromedp/cdproto/network"
)

// buildAllocatorArgs is the single source of truth for the browser
// argv: the exact-table tests below pin every flag (headless ALWAYS,
// memory diet, conditional proxy/lang). allocatorOptions derives the
// chromedp options from this very list, so the tables double as the
// mapping contract between argv and ExecAllocatorOption.

// fixedAllocatorFlags is the launch posture shared by every solve:
// headless-only, memory-diet, no background churn. Order matters only
// to the tests (chromedp stores flags in a map); the LIST is the spec.
func fixedAllocatorFlags() []string {
	return []string{
		"--headless",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--disable-extensions",
		"--disable-background-networking",
		"--mute-audio",
		"--hide-scrollbars",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync",
		"--disable-translate",
		"--renderer-process-limit=1",
		"--disable-component-update",
		"--password-store=basic",
		"--use-mock-keychain",
	}
}

func wantArgs(extra ...string) []string {
	out := append([]string{}, fixedAllocatorFlags()...)
	return append(out, extra...)
}

func argsEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBuildAllocatorArgsHeadlessOnlyExactTable pins the minimal argv
// exactly: headless is ALWAYS present (headless-only ruling — there is
// no code path that can drop it) and the memory-diet flags ride along.
func TestBuildAllocatorArgsHeadlessOnlyExactTable(t *testing.T) {
	got := buildAllocatorArgs(LaunchOptions{
		BinaryPath:  "/opt/cloakbrowser/chrome",
		UserDataDir: "/data/cfprofile",
	})
	want := wantArgs("--user-data-dir=/data/cfprofile")
	if !argsEqual(got, want) {
		t.Errorf("argv mismatch:\n got  %v\n want %v", got, want)
	}
}

// TestBuildAllocatorArgsProxyAndLangConditional pins the conditional
// tail: proxy-server and lang appear only when configured, in that
// order, after the fixed flags.
func TestBuildAllocatorArgsProxyAndLangConditional(t *testing.T) {
	got := buildAllocatorArgs(LaunchOptions{
		BinaryPath:  "/opt/cloakbrowser/chrome",
		UserDataDir: "/data/cfprofile",
		ProxyURL:    "http://127.0.0.1:8080",
		Locale:      "ru-RU",
	})
	want := wantArgs(
		"--user-data-dir=/data/cfprofile",
		"--proxy-server=http://127.0.0.1:8080",
		"--lang=ru-RU",
	)
	if !argsEqual(got, want) {
		t.Errorf("argv mismatch:\n got  %v\n want %v", got, want)
	}

	// Locale without proxy: lang still lands, proxy flag must NOT.
	got = buildAllocatorArgs(LaunchOptions{
		BinaryPath:  "/opt/cloakbrowser/chrome",
		UserDataDir: "/data/cfprofile",
		Locale:      "de-DE",
	})
	want = wantArgs(
		"--user-data-dir=/data/cfprofile",
		"--lang=de-DE",
	)
	if !argsEqual(got, want) {
		t.Errorf("argv mismatch (locale only):\n got  %v\n want %v", got, want)
	}
}

// TestBuildAllocatorArgsOmitsDefaultFlags guards the PR14 bug fix:
// the previous driver appended chromedp.DefaultExecAllocatorOptions
// (which smuggle their own Headless, --enable-automation,
// --disable-features, … wholesale). Our explicit list must NOT grow
// those defaults back.
func TestBuildAllocatorArgsOmitsDefaultFlags(t *testing.T) {
	got := buildAllocatorArgs(LaunchOptions{
		BinaryPath:  "/opt/cloakbrowser/chrome",
		UserDataDir: "/data/cfprofile",
		ProxyURL:    "socks5://127.0.0.1:1080",
		Locale:      "ru-RU",
	})
	banned := []string{
		"--enable-automation",
		"--disable-features=site-per-process,Translate,BlinkGenPropertyTrees",
		"--enable-features=NetworkService,NetworkServiceInProcess",
		"--disable-default-apps",
		"--force-color-profile=srgb",
	}
	set := make(map[string]struct{}, len(got))
	for _, a := range got {
		set[a] = struct{}{}
	}
	for _, b := range banned {
		if _, ok := set[b]; ok {
			t.Errorf("argv must not carry chromedp default flag %q (explicit list replaced wholesale)", b)
		}
	}
}

// TestFlagFromArgRoundTripMapping is the documented mapping test:
// allocatorOptions feeds every argv entry through flagFromArg into
// chromedp.Flag(name, value). chromedp renders string values back as
// --name=value and boolean true as bare --name (allocate.go
// Allocate), so the inverse parse must reproduce the argv exactly —
// proving the option list and the tested argv are one and the same
// list.
func TestFlagFromArgRoundTripMapping(t *testing.T) {
	for _, opts := range []LaunchOptions{
		{BinaryPath: "/bin/chrome", UserDataDir: "/p"},
		{BinaryPath: "/bin/chrome", UserDataDir: "/p", ProxyURL: "http://127.0.0.1:9", Locale: "ru-RU"},
		{BinaryPath: "/bin/chrome", UserDataDir: "/p", ProxyURL: "socks5://127.0.0.1:1080", Locale: "en-US"},
	} {
		for _, arg := range buildAllocatorArgs(opts) {
			name, value := flagFromArg(arg)
			if name == "" {
				t.Fatalf("flagFromArg(%q): empty name", arg)
			}
			var rendered string
			switch v := value.(type) {
			case string:
				rendered = "--" + name + "=" + v
			case bool:
				if !v {
					t.Fatalf("flagFromArg(%q): boolean false has no rendering", arg)
				}
				rendered = "--" + name
			default:
				t.Fatalf("flagFromArg(%q): unsupported value type %T", arg, value)
			}
			if rendered != arg {
				t.Errorf("round trip %q -> (%q,%v) -> %q", arg, name, value, rendered)
			}
		}
	}
}

// TestBlockedResourceTypeTable pins the solve-page resource diet:
// Image, Media and Font are denied; everything else — most critically
// Stylesheet (Turnstile renders visually; breaking CSS breaks the
// widget), Script, XHR/Fetch (challenge orchestration), Document,
// WebSocket and frames — flows untouched.
func TestBlockedResourceTypeTable(t *testing.T) {
	cases := map[network.ResourceType]bool{
		network.ResourceTypeDocument:           false,
		network.ResourceTypeStylesheet:         false, // NOT blocked: Turnstile renders visually
		network.ResourceTypeImage:              true,
		network.ResourceTypeMedia:              true,
		network.ResourceTypeFont:               true,
		network.ResourceTypeScript:             false,
		network.ResourceTypeTextTrack:          false,
		network.ResourceTypeXHR:                false,
		network.ResourceTypeFetch:              false,
		network.ResourceTypePrefetch:           false,
		network.ResourceTypeEventSource:        false,
		network.ResourceTypeWebSocket:          false,
		network.ResourceTypeManifest:           false,
		network.ResourceTypeSignedExchange:     false,
		network.ResourceTypePing:               false,
		network.ResourceTypeCSPViolationReport: false,
		network.ResourceTypePreflight:          false,
		network.ResourceTypeFedCM:              false,
		network.ResourceTypeOther:              false,
	}
	for rt, want := range cases {
		if got := blockedResourceType(rt); got != want {
			t.Errorf("blockedResourceType(%s) = %v, want %v", rt, got, want)
		}
	}
}
