// Package sysdeps is the PR148 startup dependency check: it verifies
// the external programs anicli needs — mpv (playback) and ffmpeg
// (download mux) — via exec.LookPath and, when one is missing, offers
// to install it through the platform package manager (winget/choco on
// Windows, Homebrew on macOS, apt/dnf/pacman/zypper on Linux).
//
// Contract (owner spec, PR148):
//   - the localized warning names the missing programs («mpv и ffmpeg
//     не установлены» — joined with « и », singular verb form for one);
//   - interactive prompting ONLY when stdin is a TTY; under
//     systemd/docker/pipe the exact actionable commands are printed
//     instead and startup never blocks;
//   - a declined or failed install NEVER aborts startup: search,
//     browsing and torrents work, playback/download warn again at use
//     (the player and downloader fail loud on a missing binary), and
//     the warning rides the TUI startup notices;
//   - after an accepted install the programs are re-checked; when they
//     are still not on PATH (fresh-PATH caveat of winget/brew/choco
//     installers) the user is told a terminal restart may be needed;
//   - installers run as direct child processes only — no name-based
//     process operations anywhere.
//
// The OS seam is the goos PARAMETER of planFor (not build-tagged
// files): every strategy compiles on every platform, so the
// windows/darwin plans are unit-tested on linux and the three-GOOS
// builds are proven in CI. Tests inject Looker/runner seams and never
// execute a real package manager.
package sysdeps

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// required are the external programs checked at startup, in the fixed
// message order (mpv first — the spec's example «mpv и ffmpeg»).
var required = []string{"mpv", "ffmpeg"}

// Looker resolves a program name on PATH; production passes
// exec.LookPath, tests script the verdict.
type Looker func(name string) (string, error)

// runner executes one install command as a direct child process;
// production passes realRunner, tests record the argv.
type runner func(bin string, args []string) error

