package download

// PR165b T10: RunSyncJob — the owner's rev-3 two-stage assembly, one
// episode at a time. The analysis (the audsync offsets) happened
// upstream; this pipeline APPLIES them byte-exact and verifies the
// product:
//
//	Stage A  video alone:  ffmpeg -map 0:v:0 -c:v copy video.tmp.mkv
//	Stage B  per donor:    whole 48 kHz stereo WAV decode →
//	                       ShiftWAVFile(round(offset×48000) frames) →
//	                       isolated m4a (aac 256k)
//	Stage C  assembly:     mkvmerge --no-audio video + N×m4a
//	Stage D  verification: VerifyAssembled — a silent or mis-shaped
//	                       file deletes the output and fails the job
//
// Every intermediate lives next to the output and is removed on both
// success and failure; the failure of ANY stage fails the EPISODE
// (typed error) — a silent file never ships.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/an0nx/anicli-go/internal/audsync"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// SyncDonor is one dub track of the assembly with its measured
// offset (0 = no shift — the donor still assembles, plan §1.1).
type SyncDonor struct {
	// Dub is the bare dub name (diagnostics and notes).
	Dub string
	// Source is the resolved donor stream.
	Source contracts.VideoSource
	// Lang/Title are the Matroska track language and name.
	Lang, Title string
	// Default marks the default disposition.
	Default bool
	// Offset is the measured LAG in seconds (>0 trim, <0 pad) — the
	// pipeline adds the AAC priming before moving frames (the v2
	// convention; the closed loop may refine it further). Once the
	// shift is applied, the same field carries the APPLIED shift —
	// the lag plus the AAC priming compensation, exactly what
	// ShiftWAVFile consumed — and the Stage D lead verification
	// (syncExpectedLead) reads it directly.
	Offset float64
	// Measured marks donors the analyzer actually measured. Only
	// measured donors run the closed loop and carry the priming
	// compensation; an unmeasured donor ships byte-identical (a
	// same-provider timeline is in sync by construction).
	Measured bool
}

// DonorSynchronizer is the v2 closed-loop seam (the audsync.Analyzer
// in production): it refines one donor's measured lag against the
// decoded pipeline WAV, leaving shiftedWAV as the final product, and
// returns the applied shift (final lag + AAC priming).
type DonorSynchronizer interface {
	ClosedLoop(ctx context.Context, ref audsync.Reference, rawWAV, shiftedWAV string, measuredLag float64) (float64, error)
}

// SyncJob is one episode's assembly plan.
type SyncJob struct {
	// Video is the resolved video stream.
	Video contracts.VideoSource
	// Donors is ≥1 dub track in mux order.
	Donors []SyncDonor
	// ChaptersFFMeta is the optional FFMETADATA path (converted to
	// the mkvmerge simple format for Stage C).
	ChaptersFFMeta string
	// Output is the FINAL .mkv path (the .part sibling is internal).
	Output string
	// Title feeds the container title.
	Title string
	// SyncRef/SyncLoop enable the v2 closed-loop refinement in
	// Stage B (the episode's reference envelope + the analyzer
	// seam). nil either — the measured offsets apply verbatim (the
	// plain shift).
	SyncRef  *audsync.Reference
	SyncLoop DonorSynchronizer
}

// tempSibling derives a hidden intermediate next to the output.
func tempSibling(output, suffix string) string {
	return filepath.Join(filepath.Dir(output), "."+filepath.Base(output)+suffix)
}

