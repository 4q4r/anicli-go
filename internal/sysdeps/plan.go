package sysdeps

// The per-OS install plans. One file, no build tags: the OS seam is
// the goos PARAMETER of planFor, so the windows and darwin strategies
// compile — and are unit-tested — on linux too. All command shapes
// were verified against the upstream sources (mpv.io/installation,
// community.chocolatey.org, winget, brew.sh, rpmfusion.org).

import "strings"

// step is one install command. disp, when set, is the copy-paste form
// shown to the user (used when the raw argv is not paste-able verbatim,
// e.g. a shell-expanded URL).
type step struct {
	argv []string
	disp string
}

// display renders the command the user would type.
func (st step) display() string {
	if st.disp != "" {
		return st.disp
	}
	return strings.Join(st.argv, " ")
}

// plan is one offer: a title, the exact commands, an optional manual
// fallback, and the bootstrap flag (the steps install a package
// manager first — after they run, the flow re-plans once).
type plan struct {
	titleKey  string
	steps     []step
	manual    []string // display-only i18n keys (no runnable steps)
	bootstrap bool
}

// planFor dispatches by OS name. Unknown platforms take the Linux
// ladder (a distro package manager may still exist) with the manual
// fallback as the last resort.
func planFor(goos string, look Looker) plan {
	switch goos {
	case "windows":
		return pickWindowsPlan(look)
	case "darwin":
		return pickDarwinPlan(look)
	default:
		return pickLinuxPlan(look)
	}
}

// Package IDs verified upstream:
//   - winget: shinchiro.mpv (the shinchiro build mpv.io lists first;
//     winget adds it to PATH), Gyan.FFmpeg (the Gyan.dev builds
//     ffmpeg.org links);
//   - Chocolatey: mpvio (the package mpv.io's installation page links)
//     and ffmpeg;
//   - Homebrew formulas: mpv, ffmpeg.
const (
	wingetMPVID = "shinchiro.mpv"
	wingetFFID  = "Gyan.FFmpeg"

	// chocoBootstrap is Chocolatey's official install one-liner.
	chocoBootstrap = `Set-ExecutionPolicy Bypass -Scope Process -Force; ` +
		`[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072; ` +
		`iex ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))`

	// brewBootstrap is Homebrew's official install body; bash -c
	// evaluates the $( ) itself, so the argv reproduces the verbatim
	// brew.sh one-liner.
	brewBootstrap = "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

	// rpmFusionFreeRelease enables the RPM Fusion free repo (mpv and
	// the full ffmpeg are not in stock Fedora repos); the $( ) is
	// evaluated by the sh -c runner step, never by this process.
	rpmFusionFreeRelease = "https://download1.rpmfusion.org/free/fedora/rpmfusion-free-release-$(rpm -E %fedora).noarch.rpm"
)

// pickWindowsPlan: winget (ships with Windows 10+), else Chocolatey,
// else the official Chocolatey bootstrap first (winget is a Store app
// with no scriptable CLI installer).
func pickWindowsPlan(look Looker) plan {
	if _, err := look("winget"); err == nil {
		return plan{
			titleKey: "deps.offer_winget",
			steps: []step{
				{argv: []string{"winget", "install", "-e", "--id", wingetMPVID}},
				{argv: []string{"winget", "install", "-e", "--id", wingetFFID}},
			},
		}
	}
	if _, err := look("choco"); err == nil {
		return plan{
			titleKey: "deps.offer_choco",
			steps:    []step{{argv: []string{"choco", "install", "mpvio", "ffmpeg", "-y"}}},
		}
	}
	return plan{
		titleKey:  "deps.offer_bootstrap_choco",
		bootstrap: true,
		steps: []step{{
			argv: []string{"powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", chocoBootstrap},
			disp: `powershell -NoProfile -ExecutionPolicy Bypass -Command "` + chocoBootstrap + `"`,
		}},
	}
}

// pickDarwinPlan: Homebrew, else the official Homebrew bootstrap
// first.
func pickDarwinPlan(look Looker) plan {
	if _, err := look("brew"); err == nil {
		return plan{
			titleKey: "deps.offer_brew",
			steps:    []step{{argv: []string{"brew", "install", "mpv", "ffmpeg"}}},
		}
	}
	return plan{
		titleKey:  "deps.offer_bootstrap_brew",
		bootstrap: true,
		steps: []step{{
			argv: []string{"/bin/bash", "-c", brewBootstrap},
			disp: `/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`,
		}},
	}
}

// pickLinuxPlan walks the distro package managers (first present wins)
// and falls back to per-family manual instructions.
func pickLinuxPlan(look Looker) plan {
	ladder := []struct {
		pm    string
		steps []step
	}{
		{"apt", []step{{argv: []string{"sudo", "apt", "install", "mpv", "ffmpeg"}}}},
		{"dnf", []step{
			{
				argv: []string{"sh", "-c", "sudo dnf install " + rpmFusionFreeRelease},
				disp: "sudo dnf install " + rpmFusionFreeRelease,
			},
			// --exclude=openh264* skips Fedora's stripped openh264
			// builds of mpv/ffmpeg (a poor substitute that blocks the
			// real packages); the raw glob is safe in argv because the
			// runner execs without a shell, and the display form quotes
			// it for a zsh paste.
			{
				argv: []string{"sudo", "dnf", "install", "--exclude=openh264*", "mpv", "ffmpeg"},
				disp: "sudo dnf install --exclude='openh264*' mpv ffmpeg",
			},
		}},
		{"pacman", []step{{argv: []string{"sudo", "pacman", "-S", "mpv", "ffmpeg"}}}},
		{"zypper", []step{{argv: []string{"sudo", "zypper", "install", "mpv", "ffmpeg"}}}},
	}
	for _, entry := range ladder {
		if _, err := look(entry.pm); err == nil {
			return plan{titleKey: "deps.offer_" + entry.pm, steps: entry.steps}
		}
	}
	return plan{
		manual: []string{
			"deps.manual_debian",
			"deps.manual_fedora",
			"deps.manual_arch",
			"deps.manual_suse",
		},
	}
}
