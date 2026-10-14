package download

// PR165b T7: the mkvmerge assembler — the owner's verbatim rev-3
// shape, generalized to N audio tracks:
//
//	mkvmerge -o out.part.mkv --no-audio video.tmp.mkv
//	         [--language 0:rus --track-name 0:"<dub>" (--default-track-flag|--default-track) 0:yes] dub.m4a …
//	         [--chapters chapters.simple] --title "…"
//
// Per-input options sit strictly BEFORE their input; the argv goes to
// the exec seam directly (no shell quoting: "0:<name>" is ONE argv
// element). The default-disposition flag is VERSION-GATED (owner
// order): mkvtoolnix ≥ 65 takes --default-track-flag (the rename
// shipped in v65.0.0, 2022-02 — upstream announcement; the old name
// stayed recognized), < 65 takes the legacy --default-track; an
// unparsable `mkvmerge --version` is a TYPED error — the assembly
// never guesses the flag.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// mkvmergeDefaultTrackRename is the mkvtoolnix release that renamed
// --default-track to --default-track-flag (v65.0.0, 2022-02-06; the
// upstream announcement adds that the old name stays recognized).
const mkvmergeDefaultTrackRename = 65

// AssembleAudio is one donor m4a of the assembly (the Stage B
// product).
type AssembleAudio struct {
	// Path is the local .m4a file.
	Path string
	// Lang is the ISO 639-2 language ("" omits --language).
	Lang string
	// Title is the human-readable track name (the dub name; ""
	// omits --track-name).
	Title string
	// Default marks the default disposition.
	Default bool
}

// AssembleInput is one mkvmerge assembly.
type AssembleInput struct {
	// VideoPath is the intermediate video-only .mkv (Stage A).
	VideoPath string
	// AudioFiles is ≥1 donor track, in mux order.
	AudioFiles []AssembleAudio
	// ChaptersFile is the mkvmerge simple-chapters file ("" omits the
	// flag).
	ChaptersFile string
	// OutputPath is the final container.
	OutputPath string
	// Title feeds --title ("" renders the python default «Эпизод»).
	Title string
}

// ErrMKVMergeVersion reports a `mkvmerge --version` probe that failed
// or produced an unparsable version: the assembly refuses to guess
// the default-track flag (a wrong flag is a guaranteed exit 2 — a
// typed failure beats a silent one).
type ErrMKVMergeVersion struct {
	// Output is the raw probe output ("" when the probe itself failed).
	Output string
	// Err is the probe execution error, nil for unparsable output.
	Err error
}

// Error implements error.
func (e *ErrMKVMergeVersion) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("download: mkvmerge version probe failed: %v", e.Err)
	}
	return fmt.Sprintf("download: mkvmerge version unparsable (output %q) — refusing to guess the default-track flag", e.Output)
}

// Unwrap exposes the probe execution error.
func (e *ErrMKVMergeVersion) Unwrap() error { return e.Err }

var mkvmergeVersionRe = regexp.MustCompile(`v([0-9]+)\.`)

// parseMKVMergeVersion extracts the major version from
// `mkvmerge --version` output ("mkvmerge v102.0 ('…') 64-bit").
func parseMKVMergeVersion(out string) (int, error) {
	m := mkvmergeVersionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("no v<digits> token")
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	return v, nil
}

// defaultTrackFlagFor resolves the default-disposition flag for an
// installed mkvmerge version (the owner-ordered gate).
func defaultTrackFlagFor(version int) string {
	if version >= mkvmergeDefaultTrackRename {
		return "--default-track-flag"
	}
	return "--default-track"
}

