package buffered

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// Typed failures of the buffered pipeline. The TUI surfaces them
// verbatim in the status line; the HLS ones name the recovery (the
// streaming mode still plays those sources).
var (
	// ErrEncryptedHLS reports an EXT-X-KEY with a real METHOD: the
	// buffered concatenation cannot decrypt segment payloads.
	ErrEncryptedHLS = errors.New("зашифрованный HLS — доступен только потоковый режим")
	// ErrLiveHLS reports a media playlist without EXT-X-ENDLIST: a
	// live stream never completes, so buffering it cannot finish.
	ErrLiveHLS = errors.New("живой HLS-поток — доступен только потоковый режим")
	// ErrUnsupportedHLS reports HLS features the minimal downloader
	// deliberately does not implement (BYTERANGE).
	ErrUnsupportedHLS = errors.New("неподдерживаемая функция HLS — доступен только потоковый режим")
	// ErrNotPlaylist reports an .m3u8 URL whose body is not an
	// EXTM3U playlist.
	ErrNotPlaylist = errors.New("HLS-плейлист не распознан")
)

// StatusError reports a final non-2xx media response.
type StatusError struct {
	Code int
	URL  string
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("сервер ответил %d для %s", e.Code, e.URL)
}

// Source is one media source to buffer locally.
type Source struct {
	// URL is the media, torrent-loopback or HLS playlist URL.
	URL string
	// Headers are the source-required request headers (Referer, UA).
	Headers map[string]string
	// Name optionally pins the local file name; empty derives it from
	// the URL path.
	Name string
}

// Progress is one throttled progress sample. Byte fields drive the
// progressive/torrent display, the segment fields the HLS one.
type Progress struct {
	// Done is the bytes written so far (0 on HLS downloads).
	Done int64
	// Total is the expected byte count; 0 when unknown.
	Total int64
	// SegmentsDone / SegmentsTotal count HLS playlist progress.
	SegmentsDone  int
	SegmentsTotal int
	// SpeedBPS is the smoothed transfer speed in bytes per second.
	SpeedBPS float64
}

// Handle is the completed local file with its cleanup.
type Handle struct {
	// Path is the fully buffered local file (mpv plays it directly).
	Path    string
	cleanup func()
}

// NewHandle builds a Handle over a local file with its cleanup func
// (service adapters and tests construct handles; idempotent cleanups
// are the builder's responsibility — releaseTempDir already is).
func NewHandle(path string, cleanup func()) Handle {
	return Handle{Path: path, cleanup: cleanup}
}

// Cleanup removes the buffered file and its temp dir; idempotent —
// releaseTempDir forgets the dir on the first call, so repeats are
// no-ops.
func (h Handle) Cleanup() {
	if h.cleanup == nil {
		return
	}
	h.cleanup()
}

// Downloader buffers sources to temporary files.
type Downloader struct {
	// net fetches playlists (small bodies, browser-parity transport).
	net *netclient.Client
	// stream pulls media bytes (progressive files, HLS segments).
	stream *http.Client
	log    *slog.Logger
	// progressInterval throttles progress callbacks; 0 keeps 250ms.
	progressInterval time.Duration
}

// New builds the downloader. net may be nil only for sources that
// never need playlist fetches (progressive URLs).
func New(net *netclient.Client, stream *http.Client, log *slog.Logger) *Downloader {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if stream == nil {
		stream = &http.Client{}
	}
	return &Downloader{net: net, stream: stream, log: log}
}

// isHLS reports whether the URL targets an HLS playlist.
func isHLS(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.Contains(strings.ToLower(rawURL), ".m3u8")
	}
	return strings.Contains(strings.ToLower(u.Path), ".m3u8")
}

// Buffer downloads src to a temporary file. progress (may be nil)
// receives throttled samples and one final forced sample. On ANY error
// the temp file is removed before the call returns; on success Cleanup
// removes it after the player exits.
func (d *Downloader) Buffer(ctx context.Context, src Source, progress func(Progress)) (Handle, error) {
	dir, err := newTempDir()
	if err != nil {
		return Handle{}, fmt.Errorf("buffered: create temp dir: %w", err)
	}
	path, err := d.bufferInto(ctx, src, dir, newTracker(d.progressInterval, progress))
	if err != nil {
		releaseTempDir(dir)
		return Handle{}, err
	}
	d.log.Info("buffered: download complete", "url", src.URL, "path", path)
	return Handle{Path: path, cleanup: func() {
		releaseTempDir(dir)
		d.log.Info("buffered: temp file removed", "dir", dir)
	}}, nil
}

