package download

// PR165b T8: VerifyAssembled — the delivery invariant of every
// synchronized file (plan rev 3): the assembled container MUST have
// audible sound at 45 s (the owner's «половина без звука» root-cause
// 2) AND a leading-silence head matching the applied shift's geometry
// (trim → ≈0, pad → ≈|offset|) within ±1 s. Any failure is a
// *ErrSilentOutput and the episode FAILS — a silent or half-silent
// file never ships (the caller removes it).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/audsync"
)

// The verification measurement windows (the owner's pins).
const (
	// LeadSilenceWindow is how deep the lead-geometry check looks.
	LeadSilenceWindow = 15 * time.Second
	// MaxLeadDeviation is the head-geometry tolerance.
	MaxLeadDeviation = 1 * time.Second
	// RMSWindow is the RMS sampling window at RMSAt.
	RMSWindow = 5 * time.Second
	// RMSAt is where the audible-sound check samples.
	RMSAt = 45 * time.Second
)

// ErrSilentOutput reports an assembled file that failed delivery
// verification: the caller removes the file and fails the episode.
type ErrSilentOutput struct {
	Path    string
	Check   string // "rms@45s" | "lead"
	Details string // measured vs expected plus one stderr line
}

// Error implements error.
func (e *ErrSilentOutput) Error() string {
	return fmt.Sprintf("download: assembled file %s failed verification (%s): %s", e.Path, e.Check, e.Details)
}

// VerifyAssembled runs the two checks against the assembled file.
// expectedLead: a trim shift expects ≈0 head silence, a pad shift
// expects ≈|offset|.
func (d *Downloader) VerifyAssembled(ctx context.Context, path string, expectedLead time.Duration) error {
	// 1. The audible-sound floor at 45 s (first-audio track).
	rmsArgs := []string{"-hide_banner", "-i", path,
		"-map", "0:a:0",
		"-ss", strconv.FormatFloat(RMSAt.Seconds(), 'f', 3, 64),
		"-t", strconv.FormatFloat(RMSWindow.Seconds(), 'f', 3, 64),
		"-f", "s16le", "-ar", "8000", "-ac", "1", "pipe:"}
	pcm, stderr, err := d.runOut(ctx, d.ffmpeg, rmsArgs)
	if err != nil {
		return &ErrSilentOutput{Path: path, Check: "rms@45s", Details: "probe failed: " + oneLine(stderr, err)}
	}
	if rms := audsync.RMSOfPCM(pcm); rms < audsync.MinRMS {
		return &ErrSilentOutput{Path: path, Check: "rms@45s",
			Details: fmt.Sprintf("rms %.0f < %.0f (s16)", rms, audsync.MinRMS)}
	}

	// 2. The head geometry against silencedetect's stderr.
	leadArgs := []string{"-hide_banner", "-i", path,
		"-map", "0:a:0",
		"-t", strconv.FormatFloat(LeadSilenceWindow.Seconds(), 'f', 3, 64),
		"-af", "silencedetect=noise=-50dB:d=0.2",
		"-f", "null", "-"}
	leadStderr, err := d.run(ctx, d.ffmpeg, leadArgs)
	if err != nil {
		return &ErrSilentOutput{Path: path, Check: "lead", Details: "probe failed: " + oneLine(leadStderr, err)}
	}
	lead := parseLeadSilence(leadStderr)
	deviation := lead - expectedLead
	if deviation < 0 {
		deviation = -deviation
	}
	if deviation > MaxLeadDeviation {
		return &ErrSilentOutput{Path: path, Check: "lead",
			Details: fmt.Sprintf("lead %v, expected %v (±%v)", lead, expectedLead, MaxLeadDeviation)}
	}
	return nil
}

// parseLeadSilence reads the leading-silence duration from
// silencedetect's stderr: the FIRST silence that starts at ≈0
// (≤ 0.05 s) contributes its silence_end; any other silence shape
// means the head is audible from the start → 0.
func parseLeadSilence(stderr string) time.Duration {
	const headStartMax = 0.05
	lines := strings.Split(stderr, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "silence_start:") {
			continue
		}
		_, value, found := strings.Cut(line, "silence_start:")
		if !found {
			continue
		}
		start, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || start > headStartMax {
			return 0 // the head is audible (or unreadable — no claim)
		}
		// Find THIS silence's end after it.
		for _, tail := range lines[i+1:] {
			if !strings.Contains(tail, "silence_end:") {
				continue
			}
			_, value, found := strings.Cut(tail, "silence_end:")
			if !found {
				continue
			}
			// "silence_end: 8.123 | silence_duration: 8.123" — the
			// duration column follows the pipe.
			if pipe := strings.IndexByte(value, '|'); pipe >= 0 {
				value = value[:pipe]
			}
			end, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return 0
			}
			return time.Duration(end * float64(time.Second))
		}
		return 0 // unterminated silence runs past the window: no claim
	}
	return 0
}
