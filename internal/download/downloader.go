// Package download merges video/audio streams into local files with
// ffmpeg, manages background download queues with bounded concurrency,
// and maintains the per-title offline index
// (.anicli_offline_index.json) ported from anicli-py
// anicli/core/downloader.py and anicli/core/offline_index.py.
package download

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TempSuffix marks in-progress downloads; only finalized files carry
// the final name.
const TempSuffix = ".part"

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
}

// CommandInput is one download request.
type CommandInput struct {
	// Video is the video source (URL or local path).
	Video contracts.VideoSource
	// Audio is an optional separate audio source; equal URLs are
	// treated as absent (python guard).
	Audio *contracts.VideoSource
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

// BuildCommand renders the ffmpeg argv (python download_episode
// command construction): base flags, per-input headers, video and
// optional audio inputs, optional chapters input with -map_metadata,
// stream copy and title metadata. The output is the .part temp file.
func BuildCommand(in CommandInput) Command {
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

// headerArgs renders per-input HTTP headers (python _build_headers_args:
// one -headers pair per header, CRLF-terminated).
func headerArgs(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		args = append(args, "-headers", k+": "+headers[k]+"\r\n")
	}
	return args
}

// runner executes ffmpeg; swapped by tests.
type runner func(ctx context.Context, bin string, args []string) (stderr string, err error)

// Downloader merges streams into local files with an atomic finalize.
type Downloader struct {
	ffmpeg string
	// run executes the command; injectable for tests.
	run runner
}

// New builds the Downloader.
func New(opts Options) *Downloader {
	if opts.FFmpeg == "" {
		opts.FFmpeg = "ffmpeg"
	}
	d := &Downloader{ffmpeg: opts.FFmpeg}
	d.run = d.runFFmpeg
	return d
}

// Download runs the merge: ffmpeg writes OutputPath+".part" and the
// file is atomically renamed on success. Failures and cancellations
// remove the temp file and surface the error (python wrote in place;
// the atomic finalize is the PR9 hardening).
func (d *Downloader) Download(ctx context.Context, in CommandInput) error {
	if err := os.MkdirAll(filepath.Dir(in.OutputPath), 0o750); err != nil {
		return fmt.Errorf("download: create output dir: %w", err)
	}

	cmd := BuildCommand(in)
	if _, err := d.run(ctx, d.ffmpeg, cmd.Args); err != nil {
		_ = os.Remove(cmd.TempPath)
		return fmt.Errorf("download: ffmpeg: %w", err)
	}

	if err := os.Rename(cmd.TempPath, cmd.OutputPath); err != nil {
		_ = os.Remove(cmd.TempPath)
		return fmt.Errorf("download: finalize %s: %w", cmd.OutputPath, err)
	}
	return nil
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

// ErrNoOutput reports ffmpeg succeeding without producing the temp
// file (defensive; keeps finalize honest).
var ErrNoOutput = errors.New("download: ffmpeg produced no output file")
