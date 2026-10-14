package audsync

// ffprobe argv and parsers: duration (the window-schedule gate) and
// r_frame_rate (the snap grid). An unreadable fps is DefaultFPS —
// never an error; an unreadable duration is, because the schedule
// cannot be built without it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// durationArgs probes the container duration of one source.
func durationArgs(src contracts.VideoSource) []string {
	args := []string{"-v", "error", "-show_entries", "format=duration", "-of", "json"}
	args = append(args, headerBlob(src.Headers)...)
	return append(args, src.URL)
}

// fpsArgs probes the first video stream's r_frame_rate.
func fpsArgs(src contracts.VideoSource) []string {
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate",
		"-of", "json",
	}
	args = append(args, headerBlob(src.Headers)...)
	return append(args, src.URL)
}

type durationProbe struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// parseDuration extracts the container duration (seconds).
func parseDuration(data []byte) (float64, error) {
	var parsed durationProbe
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0, fmt.Errorf("audsync: duration probe: %w", err)
	}
	d, err := strconv.ParseFloat(parsed.Format.Duration, 64)
	if err != nil || d <= 0 {
		return 0, errors.New("audsync: duration probe: unreadable duration")
	}
	return d, nil
}

type fpsProbe struct {
	Streams []struct {
		RFrameRate string `json:"r_frame_rate"`
	} `json:"streams"`
}

// parseFPS extracts the frame rate from an ffmpeg ratio
// ("24000/1001"). Anything unreadable — malformed json, a missing
// stream, a zero denominator — is DefaultFPS with NO error: the snap
// grid is a nicety, never a failure.
func parseFPS(data []byte) (float64, error) {
	var parsed fpsProbe
	if err := json.Unmarshal(data, &parsed); err != nil {
		return DefaultFPS, nil
	}
	if len(parsed.Streams) == 0 {
		return DefaultFPS, nil
	}
	rate := parsed.Streams[0].RFrameRate
	num, den, ok := strings.Cut(rate, "/")
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return DefaultFPS, nil
	}
	if ok {
		d, err := strconv.ParseFloat(den, 64)
		if err != nil || d == 0 {
			return DefaultFPS, nil
		}
		f /= d
	}
	if f <= 0 {
		return DefaultFPS, nil
	}
	return f, nil
}
