// Package download merges video/audio streams into local files with
// ffmpeg, manages background download queues with bounded concurrency,
// and maintains the per-title offline index
// (.anicli_offline_index.json) ported from anicli-py
// anicli/core/downloader.py and anicli/core/offline_index.py.
package download

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TempSuffix marks in-progress downloads; only finalized files carry
// the final name.
const TempSuffix = ".part"

// Progress is one honest progress sample of a running ffmpeg pass
// (PR174): the output time written so far, against the probed input
// duration when one was measurable. TotalSec 0 = unknown — the bar
// stays indeterminate, never a fabricated fraction.
type Progress struct {
	// TimeSec is the ffmpeg out_time (seconds of output written).
	TimeSec float64
	// TotalSec is the probed input duration; 0 when the probe failed.
	TotalSec float64
}

// Fraction renders the sample's completion ratio; 0 when the total
// is unknown (indeterminate).
func (p Progress) Fraction() float64 {
	if p.TotalSec <= 0 {
		return 0
	}
	if p.TimeSec >= p.TotalSec {
		return 1
	}
	return p.TimeSec / p.TotalSec
}

// ErrBinaryNotFound reports a missing ffmpeg binary (fail-loud typed
// error; wraps exec.ErrNotFound).
type ErrBinaryNotFound struct {
	// Bin is the binary that was requested.
	Bin string
}

// Error implements error.
func (e *ErrBinaryNotFound) Error() string {
	return fmt.Sprintf("download: required binary not found: %s", e.Bin)
}

// Options customizes the Downloader.
type Options struct {
	// FFmpeg is the ffmpeg binary path (default "ffmpeg").
	FFmpeg string
	// MKVMerge is the mkvmerge binary path (default "mkvmerge") —
	// the PR165b sync-assembly muxer.
	MKVMerge string
	// Logger is the diagnostics sink (mkvmerge's exit-1 warnings);
	// nil degrades to a discard logger.
	Logger *slog.Logger
}

// CommandInput is one download request.
type CommandInput struct {
	// Mode selects the download shape; "" behaves as ModeVideoAudio
	// (the default, as today).
	Mode Mode
	// Video is the video source (URL or local path).
	Video contracts.VideoSource
	// Audio is an optional separate audio source; equal URLs are
	// treated as absent (python guard). Audio-only mode consumes it
	// as THE media source.
	Audio *contracts.VideoSource
	// Tracks are the marked audio inputs of the multi-audio mux
	// (video+аудио mode with a non-empty set); empty keeps today's
	// single-audio semantics.
	Tracks []AudioTrack
	// ChaptersFile embeds FFMETADATA chapters into the output
	// (FEATURE E: offline playback never re-looks-up skip times).
	ChaptersFile string
	// OutputPath is the final file destination.
	OutputPath string
	// Title feeds the container metadata title (python default
	// "Эпизод").
	Title string
}

// Command is the constructed ffmpeg invocation.
type Command struct {
	// Args are the ffmpeg arguments (without the binary).
	Args []string
	// OutputPath is the final destination.
	OutputPath string
	// TempPath is the in-progress file, renamed on success.
	TempPath string
}

// BuildCommand renders the ffmpeg argv. The default shape (no mode,
// no tracks) is the python download_episode command construction:
// base flags, per-input headers, video and optional audio inputs,
// optional chapters input with -map_metadata, stream copy and title
// metadata. The PR165 modes dispatch to the research templates:
// video + marked audio set → the multi-input mux, video-only and
// audio-only the §2.2/§2.3 shapes. The output is the .part temp file.
func BuildCommand(in CommandInput) Command {
	switch {
	case in.Mode == ModeVideoOnly:
		return Command{Args: buildVideoOnlyArgs(in), OutputPath: in.OutputPath, TempPath: TempPathFor(in.OutputPath)}
	case in.Mode == ModeAudioOnly:
		return Command{Args: buildAudioOnlyArgs(in), OutputPath: in.OutputPath, TempPath: TempPathFor(in.OutputPath)}
	case len(in.Tracks) > 0:
		return Command{Args: buildMuxArgs(in, in.Tracks), OutputPath: in.OutputPath, TempPath: TempPathFor(in.OutputPath)}
	}

	out := in.OutputPath
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}

	args = append(args, headerArgs(in.Video.Headers)...)
	args = append(args, "-i", in.Video.URL)

	audioIdx := 0
	if in.Audio != nil && in.Audio.URL != in.Video.URL {
		args = append(args, headerArgs(in.Audio.Headers)...)
		args = append(args, "-i", in.Audio.URL)
		audioIdx = 1
	}

	if in.ChaptersFile != "" {
		if _, err := os.Stat(in.ChaptersFile); err == nil {
			args = append(args, "-i", in.ChaptersFile)
			args = append(args, "-map_metadata", fmt.Sprintf("%d", audioIdx+1))
		}
	}

	args = append(args, "-map", "0:v", "-map", fmt.Sprintf("%d:a", audioIdx))
	args = append(args, "-c", "copy")

	title := in.Title
	if title == "" {
		title = "Эпизод"
	}
	args = append(args, "-metadata", "title="+title)

	temp := TempPathFor(out)
	args = append(args, temp)
	return Command{Args: args, OutputPath: out, TempPath: temp}
}

