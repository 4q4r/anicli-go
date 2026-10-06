package sysdeps

// PR148 tests: the startup dependency auto-installer. Everything runs
// against injected seams (Looker, runner, prompt reader) — no test ever
// executes a real package manager or touches the real PATH.

import (
	"bytes"
	"strings"
	"testing"

	anicli "github.com/an0nx/anicli-go"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// fakeLook scripts LookPath results. mpv/ffmpeg resolve only when
// installed is true; tools carries a fixed verdict per tool name.
type fakeLook struct {
	installed bool
	tools     map[string]bool
}

var errLookMissing = errFixed("not found")

func (f *fakeLook) look(name string) (string, error) {
	switch name {
	case "mpv", "ffmpeg":
		if f.installed {
			return "/usr/bin/" + name, nil
		}
		return "", errLookMissing
	case "sh":
		return "/bin/sh", nil
	default:
		if f.tools[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errLookMissing
	}
}

// runFlip records every Run call. With flip set, the first successful
// run marks the required programs installed — modeling an installer
// that lands the binaries on the (fresh) PATH.
type runFlip struct {
	look    *fakeLook
	flip    bool
	calls   [][]string
	failCmd string // when non-empty, commands containing this token fail
}

var errRunFailed = errFixed("exit status 1")

func (r *runFlip) run(bin string, args []string) error {
	argv := append([]string{bin}, args...)
	r.calls = append(r.calls, argv)
	if r.failCmd != "" && strings.Contains(strings.Join(argv, " "), r.failCmd) {
		return errRunFailed
	}
	if r.flip {
		r.look.installed = true
	}
	return nil
}

type errFixed string

func (e errFixed) Error() string { return string(e) }

// bundle installs the real bundled locale table for locale and restores
// the previous process-global bundle after the test.
func bundle(t *testing.T, locale string) {
	t.Helper()
	b, err := i18n.Load(anicli.Locales, "", locale)
	if err != nil {
		t.Fatalf("load bundled %s: %v", locale, err)
	}
	prev := i18n.Active()
	i18n.SetBundle(b)
	t.Cleanup(func() { i18n.SetBundle(prev) })
}

// --- detection -------------------------------------------------------

func TestDetectMissingReportsInRequiredOrder(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{}}
	got := Detect(look.look)
	want := []string{"mpv", "ffmpeg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Detect() = %v, want %v (required order)", got, want)
	}
}

func TestDetectPartialMissing(t *testing.T) {
	l := func(name string) (string, error) {
		if name == "mpv" {
			return "/usr/bin/mpv", nil
		}
		return "", errLookMissing
	}
	if got := Detect(l); strings.Join(got, ",") != "ffmpeg" {
		t.Fatalf("Detect() = %v, want [ffmpeg]", got)
	}
}

func TestDetectAllPresent(t *testing.T) {
	look := &fakeLook{installed: true, tools: map[string]bool{}}
	if got := Detect(look.look); len(got) != 0 {
		t.Fatalf("Detect() = %v, want empty", got)
	}
}

// --- message building (plural/join logic) ----------------------------

func TestMissingMessageSingularRu(t *testing.T) {
	bundle(t, "ru")
	if got := MissingMessage([]string{"mpv"}); got != "mpv не установлена" {
		t.Fatalf("MissingMessage([mpv]) = %q, want %q", got, "mpv не установлена")
	}
}

func TestMissingMessagePluralRuJoinsWithI(t *testing.T) {
	bundle(t, "ru")
	if got, want := MissingMessage([]string{"mpv", "ffmpeg"}), "mpv и ffmpeg не установлены"; got != want {
		t.Fatalf("MissingMessage([mpv ffmpeg]) = %q, want %q", got, want)
	}
}

func TestMissingMessageSingularEn(t *testing.T) {
	bundle(t, "en")
	if got := MissingMessage([]string{"mpv"}); got != "mpv is not installed" {
		t.Fatalf("MissingMessage([mpv]) = %q, want %q", got, "mpv is not installed")
	}
}

