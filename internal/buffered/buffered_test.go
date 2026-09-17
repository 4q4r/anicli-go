package buffered

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// newTestDownloader builds a Downloader over a live netclient (for
// playlists) and the default streaming client; both hit httptest
// servers only.
func newTestDownloader(t *testing.T) *Downloader {
	t.Helper()
	net, err := netclient.New(config.Network{
		RequestTimeout: 5 * time.Second,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	return New(net, &http.Client{Timeout: 10 * time.Second}, discardLogger())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitForOrphans asserts the active-dir registry drained to empty —
// the no-temp-orphan contract of every error path.
func waitForOrphans(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(activeDirsSnapshot()) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("temp dirs leaked: %v", activeDirsSnapshot())
}

// slowBody writes size bytes in chunks with a delay, honouring the
// request context so cancellation unblocks the handler.
func slowBody(ctx context.Context, size int, chunkDelay time.Duration) io.Reader {
	return &slowReader{ctx: ctx, left: size, delay: chunkDelay}
}

type slowReader struct {
	ctx   context.Context
	left  int
	delay time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(r.delay):
	}
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.left {
		n = r.left
	}
	r.left -= n
	for i := 0; i < n; i++ {
		p[i] = byte(i % 251)
	}
	return n, nil
}

// TestBufferProgressiveMP4: a plain mp4 URL buffers to a temp file byte
// for byte, reports 100% progress, and Cleanup removes every trace.
func TestBufferProgressiveMP4(t *testing.T) {
	payload := []byte("fake-mp4-payload-0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := newTestDownloader(t)
	var final Progress
	var mu sync.Mutex
	handle, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/video/ep1.mp4"},
		func(p Progress) {
			mu.Lock()
			final = p
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	mu.Lock()
	got := final
	mu.Unlock()
	if got.Done != int64(len(payload)) || got.Total != int64(len(payload)) {
		t.Fatalf("final progress = %+v, want done=total=%d", got, len(payload))
	}
	data, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatalf("read buffered file: %v", err)
	}
	if string(data) != string(payload) {
		t.Fatalf("buffered payload differs: %d bytes", len(data))
	}
	if filepath.Base(handle.Path) != "ep1.mp4" {
		t.Fatalf("file name = %s, want ep1.mp4", handle.Path)
	}
	handle.Cleanup()
	if _, err := os.Stat(handle.Path); !os.IsNotExist(err) {
		t.Fatalf("buffered file survived cleanup: %v", err)
	}
	waitForOrphans(t)
}

// TestBufferProgressiveCancellation: cancelling mid-download stops the
// copy, fails the Buffer call and leaves no temp file behind.
func TestBufferProgressiveCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = io.Copy(w, slowBody(r.Context(), 1000000, 20*time.Millisecond))
	}))
	defer srv.Close()

	d := newTestDownloader(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Buffer(ctx, Source{URL: srv.URL + "/big.mp4"}, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Buffer err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Buffer did not react to cancellation")
	}
	waitForOrphans(t)
}

// TestBufferProgressiveHTTPError: a failed download surfaces the HTTP
// status as a typed error and leaves no temp file behind.
func TestBufferProgressiveHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	d := newTestDownloader(t)
	_, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/x.mp4"}, nil)
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		t.Fatalf("err = %v, want *StatusError 404", err)
	}
	waitForOrphans(t)
}

// hlsServer serves fixture playlists/segments from a map; the handler
// records the request paths for variant-selection assertions.
type hlsServer struct {
	*httptest.Server
	mu     sync.Mutex
	served []string
}

func newHLSServer(files map[string]string) *hlsServer {
	h := &hlsServer{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.served = append(h.served, r.URL.Path)
		h.mu.Unlock()
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	return h
}

func (h *hlsServer) paths() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.served...)
}

// TestBufferHLSConcatenatesSegments: a plain media playlist downloads
// its segments sequentially into one file, counting progress by
// segments, with relative URLs resolved against the playlist URL.
func TestBufferHLSConcatenatesSegments(t *testing.T) {
	srv := newHLSServer(map[string]string{
		"/live/ep1.m3u8": "#EXTM3U\n" +
			"#EXT-X-TARGETDURATION:6\n" +
			"#EXTINF:6.0,\n" +
			"seg1.ts\n" +
			"#EXTINF:6.0,\n" +
			"seg2.ts\n" +
			"#EXT-X-ENDLIST\n",
		"/live/seg1.ts": "SEGONE",
		"/live/seg2.ts": "SEGTWO",
	})
	defer srv.Close()

	d := newTestDownloader(t)
	var final Progress
	handle, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/live/ep1.m3u8"},
		func(p Progress) { final = p })
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	if final.SegmentsDone != 2 || final.SegmentsTotal != 2 {
		t.Fatalf("final progress = %+v, want segments 2/2", final)
	}
	data, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "SEGONESEGTWO" {
		t.Fatalf("concatenated content = %q, want SEGONESEGTWO", data)
	}
	handle.Cleanup()
	waitForOrphans(t)
}