// Detect reports the required programs missing from PATH, in the
// required order.
func Detect(look Looker) []string {
	var missing []string
	for _, name := range required {
		if _, err := look(name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// MissingMessage renders the localized «не установлена/не установлены»
// line: one missing program takes the singular verb form, several are
// joined with the locale's separator (« и » in Russian) and take the
// plural form.
func MissingMessage(names []string) string {
	if len(names) == 1 {
		return i18n.T("deps.missing_one", i18n.Vals{"name": names[0]})
	}
	return i18n.T("deps.missing_many", i18n.Vals{"names": joinNames(names)})
}

// joinNames joins program names with the localized separator.
func joinNames(names []string) string {
	return strings.Join(names, i18n.T("deps.list_sep"))
}

// Env carries the startup-flow seams. The zero-value Look/Run/In fall
// back to the production implementations (exec.LookPath, direct child
// processes, os.Stdin with its TTY verdict); tests inject all of them.
// GOOS overrides the host OS for the plan selection (empty = the real
// runtime.GOOS) so the windows/darwin flows are testable on linux.
type Env struct {
	Out   io.Writer
	Look  Looker
	Run   runner
	In    io.Reader
	InTTY bool
	GOOS  string

	// in is the shared buffered prompt reader (built once from In so a
	// multi-question flow never loses buffered answers).
	in *bufio.Reader
}

// EnsureStartup runs the whole PR148 flow against env and returns the
// extra TUI startup notices (empty when every required program is
// present, or when an accepted install fixed the situation). All
// output is printed pre-TUI on env.Out; the flow never aborts the
// caller — the return value is warnings, never errors.
func EnsureStartup(env Env) []string {
	if env.Out == nil {
		env.Out = io.Discard
	}
	look := env.Look
	if look == nil {
		look = exec.LookPath
	}
	run := env.Run
	if run == nil {
		run = realRunner
	}
	if env.In == nil {
		env.In = os.Stdin
		env.InTTY = stdinIsTTY()
	}
	if env.in == nil {
		env.in = bufio.NewReader(env.In)
	}

	missing := Detect(look)
	if len(missing) == 0 {
		return nil // present: the silent fast path
	}
	say(env.Out, MissingMessage(missing))

	goos := env.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	p := planFor(goos, look)
	var ran bool
	switch {
	case len(p.steps) == 0:
		printManual(env.Out, p)
	case !env.InTTY:
		printCommandsNonTTY(env.Out, goos, look, p)
	default:
		ran = runPlan(env, goos, look, run, p, 0)
	}

	still := Detect(look)
	if len(still) == 0 {
		return nil // an accepted install landed the programs: quiet
	}
	if ran {
		// Fresh-PATH caveat: winget/brew/choco installers update the
		// shell profile or registry, not this process's environment.
		say(env.Out, i18n.T("deps.caveat_fresh_path", i18n.Vals{"names": joinNames(still)}))
	} else {
		say(env.Out, i18n.T("deps.continue_note"))
	}
	return []string{MissingMessage(still)}
}

// runPlan walks one plan: offer title, exact commands, one Y/n
// question, then the commands in order as direct children. A bootstrap
// plan (install a package manager first) re-plans after its steps and
// runs the follow-up install plan when the manager appeared. Returns
// whether any command was attempted.
func runPlan(env Env, goos string, look Looker, run runner, p plan, depth int) bool {
	say(env.Out, i18n.T(p.titleKey))
	printPlanCommands(env.Out, p)
	_, _ = fmt.Fprintf(env.Out, "%s %s ", i18n.T("deps.ask"), i18n.T("deps.prompt_hint"))
	if !readYn(env.in) {
		return false // declined: startup continues below
	}
	ran := true
	for _, st := range p.steps {
		d := st.display()
		say(env.Out, i18n.T("deps.running", i18n.Vals{"cmd": d}))
		if err := run(st.argv[0], st.argv[1:]); err != nil {
			say(env.Out, i18n.T("deps.run_failed", i18n.Vals{"cmd": d, "err": err.Error()}))
			return ran // stop the remaining steps; startup continues
		}
	}
	if p.bootstrap && depth < maxBootstrapDepth {
		if next := planFor(goos, look); next.titleKey != p.titleKey && len(next.steps) > 0 {
			ran = runPlan(env, goos, look, run, next, depth+1) || ran
		}
	}
	return ran
}

// maxBootstrapDepth caps the install-a-manager-first chain at one
// round (no manager bootstraps another manager in practice).
const maxBootstrapDepth = 1

// printCommandsNonTTY is the never-block path: the exact actionable
// commands instead of any prompt — for a bootstrap plan the follow-up
// install commands are printed too.
func printCommandsNonTTY(out io.Writer, goos string, look Looker, p plan) {
	say(out, i18n.T("deps.nontty_note"))
	printPlanCommands(out, p)
	if p.bootstrap {
		if next := planFor(goos, look); next.titleKey != p.titleKey {
			printPlanCommands(out, next)
		}
	}
}

// printPlanCommands renders one plan's commands (and any manual lines)
// as indented, copy-paste-ready text.
func printPlanCommands(out io.Writer, p plan) {
	for _, st := range p.steps {
		say(out, "  "+st.display())
	}
	for _, key := range p.manual {
		say(out, "  "+i18n.T(key))
	}
}

// printManual renders the display-only manual-install instructions
// (no package manager detected on Linux; unknown platforms).
func printManual(out io.Writer, p plan) {
	say(out, i18n.T("deps.manual_title"))
	printPlanCommands(out, p)
}

// say writes one line to out; pre-TUI console output is best-effort —
// a write error never aborts startup.
func say(out io.Writer, line string) {
	_, _ = fmt.Fprintln(out, line)
}

// realRunner executes one command as a DIRECT child process with
// stdin/stdout/stderr passthrough (sudo's password prompt, winget's
// progress and the installers' own output stay on the user's
// terminal). No name-based process operations anywhere — the PID rule.
func realRunner(bin string, args []string) error {
	cmd := exec.Command(bin, args...) //nolint:gosec // argv is a compiled-in installer command, never request input
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
