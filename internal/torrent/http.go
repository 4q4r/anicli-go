package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// The loopback stream server is what the player actually plays: mpv
// gets an http://127.0.0.1/... URL with Content-Length and byte-range
// support (single range — 206 Partial Content — is what seeking uses),
// so playback starts while pieces still download.

// ErrRangeNotSatisfiable reports a Range header whose start lies
// beyond the file (RFC 9110 §14.2: answer 416).
var ErrRangeNotSatisfiable = errors.New("torrent: range not satisfiable")

// mimeByExtension maps the container extensions worth labeling; the
// fallback is generic bytes (players sniff content anyway).
var mimeByExtension = map[string]string{
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".avi":  "video/x-msvideo",
	".mov":  "video/quicktime",
	".ts":   "video/mp2t",
	".webm": "video/webm",
}

// MimeTypeOf resolves the stream Content-Type from the file extension.
func MimeTypeOf(path string) string {
	dot := strings.LastIndexByte(path, '.')
	if dot >= 0 {
		if mt, ok := mimeByExtension[strings.ToLower(path[dot:])]; ok {
			return mt
		}
	}
	return "application/octet-stream"
}

// startStreamServerLocked binds the loopback-only stream server on an
// ephemeral port. Callers hold mu.
func (e *Engine) startStreamServerLocked() error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("torrent: listen stream server: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream/{infohash}/{index}", e.serveStream)
	srv := &http.Server{ //nolint:gosec // loopback-only listener; timeouts below
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	e.httpSrv = srv
	e.httpPort = l.Addr().(*net.TCPAddr).Port
	go func() {
		// Serve always returns http.ErrServerClosed after Shutdown.
		_ = srv.Serve(l)
	}()
	return nil
}

// serveStream plays one file of one release: it resolves the handle
// (which may wait for metadata — bounded by the request context),
// then serves it with Content-Length and byte-range support.
func (e *Engine) serveStream(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("infohash")
	if !isInfoHashHex(raw) {
		http.Error(w, "bad infohash", http.StatusBadRequest)
		return
	}
	ih := metainfo.NewHashFromHex(raw)
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		http.Error(w, "bad file index", http.StatusBadRequest)
		return
	}
	handle, err := e.Resolve(r.Context(), ih, index)
	if err != nil {
		http.Error(w, err.Error(), streamErrorStatus(err))
		return
	}
	defer func() { _ = handle.Reader.Close() }()

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", handle.MimeType)

	status := http.StatusOK
	length := handle.Size
	if spec := r.Header.Get("Range"); spec != "" {
		start, rangeLen, err := parseRange(spec, handle.Size)
		if errors.Is(err, ErrRangeNotSatisfiable) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", handle.Size))
			http.Error(w, err.Error(), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if err == nil {
			if _, err := handle.Reader.Seek(start, io.SeekStart); err != nil {
				http.Error(w, fmt.Sprintf("torrent: seek: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, start+rangeLen-1, handle.Size))
			status = http.StatusPartialContent
			length = rangeLen
		}
		// A malformed Range header serves the full body (RFC 9110
		// §14.2: an invalid header MUST be ignored).
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
	flushCopy(w, io.LimitReader(handle.Reader, length))
}

// streamErrorStatus maps typed engine errors onto HTTP statuses.
func streamErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrUnknownRelease), errors.Is(err, ErrBadFileIndex):
		return http.StatusNotFound
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// parseRange parses a single byte-range spec ("bytes=a-b", "bytes=a-",
// "bytes=-suffix", first range only when several are listed) against a
// file of the given size. Malformed specs yield the full range
// (0, size, nil) — the caller serves the whole body; a start beyond
// the file yields ErrRangeNotSatisfiable.
func parseRange(spec string, size int64) (start, length int64, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, size, nil
	}
	if !strings.HasPrefix(spec, "bytes=") {
		return 0, size, nil
	}
	// Multiple ranges: serve the first (single-range is all mpv uses).
	first := strings.SplitN(strings.TrimPrefix(spec, "bytes="), ",", 2)[0]
	first = strings.TrimSpace(first)
	dash := strings.IndexByte(first, '-')
	if dash < 0 {
		return 0, size, nil
	}
	startPart, endPart := strings.TrimSpace(first[:dash]), strings.TrimSpace(first[dash+1:])

	switch {
	case startPart == "" && endPart == "":
		// "bytes=-" is malformed: ignore.
		return 0, size, nil
	case startPart == "":
		// Suffix range: the last N bytes.
		n, err := strconv.ParseInt(endPart, 10, 64)
		if err != nil || n < 0 {
			return 0, size, nil
		}
		if n == 0 || size == 0 {
			// "bytes=-0" and any suffix against an empty file are
			// unsatisfiable (a length-0 206 would render
			// "Content-Range: bytes 0--1/0").
			return 0, 0, ErrRangeNotSatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, n, nil
	default:
		s, err := strconv.ParseInt(startPart, 10, 64)
		if err != nil || s < 0 {
			return 0, size, nil
		}
		if s >= size {
			return 0, 0, ErrRangeNotSatisfiable
		}
		if endPart == "" {
			return s, size - s, nil
		}
		e, err := strconv.ParseInt(endPart, 10, 64)
		if err != nil || e < s {
			// Malformed or reversed: ignore the header entirely.
			return 0, size, nil
		}
		if e >= size {
			e = size - 1
		}
		return s, e - s + 1, nil
	}
}

// flushCopy writes the body through in chunks, flushing after each —
// the player starts rendering as soon as the first pieces land.
func flushCopy(w http.ResponseWriter, r io.Reader) {
	buf := make([]byte, 64<<10)
	flusher, _ := w.(http.Flusher)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
