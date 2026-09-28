// Package player launches the external mpv player with the argument
// family ported from anicli-py anicli/core/player.py: cache/demuxer
// buffering (including --demuxer-max-back-bytes=500MiB), HTTP header
// mapping, chapters files with guaranteed cleanup, and a deterministic
// SIGTERM -> SIGKILL shutdown ladder on context cancellation.
package player

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Options carries the launcher knobs. Timeout, retries and warmup
// defaults mirror the python MPVSettings/play_detached behavior; the
// TUI shell wires config onto them.
type Options struct {
	// Bin is the mpv binary or path (default "mpv").
	Bin string
	// Timeout feeds --network-timeout in seconds (python mpv.timeout,
	// default 30).
	Timeout int
	// Profile is an optional --profile value.
	Profile string
	// MaxRetries bounds launch attempts (python max_retries=5).
	MaxRetries int
	// Warmup is how long a process must survive to count as launched
	// (python 2s sleep check).
	Warmup time.Duration
	// RetryDelay pauses between attempts (python 1s).
	RetryDelay time.Duration
	// TermGrace is the SIGTERM -> SIGKILL grace window.
	TermGrace time.Duration
}

// Request is one playback invocation.
type Request struct {
	// URL is the media URL or local file path.
	URL string
	// AudioURL is an optional external audio stream; ignored when
	// equal to URL.
	AudioURL string
	// Title feeds --force-media-title.
	Title string
	// Headers are HTTP headers required by the source.
	Headers map[string]string
	// ExtraMPVOpts are source-specific extra options.
	ExtraMPVOpts []string
	// ChaptersFile is an FFMETADATA path; it is deleted once the
	// player exits (python lifecycle cleanup).
	ChaptersFile string
}

// BuildArgs renders the mpv argv (python _build_command, ported
// verbatim including the option order).
func BuildArgs(req Request, opts Options) []string {
	bin := opts.Bin
	if bin == "" {
		bin = "mpv"
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30
	}

	parts := []string{bin}
	parts = append(parts,
		"--cache=yes",
		"--demuxer-max-bytes=2048MiB",
		"--demuxer-max-back-bytes=500MiB",
		fmt.Sprintf("--network-timeout=%d", timeout),
		"--stream-buffer-size=16MiB",
		"--hwdec=auto-safe",
	)

	if opts.Profile != "" {
		parts = append(parts, "--profile="+opts.Profile)
	}
	// PR113: mpv writes its watch-later file on quit, so replaying
	// the same episode resumes from the saved position natively —
	// no position tracking on the Go side.
	parts = append(parts, "--save-position-on-quit")
	if req.AudioURL != "" && req.AudioURL != req.URL {
		parts = append(parts, "--audio-file="+req.AudioURL)
	}
	if req.Title != "" {
		parts = append(parts, "--force-media-title="+req.Title)
	}
	if req.ChaptersFile != "" {
		parts = append(parts, "--chapters-file="+req.ChaptersFile)
	}

	parts = append(parts,
		"--osd-level=1",
		"--osd-duration=2500",
		"--osd-font-size=32",
		"--cursor-autohide=1000",
	)

	parts = append(parts, headersToMPVOpts(req.Headers)...)
	parts = append(parts, req.ExtraMPVOpts...)
	parts = append(parts,
		"--msg-level=all=error",
		"--no-ytdl",
		req.URL,
	)
	return parts
}

// headersToMPVOpts maps headers to mpv arguments: user-agent and
// referer are popped case-insensitively into their dedicated flags,
// the rest join into one --http-header-fields entry (python
// _headers_to_mpv_opts, comma-joined "Key: Value" pairs).
func headersToMPVOpts(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	remaining := make(map[string]string, len(headers))
	for k, v := range headers {
		remaining[k] = v
	}

	var opts []string
	if ua := popHeader(remaining, "User-Agent"); ua != "" {
		opts = append(opts, "--user-agent="+ua)
	}
	if ref := popHeader(remaining, "Referer"); ref != "" {
		opts = append(opts, "--referrer="+ref)
	}
	if len(remaining) > 0 {
		fields := make([]string, 0, len(remaining))
		keys := make([]string, 0, len(remaining))
		for k := range remaining {
			keys = append(keys, k)
		}
		// Python dicts preserved insertion order; Go maps do not, so
		// the joined form is sorted for deterministic argv.
		sort.Strings(keys)
		for _, k := range keys {
			fields = append(fields, k+": "+remaining[k])
		}
		opts = append(opts, "--http-header-fields="+strings.Join(fields, ","))
	}
	return opts
}

// popHeader removes and returns a header matched case-insensitively.
func popHeader(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			delete(headers, k)
			return v
		}
	}
	return ""
}

// OfflineTitle composes the offline window title with the [OFFLINE]
// marker: "[<video dub> - <audio dub>] <title> - <episode> [OFFLINE]",
// stripping any leading "[provider] " prefixes from the dub keys
// (python cli/offline.py _offline_session).
func OfflineTitle(videoKey, audioKey, title, episode string) string {
	return fmt.Sprintf("[%s - %s] %s - %s [OFFLINE]",
		stripProviderPrefix(videoKey), stripProviderPrefix(audioKey), title, episode)
}

// stripProviderPrefix removes a leading bracketed provider tag.
func stripProviderPrefix(key string) string {
	if !strings.HasPrefix(key, "[") {
		return key
	}
	if end := strings.Index(key, "]"); end >= 0 {
		return strings.TrimSpace(key[end+1:])
	}
	return key
}