// TempPathFor derives the in-progress sibling of the final output.
// The extension is preserved (final.part.mp4, not final.mp4.part)
// because ffmpeg infers the container from the output extension.
func TempPathFor(out string) string {
	ext := filepath.Ext(out)
	if ext == "" {
		return out + TempSuffix
	}
	return strings.TrimSuffix(out, ext) + TempSuffix + ext
}

// headerArgs renders per-input HTTP headers as ONE -headers pair:
// ffmpeg honors only the LAST -headers argument, so the python shape
// (one pair per header) silently dropped every header but the final
// one — all headers are CRLF-joined inside a single value instead.
// CR/LF is stripped from keys and values first: the value is a
// newline-joined blob, and an embedded CRLF would smuggle extra header
// lines (or whole extra requests) into the HTTP exchange.
func headerArgs(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var joined strings.Builder
	for _, k := range keys {
		joined.WriteString(stripCRLF(k))
		joined.WriteString(": ")
		joined.WriteString(stripCRLF(headers[k]))
		joined.WriteString("\r\n")
	}
	return []string{"-headers", joined.String()}
}

// stripCRLF removes CR and LF bytes from one header key/value.
func stripCRLF(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// runner executes ffmpeg; swapped by tests.
type runner func(ctx context.Context, bin string, args []string) (stderr string, err error)

// Downloader merges streams into local files with an atomic finalize.
type Downloader struct {
	ffmpeg string
	// mkvmerge is the sync-assembly muxer (PR165b).
	mkvmerge string
	// log is the optional diagnostics sink (mkvmerge warnings).
	log *slog.Logger
	// run executes the ffmpeg command; injectable for tests.
	run runner
	// runOut executes ffmpeg and captures STDOUT (the verification
	// RMS pipe); injectable for tests.
	runOut func(ctx context.Context, bin string, args []string) (stdout []byte, stderr string, err error)
	// runMKV executes mkvmerge and reports its exit-code contract;
	// injectable for tests.
	runMKV func(ctx context.Context, bin string, args []string) (code int, stderr string, err error)
	// mkvVersion probes `mkvmerge --version`; injectable for tests.
	mkvVersion func(ctx context.Context, bin string) (string, error)
	// runProgress executes ffmpeg with a live stdout pipe, feeding
	// each `-progress` line to onLine (the PR174 detailed pass);
	// injectable for tests.
	runProgress func(ctx context.Context, bin string, args []string, onLine func(string)) error
	// probeDuration measures the input's format duration via ffprobe
	// (the detailed pass denominator); injectable for tests.
	probeDuration func(ctx context.Context, src contracts.VideoSource) (float64, error)
}

// New builds the Downloader.
func New(opts Options) *Downloader {
	if opts.FFmpeg == "" {
		opts.FFmpeg = "ffmpeg"
	}
	if opts.MKVMerge == "" {
		opts.MKVMerge = "mkvmerge"
	}
	d := &Downloader{ffmpeg: opts.FFmpeg, mkvmerge: opts.MKVMerge, log: opts.Logger}
	d.run = d.runFFmpeg
	d.runOut = d.realRunOut
	d.runMKV = d.realRunMKV
	d.mkvVersion = d.realMKVMergeVersion
	d.runProgress = d.realRunProgress
	d.probeDuration = d.realProbeDuration
	return d
}

// realRunOut executes ffmpeg capturing stdout (the verification
// pipe), LookPath fail-loud, context cancellation killing the child.
func (d *Downloader) realRunOut(ctx context.Context, bin string, args []string) ([]byte, string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, "", &ErrBinaryNotFound{Bin: bin}
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin/args are config-derived, not request input
	var stderr errBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return out, stderr.String(), ctx.Err()
		}
		return out, stderr.String(), fmt.Errorf("%s: %s", bin, oneLine(stderr.String(), err))
	}
	return out, stderr.String(), nil
}