// RunSyncJob executes the four stages. progress reports the honest
// ticks 10 (video ready) / 30 (audio ready) / 90 (assembled) / 100
// (delivered); nil is fine.
func (d *Downloader) RunSyncJob(ctx context.Context, job SyncJob, progress func(float64)) error {
	if len(job.Donors) == 0 {
		return fmt.Errorf("download: sync job %s has no donors", job.Output)
	}
	tick := func(p float64) {
		if progress != nil {
			progress(p)
		}
	}
	outPart := TempPathFor(job.Output)
	var temps []string
	defer func() {
		for _, p := range temps {
			_ = os.Remove(p)
		}
	}()

	// Stage A: the video alone (no chapters, no title — Stage C owns
	// both).
	videoTmp := tempSibling(job.Output, ".video.tmp.mkv")
	temps = append(temps, videoTmp)
	if _, _, err := d.runOut(ctx, d.ffmpeg, buildVideoSoloArgs(job.Video, videoTmp)); err != nil {
		return fmt.Errorf("download: sync stage A: %w", err)
	}
	tick(10)

	// Stage B: decode → byte-shift (the v2 closed loop for measured
	// donors) → m4a per donor (ALL donors, a zero-offset one
	// included — one mkvmerge pass carries every track).
	var audio []AssembleAudio
	finalDonors := make([]SyncDonor, len(job.Donors))
	copy(finalDonors, job.Donors)
	for i, don := range job.Donors {
		wav := tempSibling(job.Output, fmt.Sprintf(".donor%d.tmp.wav", i))
		shifted := wav + ".shifted"
		m4a := tempSibling(job.Output, fmt.Sprintf(".donor%d.tmp.m4a", i))
		temps = append(temps, wav, shifted, m4a)

		if _, _, err := d.runOut(ctx, d.ffmpeg, audsync.DecodeWAVArgs(don.Source, wav)); err != nil {
			return fmt.Errorf("download: sync stage B (%s): decode: %w", don.Dub, err)
		}
		applied := 0.0
		switch {
		case don.Measured && job.SyncRef != nil && job.SyncLoop != nil:
			// The v2 closed loop: measure → shift → re-measure → one
			// correction, leaving `shifted` as the final product. A
			// loop failure degrades to the measured shift (the v1
			// product) — the analysis never fails a download.
			//
			// The fallback re-shifts FROM THE RAW wav below: ClosedLoop
			// performs its initial shift before the re-measure, so
			// `shifted` may already hold a shifted copy — the fresh
			// ShiftWAVFile(wav → shifted) overwrites it wholesale (a
			// duplicate of that initial shift, never a chained second
			// shift of already-shifted PCM).
			applied = don.Offset + audsync.AACPrimingSec
			if v, err := job.SyncLoop.ClosedLoop(ctx, *job.SyncRef, wav, shifted, don.Offset); err == nil {
				applied = v
			} else {
				d.logger().Warn("download: sync closed loop failed; applying the measured shift",
					"donor", don.Dub, "error", err)
				if err := audsync.ShiftWAVFile(wav, shifted, audsync.FramesFor(applied, audsync.PipelineRate)); err != nil {
					return fmt.Errorf("download: sync stage B (%s): shift: %w", don.Dub, err)
				}
			}
		case don.Measured:
			applied = don.Offset + audsync.AACPrimingSec
			if err := audsync.ShiftWAVFile(wav, shifted, audsync.FramesFor(applied, audsync.PipelineRate)); err != nil {
				return fmt.Errorf("download: sync stage B (%s): shift: %w", don.Dub, err)
			}
		default:
			// Unmeasured: the donor ships byte-identical (a plain
			// copy through the same temp path).
			if err := audsync.ShiftWAVFile(wav, shifted, 0); err != nil {
				return fmt.Errorf("download: sync stage B (%s): copy: %w", don.Dub, err)
			}
		}
		finalDonors[i].Offset = applied
		if _, _, err := d.runOut(ctx, d.ffmpeg, audsync.EncodeM4AArgs(shifted, m4a)); err != nil {
			return fmt.Errorf("download: sync stage B (%s): encode: %w", don.Dub, err)
		}
		audio = append(audio, AssembleAudio{Path: m4a, Lang: don.Lang, Title: don.Title, Default: don.Default})
	}
	tick(30)

	// Chapters ride the assembly in the mkvmerge simple format.
	chapters := ""
	if job.ChaptersFFMeta != "" {
		var err error
		if chapters, err = ConvertChaptersToMkv(job.ChaptersFFMeta, filepath.Dir(job.Output)); err != nil {
			return fmt.Errorf("download: sync chapters: %w", err)
		}
		if chapters != "" {
			temps = append(temps, chapters)
		}
	}

	// Stage C: the mkvmerge assembly.
	if err := d.RunAssemble(ctx, AssembleInput{
		VideoPath:    videoTmp,
		AudioFiles:   audio,
		ChaptersFile: chapters,
		OutputPath:   outPart,
		Title:        job.Title,
	}); err != nil {
		_ = os.Remove(outPart)
		return fmt.Errorf("download: sync stage C: %w", err)
	}
	tick(90)

	// Stage D: the delivery invariant — the lead geometry follows the
	// FIRST donor (trim → ≈0, pad → ≈|applied|), from the FINAL
	// offsets (the closed loop may have corrected donors[0]).
	expectedLead := syncExpectedLead(finalDonors)
	if err := d.VerifyAssembled(ctx, outPart, expectedLead); err != nil {
		_ = os.Remove(outPart)
		return fmt.Errorf("download: sync stage D: %w", err)
	}

	if err := os.Rename(outPart, job.Output); err != nil {
		_ = os.Remove(outPart)
		return fmt.Errorf("download: sync finalize %s: %w", job.Output, err)
	}
	tick(100)
	return nil
}

// syncExpectedLead resolves the verification's expected head from
// donors[0] ONLY — that is the track VerifyAssembled measures
// (0:a:0). Offset here is the APPLIED shift (the lag plus the AAC
// priming compensation — exactly what ShiftWAVFile consumed), so the
// lead derives from it directly: a nonnegative applied shift is trim
// geometry (≈0 lead), a negative one pads (≈|Offset|). No second
// priming addition happens here. Unmeasured donors ship untouched —
// no lead. A later donor's pad never sets the expectation for a head
// it does not own (CodeRabbit #4).
func syncExpectedLead(donors []SyncDonor) time.Duration {
	if len(donors) == 0 || !donors[0].Measured {
		return 0
	}
	if donors[0].Offset >= 0 {
		return 0
	}
	return secondsToDuration(-donors[0].Offset)
}

// secondsToDuration converts float seconds to a time.Duration
// half-up (never time.Duration(f)'s truncation drift).
func secondsToDuration(s float64) time.Duration {
	return time.Duration(s*float64(time.Second) + 0.5)
}
