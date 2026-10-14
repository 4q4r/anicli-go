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
	// Offset is the measured offset in seconds (>0 trim, <0 pad).
	Offset float64
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

	// Stage B: decode → byte-shift → m4a per donor (ALL donors, a
	// zero-offset one included — one mkvmerge pass carries every
	// track).
	var audio []AssembleAudio
	for i, don := range job.Donors {
		wav := tempSibling(job.Output, fmt.Sprintf(".donor%d.tmp.wav", i))
		shifted := wav + ".shifted"
		m4a := tempSibling(job.Output, fmt.Sprintf(".donor%d.tmp.m4a", i))
		temps = append(temps, wav, shifted, m4a)

		if _, _, err := d.runOut(ctx, d.ffmpeg, audsync.DecodeWAVArgs(don.Source, wav)); err != nil {
			return fmt.Errorf("download: sync stage B (%s): decode: %w", don.Dub, err)
		}
		frames := audsync.FramesFor(don.Offset, audsync.PipelineRate)
		if err := audsync.ShiftWAVFile(wav, shifted, frames); err != nil {
			return fmt.Errorf("download: sync stage B (%s): shift: %w", don.Dub, err)
		}
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
	// FIRST shifted donor (trim → ≈0, pad → ≈|offset|).
	expectedLead := syncExpectedLead(job.Donors)
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

// syncExpectedLead resolves the verification's expected head: the
// first donor with a non-zero offset decides (trim → 0, pad →
// |offset|); an all-zero set expects ≈0.
func syncExpectedLead(donors []SyncDonor) time.Duration {
	for _, don := range donors {
		if don.Offset == 0 {
			continue
		}
		if don.Offset < 0 {
			return secondsToDuration(-don.Offset)
		}
		return 0
	}
	return 0
}

// secondsToDuration converts float seconds to a time.Duration
// half-up (never time.Duration(f)'s truncation drift).
func secondsToDuration(s float64) time.Duration {
	return time.Duration(s*float64(time.Second) + 0.5)
}