// Download runs the merge: ffmpeg writes OutputPath+".part" and the
// file is atomically renamed on success. Failures and cancellations
// remove the temp file and surface the error (python wrote in place;
// the atomic finalize is the PR9 hardening). PR174 S4 tolerance: a
// rename ENOENT while the final file already exists with a nonzero
// size settles as SUCCESS — another writer delivered the same file
// first and the file IS the deliverable; the single-flight guard
// makes the branch unreachable in-product, this is defense-in-depth
// against ever failing a healthy episode.
func (d *Downloader) Download(ctx context.Context, in CommandInput) error {
	if err := os.MkdirAll(filepath.Dir(in.OutputPath), 0o750); err != nil {
		return fmt.Errorf("download: create output dir: %w", err)
	}

	cmd := BuildCommand(in)
	if _, err := d.run(ctx, d.ffmpeg, cmd.Args); err != nil {
		_ = os.Remove(cmd.TempPath)
		return fmt.Errorf("download: ffmpeg: %w", err)
	}
	return d.finalize(cmd)
}

// finalize renames the temp file onto the output, tolerating the S4
// race shape: rename ENOENT + a nonzero-size final already in place.
func (d *Downloader) finalize(cmd Command) error {
	if err := os.Rename(cmd.TempPath, cmd.OutputPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if fi, statErr := os.Stat(cmd.OutputPath); statErr == nil && fi.Size() > 0 {
				d.warnf("download: finalize race on %s: the final file already exists (size %d) — keeping it",
					cmd.OutputPath, fi.Size())
				return nil
			}
		}
		_ = os.Remove(cmd.TempPath)
		return fmt.Errorf("download: finalize %s: %w", cmd.OutputPath, err)
	}
	return nil
}

// warnf logs through the optional diagnostics sink; nil degrades to a
// discard logger (never stderr).
func (d *Downloader) warnf(format string, args ...any) {
	if d.log == nil {
		return
	}
	d.log.Warn(fmt.Sprintf(format, args...))
}

// DownloadDetailed runs the merge with honest progress (PR174): the
// same command as Download plus ffmpeg's `-progress pipe:1 -nostats`
// feed, reporting out_time against the probed input duration. A
// failed duration probe keeps the total at 0 — the samples stay
// indeterminate, never fabricated. The finalize tolerance applies
// here exactly as in Download.
func (d *Downloader) DownloadDetailed(ctx context.Context, in CommandInput, onProgress func(Progress)) error {
	if err := os.MkdirAll(filepath.Dir(in.OutputPath), 0o750); err != nil {
		return fmt.Errorf("download: create output dir: %w", err)
	}
	total := 0.0
	if d.probeDuration != nil {
		if secs, perr := d.probeDuration(ctx, in.Video); perr == nil && secs > 0 {
			total = secs
		} else if perr != nil {
			d.warnf("download: duration probe failed; progress stays indeterminate: %v", perr)
		}
	}
	cmd := BuildCommand(in)
	// The output argument is last in every shape (plain, mux,
	// video-only, audio-only): the progress pair slots in right
	// before it.
	args := make([]string, 0, len(cmd.Args)+2)
	args = append(args, cmd.Args[:len(cmd.Args)-1]...)
	args = append(args, "-progress", "pipe:1", "-nostats", cmd.TempPath)
	if err := d.runProgress(ctx, d.ffmpeg, args, func(line string) {
		if secs, ok := parseOutTime(line); ok {
			if onProgress != nil {
				onProgress(Progress{TimeSec: secs, TotalSec: total})
			}
		}
	}); err != nil {
		_ = os.Remove(cmd.TempPath)
		return fmt.Errorf("download: ffmpeg: %w", err)
	}
	return d.finalize(cmd)
}