// bufferInto dispatches on the source kind and returns the file path.
func (d *Downloader) bufferInto(ctx context.Context, src Source, dir string, tr *tracker) (string, error) {
	if isHLS(src.URL) {
		return d.bufferHLS(ctx, src, dir, tr)
	}
	return d.bufferProgressive(ctx, src, dir, tr)
}

// bufferProgressive streams one progressive GET into the temp file.
// This covers direct mp4/mkv CDN links AND the torrent engine's
// loopback stream server: the server resolves the file's pieces with
// top priority (everything else lowest), so reading it to completion
// IS the engine download-to-completion — and an already-seeded file
// completes instantly, opening immediately.
func (d *Downloader) bufferProgressive(ctx context.Context, src Source, dir string, tr *tracker) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return "", fmt.Errorf("buffered: build request: %w", err)
	}
	for k, v := range src.Headers {
		req.Header.Set(k, v)
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		return "", fmt.Errorf("buffered: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &StatusError{Code: resp.StatusCode, URL: src.URL}
	}

	name := localName(src.Name, src.URL, ".mp4")
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("buffered: create temp file: %w", err)
	}

	// Cancellation must unblock io.Copy: closing the body from the
	// ctx goroutine forces the copy to fail with the context error.
	done := make(chan error, 1)
	pw := &progressWriter{f: f, tr: tr, total: resp.ContentLength}
	go func() {
		_, copyErr := io.Copy(pw, resp.Body)
		done <- copyErr
	}()
	select {
	case copyErr := <-done:
		if copyErr != nil {
			_ = f.Close()
			return "", fmt.Errorf("buffered: download: %w", copyErr)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("buffered: close temp file: %w", err)
		}
		tr.force(Progress{Done: pw.done, Total: pw.total, SpeedBPS: tr.speed})
		return path, nil
	case <-ctx.Done():
		_ = resp.Body.Close()
		<-done // the copy unblocks and the goroutine exits (no leak)
		_ = f.Close()
		return "", ctx.Err()
	}
}

// tracker throttles and smooths progress samples.
type tracker struct {
	interval time.Duration
	emit     func(Progress)
	last     time.Time
	lastDone int64
	speed    float64
}

func newTracker(interval time.Duration, emit func(Progress)) *tracker {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	return &tracker{interval: interval, emit: emit, last: time.Now()}
}

// step records a sample and emits when the throttle window elapsed.
func (t *tracker) step(p Progress) {
	now := time.Now()
	dt := now.Sub(t.last)
	if dt < t.interval {
		return
	}
	if p.Done > t.lastDone {
		instant := float64(p.Done-t.lastDone) / dt.Seconds()
		t.speed = 0.7*instant + 0.3*t.speed
	}
	t.last, t.lastDone = now, p.Done
	if t.emit != nil {
		t.emit(p)
	}
}

// force emits the final sample regardless of the throttle window.
func (t *tracker) force(p Progress) {
	if t.emit == nil {
		return
	}
	p.SpeedBPS = t.speed
	t.emit(p)
}

// progressWriter counts bytes into the tracker.
type progressWriter struct {
	f     *os.File
	tr    *tracker
	total int64
	done  int64
}

// Write implements io.Writer.
func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if n > 0 {
		w.done += int64(n)
		w.tr.step(Progress{Done: w.done, Total: w.total})
	}
	return n, err
}

// localName resolves the temp file name: the explicit name, else the
// sanitized URL path base with the given extension fallback.
func localName(name, rawURL, fallbackExt string) string {
	if name != "" {
		return sanitize(name, fallbackExt)
	}
	u, err := url.Parse(rawURL)
	if err == nil {
		base := filepath.Base(u.Path)
		if base != "" && base != "/" && base != "." {
			return sanitize(base, fallbackExt)
		}
	}
	return "video" + fallbackExt
}

// sanitize keeps a plain file name from a URL path fragment.
func sanitize(name, fallback string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._")
	if out == "" {
		return "video" + fallback
	}
	return out
}