func TestMissingMessagePluralEn(t *testing.T) {
	bundle(t, "en")
	if got, want := MissingMessage([]string{"mpv", "ffmpeg"}), "mpv and ffmpeg are not installed"; got != want {
		t.Fatalf("MissingMessage([mpv ffmpeg]) = %q, want %q", got, want)
	}
}

// --- EnsureStartup: the full startup flow ----------------------------

func TestEnsureStartupAllPresentIsSilent(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	look := &fakeLook{installed: true, tools: map[string]bool{}}
	run := &runFlip{look: look}
	env := Env{Out: &out, Look: look.look, Run: run.run, In: strings.NewReader(""), InTTY: true}
	if notices := EnsureStartup(env); len(notices) != 0 {
		t.Fatalf("notices = %v, want none", notices)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want silence", out.String())
	}
	if len(run.calls) != 0 {
		t.Fatalf("runner called: %v, want no installs", run.calls)
	}
}

func TestEnsureStartupNonTTYNeverPrompts(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	look := &fakeLook{tools: map[string]bool{"apt": true}}
	run := &runFlip{look: look}
	env := Env{Out: &out, Look: look.look, Run: run.run, In: strings.NewReader("y\ny\n"), InTTY: false}
	notices := EnsureStartup(env)
	got := out.String()
	if !strings.Contains(got, "mpv и ffmpeg не установлены") {
		t.Errorf("output missing the localized missing-message:\n%s", got)
	}
	if !strings.Contains(got, "sudo apt install mpv ffmpeg") {
		t.Errorf("output missing the exact actionable command:\n%s", got)
	}
	if strings.Contains(got, "Установить?") {
		t.Errorf("non-TTY output prompted a question:\n%s", got)
	}
	if len(run.calls) != 0 {
		t.Fatalf("runner called under non-TTY: %v, want never", run.calls)
	}
	if len(notices) == 0 {
		t.Fatal("notices empty, want the missing-deps warning")
	}
}

func TestEnsureStartupDeclinedContinuesWithoutRunning(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	look := &fakeLook{tools: map[string]bool{"apt": true}}
	run := &runFlip{look: look}
	env := Env{Out: &out, Look: look.look, Run: run.run, In: strings.NewReader("n\n"), InTTY: true}
	notices := EnsureStartup(env)
	got := out.String()
	if !strings.Contains(got, "Установить?") {
		t.Errorf("TTY flow never asked the Y/n question:\n%s", got)
	}
	if len(run.calls) != 0 {
		t.Fatalf("declined install still ran: %v", run.calls)
	}
	if len(notices) == 0 {
		t.Fatal("declined flow produced no startup warning notice")
	}
	if !strings.Contains(got, "продолжит работу") {
		t.Errorf("declined flow missing the continue note:\n%s", got)
	}
}

func TestEnsureStartupAcceptedRunsThenRecheckStillMissing(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	// The installer runs but the binary never lands on our (stale) PATH:
	// the runner does NOT flip the looker.
	look := &fakeLook{tools: map[string]bool{"apt": true}}
	var calls [][]string
	run := func(bin string, args []string) error {
		calls = append(calls, append([]string{bin}, args...))
		return nil // installer "succeeded", PATH is still stale
	}
	env := Env{Out: &out, Look: look.look, Run: run, In: strings.NewReader("y\n"), InTTY: true}
	notices := EnsureStartup(env)
	got := out.String()
	want := "sudo apt install mpv ffmpeg"
	if len(calls) != 1 || strings.Join(calls[0], " ") != want {
		t.Fatalf("runner calls = %v, want exactly [%s] as a direct child", calls, want)
	}
	if !strings.Contains(got, "перезапустите терминал") {
		t.Errorf("missing the fresh-PATH caveat:\n%s", got)
	}
	if len(notices) == 0 {
		t.Fatal("still-missing after install must keep the warning notice")
	}
}

func TestEnsureStartupAcceptedRunsAndFixedStaysQuiet(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	look := &fakeLook{tools: map[string]bool{"apt": true}}
	run := &runFlip{look: look, flip: true} // Run flips the looker: the binary landed
	env := Env{Out: &out, Look: look.look, Run: run.run, In: strings.NewReader("y\n"), InTTY: true}
	if notices := EnsureStartup(env); len(notices) != 0 {
		t.Fatalf("notices = %v, want none after a successful install", notices)
	}
	if strings.Contains(out.String(), "перезапустите терминал") {
		t.Errorf("fresh-PATH caveat printed although the recheck passed:\n%s", out.String())
	}
}

