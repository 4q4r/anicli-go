// Package audsync implements the PR173 dub audio auto-sync v2: the
// owner's reference algorithm («подгонка в идеал», measured 0.00 ms
// residual). One REFERENCE per episode is picked from the video's own
// streams — subtitles first (ASS dialogue intervals), then a
// Japanese-tagged audio track, then any other embedded original track
// — and each dub (the DONOR) is matched by full-length
// cross-correlation of 50 Hz vocal envelopes. The application is the
// script's closed loop: shift the decoded pipeline WAV by
// lag + AAC_PRIMING, re-measure the shifted result, and correct once
// before the m4a encode.
//
// Detection — the reference script .sdd/pr173-sync-v2-reference.py,
// ported verbatim (the math is pinned by golden tests against
// scipy-computed values). Application — the owner's rev-3 mkvmerge
// architecture (video solo → donor decode 48 kHz stereo WAV →
// byte-exact PCM shift → isolated m4a → mkvmerge assembly → RMS/lead
// verification) is unchanged from PR165b.
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

// RefKind names which leg of the video fed the reference envelope.
type RefKind int

const (
	// RefSubs — the ASS dialogue track (the owner's ideal reference).
	RefSubs RefKind = iota
	// RefAudio — an embedded original audio track (japanese-tagged
	// preferred, then any other original).
	RefAudio
)

// String renders the kind for logs and notes.
func (k RefKind) String() string {
	if k == RefAudio {
		return "audio"
	}
	return "subs"
}

// Reference is one episode's decoded reference: which stream kind won
// the hierarchy, the fixed 72000-bin envelope (the sub_norm for
// RefSubs, the vocal envelope for RefAudio) and the lag band the
// correlation searches.
type Reference struct {
	Kind     RefKind
	Envelope []float64
	// MinLag/MaxLag bound the lag search in seconds: the script's
	// −3…+20 for subtitles, −60…+60 for audio references.
	MinLag, MaxLag float64
}

// SyncOffset is one donor's measured verdict. The zero value is
// «already in sync». Seconds > 0 — the donor lags the reference (trim
// its head); < 0 — it leads (pad).
type SyncOffset struct {
	// Seconds is the measured lag at envelope resolution (50 Hz —
	// 20 ms bins).
	Seconds float64
}

var (
	// ErrNoConfidence reports that a donor could not be measured — a
	// silent donor envelope or a failed correlation. The download
	// proceeds unsynchronized.
	ErrNoConfidence = errors.New("audsync: no confident offset")
	// ErrNoReference reports that no leg of the video's own streams
	// yields a usable reference (no usable subtitles, every audio
	// candidate silent, or the probe failed). The download proceeds
	// unsynchronized.
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
	// Budget bounds the probe/extraction/correlation legs of ONE
	// analysis call; ≤ 0 → DefaultBudget.
	Budget time.Duration
	// DecodeBudget bounds ONE whole-source decode (the reference
	// audio decode, the donor decode and the closed loop's local
	// re-decodes); ≤ 0 → DefaultDecodeBudget.
	DecodeBudget time.Duration
	// Exec is the command seam; nil installs realExec.
	Exec execFunc
}

// Analyzer runs the v2 detection pipeline: one Reference per episode,
// one Offset per donor, one ClosedLoop per donor's pipeline WAV.
type Analyzer struct {
	ffmpeg       string
	ffprobe      string
	budget       time.Duration
	decodeBudget time.Duration
	exec         execFunc
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
	if opts.DecodeBudget <= 0 {
		opts.DecodeBudget = DefaultDecodeBudget
	}
	if opts.Exec == nil {
		opts.Exec = realExec
	}
	return &Analyzer{
		ffmpeg:       opts.FFmpeg,
		ffprobe:      opts.FFprobe,
		budget:       opts.Budget,
		decodeBudget: opts.DecodeBudget,
		exec:         opts.Exec,
	}
}

// Detection constants — the owner's reference script
// (.sdd/pr173-sync-v2-reference.py) verbatim unless noted.
const (
	// SampleRate is the ANALYSIS decode rate (8 kHz, STEREO s16le in
	// v2 — the mid/side separation needs both channels).
	SampleRate = 8000
	// PipelineRate is the PIPELINE decode rate (48 kHz stereo s16le —
	// the owner's forced shape). A different argv than the analysis
	// decode; never mix the two.
	PipelineRate = 48000
	// MinRMS is the assembled-file RMS@45s verification floor (s16
	// scale) — the delivery invariant reads it.
	MinRMS = 100.0
	// DefaultBudget is the per-analysis budget (probes + extraction +
	// correlation).
	DefaultBudget = 15 * time.Second
	// DefaultDecodeBudget bounds ONE whole-source decode: a 24-min
	// episode decodes in 2–4 s at 8 kHz, so 120 s covers 2-h
	// episodes and slow starts while a hung source can no longer
	// hold the caller indefinitely. The reference hierarchy's
	// candidate decodes share ONE such budget per Reference call.
	DefaultDecodeBudget = 120 * time.Second
)

// The v2 algorithm constants (the script verbatim unless noted).
const (
	// EnvRate is the envelope resolution: 50 bins per second.
	EnvRate = 50
	// MaxEnvSec is the envelope canvas length (seconds).
	MaxEnvSec = 1440
	// EnvBins is the fixed envelope canvas: MaxEnvSec × EnvRate.
	EnvBins = MaxEnvSec * EnvRate
	// EnvStep is the analysis-rate samples per envelope bin
	// (8000 / EnvRate — 0.2 s of audio per bin).
	EnvStep = 8000 / EnvRate
	// AACPrimingSec is the AAC encoder delay the m4a encode inserts
	// after the byte shift (1024 samples at 48 kHz). mkvmerge does
	// not compensate it, so every v2 shift over-trims by exactly
	// this much and the encode puts it back — the owner's
	// empirically derived constant, load-bearing for the 0 ms
	// residual.
	AACPrimingSec = 1024.0 / 48000.0
	// SubLagMin/SubLagMax bound the lag search against a SUBTITLE
	// reference (the script's measure_lag defaults: −3…+20 s).
	SubLagMin = -3.0
	SubLagMax = 20.0
	// AudioLagMin/AudioLagMax bound the lag search against an AUDIO
	// reference. The script only shows the subtitle band; an
	// original-audio reference is trusted the v1 window geometry's
	// ±60 s misalignment budget (the brief's extension — the widest
	// honest band for same-content tracks).
	AudioLagMin = -60.0
	AudioLagMax = 60.0
	// ResidualLagWindow bounds the closed-loop re-measurement (±1 s —
	// the script's correction pass).
	ResidualLagWindow = 1.0
	// ResidualTolerance is the closed-loop correction trigger: a
	// residual beyond ±15 ms after the priming compensation means
	// the measured lag was off and one correction pass runs.
	ResidualTolerance = 0.015
	// MinSubIntervals is the usable-dialogue floor for a subtitle
	// reference: an ASS track with fewer usable intervals cannot pin
	// a 50 Hz lag over an episode and falls through the hierarchy
	// (the script takes whatever it finds; the port adds the floor
	// the brief mandates — 10 sparse lines carry no anchor).
	MinSubIntervals = 10
	// MinEnvBins is the envelope-domain silence gate: an envelope
	// with fewer active (non-zero) bins than one second of vocal
	// energy carries no structure and is treated as silent (the
	// translation of v1's MinRMS philosophy into the envelope
	// domain, per the brief).
	MinEnvBins = EnvRate
)