// parseOutTime extracts the seconds of one `-progress` line: the
// out_time=HH:MM:SS.microseconds form (unambiguous) with the
// out_time_ms= fallback (a long-standing ffmpeg quirk: the _ms
// suffix actually reports MICROseconds).
func parseOutTime(line string) (float64, bool) {
	if v, ok := strings.CutPrefix(line, "out_time="); ok {
		v = strings.TrimSpace(v)
		if v == "N/A" {
			return 0, false
		}
		// HH:MM:SS.fraction
		parts := strings.Split(v, ":")
		if len(parts) != 3 {
			return 0, false
		}
		h, err1 := strconv.ParseFloat(parts[0], 64)
		m, err2 := strconv.ParseFloat(parts[1], 64)
		s, err3 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return 0, false
		}
		return h*3600 + m*60 + s, true
	}
	if v, ok := strings.CutPrefix(line, "out_time_ms="); ok {
		micros, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || micros < 0 {
			return 0, false
		}
		return micros / 1e6, true
	}
	return 0, false
}

// realRunProgress executes ffmpeg streaming its `-progress` stdout
// pipe line by line; LookPath fail-loud, CRLF-stripped lines, context
// cancellation kills the child (the runFFmpeg contract).
func (d *Downloader) realRunProgress(ctx context.Context, bin string, args []string, onLine func(string)) error {
	if _, err := exec.LookPath(bin); err != nil {
		return &ErrBinaryNotFound{Bin: bin}
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin/args are config-derived, not request input
	var stderr errBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for scan.Scan() {
		if onLine != nil {
			onLine(strings.TrimRight(scan.Text(), "\r"))
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		// HIGH-2 (fix-round): the child's REAL exit status decides,
		// not the context state. Go's exec injects ctx.Err() into
		// Wait when the cancellation killed an UNREAPED process —
		// including one that already exited successfully (the
		// scanner-drain window: the kill on a zombie "succeeds").
		// A cancellation landing after a successful exit must never
		// turn the COMPLETED pass into an error (the caller would
		// delete the finished file); the success status is the truth.
		if cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %s", bin, oneLine(stderr.String(), waitErr))
	}
	return nil
}

// realProbeDuration measures the input's format duration via
// ffprobe (the same binary derivation as ProbeCodec). ffprobe prints
// the duration to STDOUT — the probe reads the stdout-capturing seam
// (fix-round CR-1: the stderr-capturing run seam read the empty pipe
// and every real probe came back unparsable, leaving the progress
// bars indeterminate in production).
func (d *Downloader) realProbeDuration(ctx context.Context, src contracts.VideoSource) (float64, error) {
	args := []string{
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
	}
	args = append(args, headerArgs(src.Headers)...)
	args = append(args, src.URL)
	out, _, err := d.runOut(ctx, d.probeBin(), args)
	if err != nil {
		return 0, fmt.Errorf("download: probe duration: %w", err)
	}
	secs, perr := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if perr != nil || secs <= 0 {
		return 0, fmt.Errorf("download: probe duration: unparsable %q", string(out))
	}
	return secs, nil
}

// runFFmpeg executes ffmpeg with LookPath fail-loud semantics and
// CRLF-terminated stderr capture; context cancellation kills it.
func (d *Downloader) runFFmpeg(ctx context.Context, bin string, args []string) (string, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", &ErrBinaryNotFound{Bin: bin}
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin/args are config-derived, not request input
	var stderr errBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stderr.String(), ctx.Err()
		}
		return stderr.String(), fmt.Errorf("%s: %s", bin, oneLine(stderr.String(), err))
	}
	return stderr.String(), nil
}

// errBuffer is a plain byte sink for stderr.
type errBuffer struct {
	data []byte
}

func (b *errBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *errBuffer) String() string { return string(b.data) }

// oneLine compresses stderr for error wrapping.
func oneLine(stderr string, err error) string {
	trimmed := ""
	for _, line := range strings.Split(stderr, "\n") {
		if line != "" {
			trimmed = line
		}
	}
	if trimmed == "" {
		return err.Error()
	}
	return trimmed
}