func TestEnsureStartupRunFailureContinuesWithWarning(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	look := &fakeLook{tools: map[string]bool{"apt": true}}
	run := &runFlip{look: look, failCmd: "apt"}
	env := Env{Out: &out, Look: look.look, Run: run.run, In: strings.NewReader("y\n"), InTTY: true}
	if notices := EnsureStartup(env); len(notices) == 0 {
		t.Fatal("failed install must keep the warning notice")
	}
	if !strings.Contains(out.String(), "Не удалось выполнить") {
		t.Errorf("missing the run-failure line:\n%s", out.String())
	}
}

// --- per-OS strategy selection ---------------------------------------

func argvOf(st step) string { return strings.Join(st.argv, " ") }

func TestWindowsPlanPrefersWinget(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{"winget": true, "choco": true}}
	p := pickWindowsPlan(look.look)
	if p.titleKey != "deps.offer_winget" {
		t.Fatalf("titleKey = %q, want deps.offer_winget", p.titleKey)
	}
	if p.bootstrap {
		t.Fatal("winget plan must not be a bootstrap")
	}
	got := make([]string, 0, len(p.steps))
	for _, st := range p.steps {
		got = append(got, argvOf(st))
	}
	joined := strings.Join(got, "|")
	for _, want := range []string{"winget install -e --id shinchiro.mpv", "winget install -e --id Gyan.FFmpeg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("winget plan commands %q missing %q", joined, want)
		}
	}
}

func TestWindowsPlanFallsBackToChocolatey(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{"choco": true}}
	p := pickWindowsPlan(look.look)
	if p.titleKey != "deps.offer_choco" {
		t.Fatalf("titleKey = %q, want deps.offer_choco", p.titleKey)
	}
	if argvOf(p.steps[0]) != "choco install mpvio ffmpeg -y" {
		t.Fatalf("choco command = %q", argvOf(p.steps[0]))
	}
}

func TestWindowsPlanNeitherBootstrapsChocolatey(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{}}
	p := pickWindowsPlan(look.look)
	if !p.bootstrap {
		t.Fatal("plan without winget/choco must be a package-manager bootstrap")
	}
	if p.titleKey != "deps.offer_bootstrap_choco" {
		t.Fatalf("titleKey = %q, want deps.offer_bootstrap_choco", p.titleKey)
	}
	if len(p.steps) == 0 || !strings.Contains(argvOf(p.steps[0]), "community.chocolatey.org/install.ps1") {
		t.Fatalf("bootstrap step must run the official Chocolatey installer, got %v", p.steps)
	}
}

func TestDarwinPlanBrewPresent(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{"brew": true}}
	p := pickDarwinPlan(look.look)
	if p.titleKey != "deps.offer_brew" || p.bootstrap {
		t.Fatalf("plan = %+v, want the brew offer", p)
	}
	if argvOf(p.steps[0]) != "brew install mpv ffmpeg" {
		t.Fatalf("brew command = %q", argvOf(p.steps[0]))
	}
}

func TestDarwinPlanBootstrapsHomebrew(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{}}
	p := pickDarwinPlan(look.look)
	if !p.bootstrap || p.titleKey != "deps.offer_bootstrap_brew" {
		t.Fatalf("plan = %+v, want the Homebrew bootstrap", p)
	}
	if len(p.steps) == 0 || !strings.Contains(argvOf(p.steps[0]), "Homebrew/install") {
		t.Fatalf("bootstrap step must run the official Homebrew installer, got %v", p.steps)
	}
}

