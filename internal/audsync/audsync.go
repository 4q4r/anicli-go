// Package audsync implements the PR165b dub audio auto-sync: detect a
// dub's time offset against the video's own audio by FFT
// cross-correlation of short windows, then apply the offset byte-exact
// to the dub's decoded PCM and assemble the final file with mkvmerge.
//
// Detection — research .sdd/pr165b-me-sync-research.md (ffmpeg-pipe PCM
// 8 kHz mono → gonum FFT cross-correlation over 3 windows → consensus
// ≥2/3 within ±80 ms → median → frame snap → SyncOffset).
// Application — the owner's rev-3 mkvmerge architecture (video solo →
// donor decode 48 kHz stereo WAV → byte-exact PCM shift → isolated
// m4a → mkvmerge assembly → RMS/lead verification).
//
// The analysis never fails a download: an analysis error degrades to
// today's unsynchronized mux plus a log line. Assembly and its
// verification are the opposite — a failure there fails the episode
// (typed error); a silent file never ships.
package audsync

import (
	"errors"
	"path/filepath"
	"time"
)

// SyncOffset is one (video, donor) analysis verdict. The zero value is
// «no confidence» → no shift (research §6.3).
type SyncOffset struct {
	// Seconds is the measured dub offset. >0 — trim the head of the
	// donor; <0 — pad silence. Formatting contract: 3 decimals
	// (17.042).
	Seconds float64
	// Confidence is min(prominence)/MinProminence capped at
	// ConfidenceCap; ≥1 for accepted offsets.
	Confidence float64
	// WindowsAgree is the count of agreeing windows out of the viable
	// ones (2 or 3).
	WindowsAgree int
}

var (
	// ErrNoConfidence reports that no offset passed the
	// RMS/prominence/consensus gates — the download proceeds
	// unsynchronized.
	ErrNoConfidence = errors.New("audsync: no confident offset")
	// ErrNoReference reports that the video's own track is silent or
	// absent — no reference to correlate against.
	ErrNoReference = errors.New("audsync: reference track is silent or absent")
)

// Options customizes the Analyzer.
type Options struct {
	// FFmpeg is the ffmpeg binary (default "ffmpeg").
	FFmpeg string
	// FFprobe is the ffprobe binary; empty derives the SIBLING of
	// FFmpeg (a custom ffmpeg location ships its own ffprobe — the
	// 169-K rule), else "ffprobe".
	FFprobe string
	// Budget bounds ONE pair analysis (Offset); ≤ 0 → DefaultBudget.
	Budget time.Duration
	// Exec is the command seam; nil installs realExec.
	Exec execFunc
}

// Analyzer runs the detection pipeline: one Reference per episode,
// one Offset per donor.
type Analyzer struct {
	ffmpeg  string
	ffprobe string
	budget  time.Duration
	exec    execFunc
}

// New builds the Analyzer.
func New(opts Options) *Analyzer {
	if opts.FFmpeg == "" {
		opts.FFmpeg = "ffmpeg"
	}
	if opts.FFprobe == "" {
		if dir := filepath.Dir(opts.FFmpeg); dir != "." {
			opts.FFprobe = filepath.Join(dir, "ffprobe")
		} else {
			opts.FFprobe = "ffprobe"
		}
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.Exec == nil {
		opts.Exec = realExec
	}
	return &Analyzer{
		ffmpeg:  opts.FFmpeg,
		ffprobe: opts.FFprobe,
		budget:  opts.Budget,
		exec:    opts.Exec,
	}
}

// Detection constants (research §1.4 table, verbatim).
const (
	// SampleRate is the ANALYSIS decode rate (mono s16le).
	SampleRate = 8000
	// PipelineRate is the PIPELINE decode rate (48 kHz stereo s16le —
	// the owner's forced shape). A different argv than the analysis
	// decode; never mix the two.
	PipelineRate = 48000
	// ReferenceDur is the reference window length (seconds).
	ReferenceDur = 60.0
	// SearchDur is the donor search window length (seconds).
	SearchDur = 180.0
	// WindowCount is the number of windows per pair.
	WindowCount = 3
	// MinProminence is the correlation peak gate: peak / mean|corr|.
	MinProminence = 20.0
	// ConsensusTolerance is the inter-window agreement band (seconds).
	ConsensusTolerance = 0.080
	// SnapTolerance is the frame-grid snap band (seconds).
	SnapTolerance = 0.020
	// DefaultFPS is used when r_frame_rate is unreadable.
	DefaultFPS = 24.0
	// MinEpisodeDur gates the whole analysis: shorter episodes skip it.
	MinEpisodeDur = 300.0
	// MinRMS is the silence gate for windows (s16 scale) AND the
	// assembled-file RMS@45s verification floor.
	MinRMS = 100.0
	// ConfidenceCap bounds Confidence.
	ConfidenceCap = 10.0
	// DefaultBudget is the per-pair analysis budget.
	DefaultBudget = 15 * time.Second
)
