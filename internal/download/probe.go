package download

// PR165: codec probing for the container decision. The container
// rules are enforced in code (mux.go), which needs the codec_name of
// the streams ffmpeg will map — detected via ffprobe of the RESOLVED
// stream URL (the mandate's pinned argv shape), before ffmpeg runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// ffprobeCodecArgs builds the codec probe argv for one resolved
// source: -v error -select_streams <kind>:0 -show_entries
// stream=codec_name -of json, with the source's HTTP headers as
// per-input options ahead of the URL (the same placement the ffmpeg
// builder uses).
func ffprobeCodecArgs(kind string, src contracts.VideoSource) []string {
	args := []string{
		"-v", "error",
		"-select_streams", kind + ":0",
		"-show_entries", "stream=codec_name",
		"-of", "json",
	}
	args = append(args, headerArgs(src.Headers)...)
	args = append(args, src.URL)
	return args
}

// probeStreams is the ffprobe -of json shape (only the fields the
// parser reads).
type probeStreams struct {
	Streams []struct {
		CodecName string `json:"codec_name"`
	} `json:"streams"`
}

// parseCodecName extracts the selected stream's codec_name; a missing
// stream or malformed output degrades to "" (the caller falls back to
// the safe Matroska container).
func parseCodecName(data []byte) string {
	var parsed probeStreams
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	if len(parsed.Streams) == 0 {
		return ""
	}
	return parsed.Streams[0].CodecName
}

// probeBin derives the ffprobe binary from the configured ffmpeg
// binary (169-K — ProbeCodec hardcoded "ffprobe" and ignored
// Options.FFmpeg): a custom ffmpeg location ships its own ffprobe next
// to it, so the probe follows the configured directory; a bare binary
// name keeps PATH resolution (production wires "ffmpeg").
func (d *Downloader) probeBin() string {
	if dir := filepath.Dir(d.ffmpeg); dir != "." {
		return filepath.Join(dir, "ffprobe")
	}
	return "ffprobe"
}

// ProbeCodec returns the codec_name of the source's first stream of
// the given kind ("a" audio, "v" video) via ffprobe. The runner is
// the injectable ffmpeg/ffprobe seam; a missing ffprobe binary or a
// failed probe surfaces as the error (the caller decides the
// fallback — the download itself never fails on a probe).
func (d *Downloader) ProbeCodec(ctx context.Context, kind string, src contracts.VideoSource) (string, error) {
	out, err := d.run(ctx, d.probeBin(), ffprobeCodecArgs(kind, src))
	if err != nil {
		return "", fmt.Errorf("download: probe %s codec: %w", kind, err)
	}
	return parseCodecName([]byte(out)), nil
}