func TestLinuxPlanDetectsEachPackageManager(t *testing.T) {
	cases := []struct {
		pm       string
		titleKey string
	}{
		{"apt", "deps.offer_apt"},
		{"dnf", "deps.offer_dnf"},
		{"pacman", "deps.offer_pacman"},
		{"zypper", "deps.offer_zypper"},
	}
	for _, tc := range cases {
		look := &fakeLook{tools: map[string]bool{tc.pm: true}}
		p := pickLinuxPlan(look.look)
		if p.titleKey != tc.titleKey {
			t.Errorf("pm %s: titleKey = %q, want %q", tc.pm, p.titleKey, tc.titleKey)
		}
		if p.bootstrap || len(p.manual) != 0 || len(p.steps) == 0 {
			t.Errorf("pm %s: plan must be a direct install offer, got %+v", tc.pm, p)
		}
	}
}

func TestLinuxPlanDnfEnablesRPMFusionFirst(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{"dnf": true}}
	p := pickLinuxPlan(look.look)
	if len(p.steps) != 2 {
		t.Fatalf("dnf plan has %d steps, want 2 (RPM Fusion first)", len(p.steps))
	}
	if !strings.Contains(argvOf(p.steps[0]), "rpmfusion-free-release") {
		t.Errorf("dnf step 1 = %q, want the RPM Fusion free release enable", argvOf(p.steps[0]))
	}
	if argvOf(p.steps[1]) != "sudo dnf install mpv ffmpeg" {
		t.Errorf("dnf step 2 = %q", argvOf(p.steps[1]))
	}
}

func TestLinuxPlanNoneDetectedPrintsManualOnly(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{}}
	p := pickLinuxPlan(look.look)
	if len(p.steps) != 0 {
		t.Fatalf("no-PM plan must have no runnable steps, got %v", p.steps)
	}
	if len(p.manual) < 4 {
		t.Fatalf("manual entries = %d, want per-distro-family lines", len(p.manual))
	}
}

func TestPlanForDispatchesByGOOS(t *testing.T) {
	look := &fakeLook{tools: map[string]bool{"winget": true}}
	if p := planFor("windows", look.look); p.titleKey != "deps.offer_winget" {
		t.Errorf("planFor(windows) titleKey = %q", p.titleKey)
	}
	if p := planFor("darwin", look.look); !p.bootstrap {
		t.Errorf("planFor(darwin) with only winget present must bootstrap brew, got %+v", p)
	}
	if p := planFor("linux", look.look); len(p.manual) == 0 {
		t.Errorf("planFor(linux) with no linux PM must be the manual plan")
	}
}

// --- bootstrap chain (install a package manager first) ----------------

func TestBootstrapThenInstallDarwin(t *testing.T) {
	bundle(t, "ru")
	var out bytes.Buffer
	look := &fakeLook{tools: map[string]bool{}} // no brew, no mpv/ffmpeg
	var calls [][]string
	run := func(bin string, args []string) error {
		calls = append(calls, append([]string{bin}, args...))
		if bin == "/bin/bash" {
			look.tools["brew"] = true // Homebrew landed
			return nil
		}
		look.installed = true // brew install landed the programs
		return nil
	}
	env := Env{Out: &out, Look: look.look, Run: run, In: strings.NewReader("y\ny\n"), InTTY: true, GOOS: "darwin"}
	if notices := EnsureStartup(env); len(notices) != 0 {
		t.Fatalf("notices = %v, want none after a successful chain", notices)
	}
	if len(calls) != 2 {
		t.Fatalf("runner calls = %v, want bootstrap then brew install", calls)
	}
	if !strings.Contains(strings.Join(calls[1], " "), "brew install mpv ffmpeg") {
		t.Errorf("second call = %v, want the brew install", calls[1])
	}
}

// --- i18n inventory ---------------------------------------------------

func TestEveryReferencedDepsKeyExists(t *testing.T) {
	en, err := i18n.Load(anicli.Locales, "", "en")
	if err != nil {
		t.Fatalf("load en: %v", err)
	}
	ru, err := i18n.Load(anicli.Locales, "", "ru")
	if err != nil {
		t.Fatalf("load ru: %v", err)
	}
	for _, key := range referencedKeys(t) {
		if got := en.T(key); got == key {
			t.Errorf("key %q missing from en.toml", key)
		}
		if got := ru.T(key); got == key {
			t.Errorf("key %q missing from ru.toml", key)
		}
	}
}