// buildMkvMergeArgs renders the assembly argv. defaultFlag is the
// version-gated disposition flag name (defaultTrackFlagFor).
func buildMkvMergeArgs(in AssembleInput, defaultFlag string) []string {
	args := []string{"-o", in.OutputPath, "--no-audio", in.VideoPath}
	for _, tr := range in.AudioFiles {
		if tr.Lang != "" {
			args = append(args, "--language", "0:"+tr.Lang)
		}
		if tr.Title != "" {
			args = append(args, "--track-name", "0:"+tr.Title)
		}
		disposition := "0:no"
		if tr.Default {
			disposition = "0:yes"
		}
		args = append(args, defaultFlag, disposition, tr.Path)
	}
	if in.ChaptersFile != "" {
		args = append(args, "--chapters", in.ChaptersFile)
	}
	title := in.Title
	if title == "" {
		title = "Эпизод"
	}
	return append(args, "--title", title)
}

// buildVideoSoloArgs renders Stage A: the intermediate video-only
// mkv (no chapters, no title — mkvmerge owns both in Stage C).
func buildVideoSoloArgs(video contracts.VideoSource, outPath string) []string {
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}
	args = append(args, headerArgs(video.Headers)...)
	args = append(args, "-i", video.URL, "-map", "0:v:0", "-c:v", "copy", outPath)
	return args
}

// RunAssemble executes mkvmerge. Exit-code contract: 0 = ok;
// 1 = warnings = SUCCESS (stderr goes to the warn log); ≥2 = typed
// failure carrying one stderr line. The version probe runs first —
// an unparsable version fails typed, before any assembly.
func (d *Downloader) RunAssemble(ctx context.Context, in AssembleInput) error {
	raw, err := d.mkvVersion(ctx, d.mkvmerge)
	if err != nil {
		return &ErrMKVMergeVersion{Err: err}
	}
	version, perr := parseMKVMergeVersion(raw)
	if perr != nil {
		return &ErrMKVMergeVersion{Output: raw}
	}

	args := buildMkvMergeArgs(in, defaultTrackFlagFor(version))
	code, stderr, err := d.runMKV(ctx, d.mkvmerge, args)
	if err != nil {
		return fmt.Errorf("download: mkvmerge: %w", err)
	}
	switch code {
	case 0:
		return nil
	case 1:
		// mkvmerge reports warnings on stderr and exits 1 — the file
		// is good; the diagnostics log carries the details.
		d.logger().Warn("download: mkvmerge assembled with warnings",
			"output", in.OutputPath, "stderr", oneLine(stderr, nil))
		return nil
	default:
		return fmt.Errorf("download: mkvmerge failed (exit %d): %s", code, oneLine(stderr, nil))
	}
}

// logger normalizes the optional diagnostics sink: nil degrades to a
// discard logger (never stderr inside the TUI).
func (d *Downloader) logger() *slog.Logger {
	if d.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.log
}

// ProbeMKVMerge is the exported one-shot probe for the startup check
// (cli wiring): it returns the parsed version of the installed
// mkvmerge. An absent binary or unparsable output is a typed error —
// the sync degrades, never guesses.
func ProbeMKVMerge(ctx context.Context, bin string) (int, error) {
	d := New(Options{MKVMerge: bin})
	out, err := d.realMKVMergeVersion(ctx, bin)
	if err != nil {
		return 0, &ErrMKVMergeVersion{Err: err}
	}
	v, perr := parseMKVMergeVersion(out)
	if perr != nil {
		return 0, &ErrMKVMergeVersion{Output: out}
	}
	return v, nil
}

// realMKVMergeVersion runs `mkvmerge --version` as a direct child.
func (d *Downloader) realMKVMergeVersion(ctx context.Context, bin string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", &ErrBinaryNotFound{Bin: bin}
	}
	out, err := exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // bin is the compiled-in mkvmerge name, never request input
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// realRunMKV executes mkvmerge as a direct child with LookPath
// fail-loud and captured stderr. mkvmerge's exit-code contract is the
// RESULT: an ExitError resolves to its code (1 = warnings, ≥2 =
// failure), not a Go error.
func (d *Downloader) realRunMKV(ctx context.Context, bin string, args []string) (int, string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return 0, "", &ErrBinaryNotFound{Bin: bin}
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin/args are compiled-in program shapes, never request input
	var stderr errBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), stderr.String(), nil
		}
		return 0, stderr.String(), err
	}
	return 0, stderr.String(), nil
}