// TestBufferHLSMasterPicksBestVariant: a master playlist selects the
// highest-BANDWIDTH variant before downloading.
func TestBufferHLSMasterPicksBestVariant(t *testing.T) {
	srv := newHLSServer(map[string]string{
		"/master.m3u8": "#EXTM3U\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=400000,RESOLUTION=640x360\n" +
			"low/idx.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080\n" +
			"high/idx.m3u8\n",
		"/low/idx.m3u8":  "#EXTM3U\n#EXTINF:1.0,\nlo.ts\n#EXT-X-ENDLIST\n",
		"/high/idx.m3u8": "#EXTM3U\n#EXTINF:1.0,\nhi.ts\n#EXT-X-ENDLIST\n",
		"/low/lo.ts":     "LOW",
		"/high/hi.ts":    "HIGH",
	})
	defer srv.Close()

	d := newTestDownloader(t)
	handle, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/master.m3u8"}, nil)
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	data, _ := os.ReadFile(handle.Path)
	if string(data) != "HIGH" {
		t.Fatalf("content = %q, want HIGH (best variant)", data)
	}
	handle.Cleanup()
	waitForOrphans(t)
}

// TestBufferHLSInitPrepended: EXT-X-MAP init data lands BEFORE the
// first segment (fMP4 streams play from the concatenation in mpv).
func TestBufferHLSInitPrepended(t *testing.T) {
	srv := newHLSServer(map[string]string{
		"/stream/media.m3u8": "#EXTM3U\n" +
			"#EXT-X-MAP:URI=\"init.mp4\"\n" +
			"#EXTINF:4.0,\n" +
			"s1.m4s\n" +
			"#EXT-X-ENDLIST\n",
		"/stream/init.mp4": "INIT",
		"/stream/s1.m4s":   "FRAG1",
	})
	defer srv.Close()

	d := newTestDownloader(t)
	handle, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/stream/media.m3u8"}, nil)
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	data, _ := os.ReadFile(handle.Path)
	if string(data) != "INITFRAG1" {
		t.Fatalf("content = %q, want INITFRAG1 (init first)", data)
	}
	handle.Cleanup()
	waitForOrphans(t)
}

// TestBufferHLSEncryptedFailsTyped: an EXT-X-KEY with a real METHOD is
// a typed fail-loud error — the buffered path cannot decrypt.
func TestBufferHLSEncryptedFailsTyped(t *testing.T) {
	srv := newHLSServer(map[string]string{
		"/enc.m3u8": "#EXTM3U\n" +
			"#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n" +
			"#EXTINF:4.0,\ns.ts\n#EXT-X-ENDLIST\n",
	})
	defer srv.Close()

	d := newTestDownloader(t)
	_, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/enc.m3u8"}, nil)
	if !errors.Is(err, ErrEncryptedHLS) {
		t.Fatalf("err = %v, want ErrEncryptedHLS", err)
	}
	waitForOrphans(t)
}

// TestBufferHLSLiveFailsTyped: a playlist without EXT-X-ENDLIST (live)
// is a typed error.
func TestBufferHLSLiveFailsTyped(t *testing.T) {
	srv := newHLSServer(map[string]string{
		"/live.m3u8": "#EXTM3U\n#EXTINF:4.0,\ns1.ts\n",
		"/s1.ts":     "X",
	})
	defer srv.Close()

	d := newTestDownloader(t)
	_, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/live.m3u8"}, nil)
	if !errors.Is(err, ErrLiveHLS) {
		t.Fatalf("err = %v, want ErrLiveHLS", err)
	}
	waitForOrphans(t)
}

// TestBufferHLSSegmentFailureCleans: a failing segment aborts the
// download and leaves no temp file behind.
func TestBufferHLSSegmentFailureCleans(t *testing.T) {
	srv := newHLSServer(map[string]string{
		"/ep.m3u8": "#EXTM3U\n#EXTINF:1.0,\na.ts\n#EXTINF:1.0,\nmissing.ts\n#EXT-X-ENDLIST\n",
		"/a.ts":    "AAA",
	})
	defer srv.Close()

	d := newTestDownloader(t)
	_, err := d.Buffer(context.Background(), Source{URL: srv.URL + "/ep.m3u8"}, nil)
	if err == nil {
		t.Fatal("segment failure must surface an error")
	}
	waitForOrphans(t)
}

// TestHeadersForwarded: the source headers (Referer etc.) reach both
// the playlist and the segment requests.
func TestHeadersForwarded(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Referer"))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			_, _ = w.Write([]byte("#EXTM3U\n#EXTINF:1.0,\na.ts\n#EXT-X-ENDLIST\n"))
			return
		}
		_, _ = w.Write([]byte("SEG"))
	}))
	defer srv.Close()

	d := newTestDownloader(t)
	handle, err := d.Buffer(context.Background(), Source{
		URL:     srv.URL + "/h.m3u8",
		Headers: map[string]string{"Referer": "https://example.org/"},
	}, nil)
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	handle.Cleanup()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "https://example.org/" || seen[1] != "https://example.org/" {
		t.Fatalf("Referer headers seen = %v, want it on playlist and segment", seen)
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
