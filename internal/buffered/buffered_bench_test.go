package buffered

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// PR81 benchmarks for the download pipeline's HLS stage: media/master
// playlist parsing and the full segment orchestration loop against a
// loopback fixture server (no external network).

// benchMediaPlaylist renders an EXTINF media playlist with n segments.
func benchMediaPlaylist(n int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:2\n")
	for i := range n {
		fmt.Fprintf(&b, "#EXTINF:1.5,\nsegment_%04d.ts\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// benchMasterPlaylist renders a multi-variant master playlist.
func benchMasterPlaylist() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n360/index.m3u8\n")
	b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1920x1080\n1080/index.m3u8\n")
	b.WriteString("#EXT-X-STREAM-INF:BANDWIDTH=1500000,RESOLUTION=1280x720\n720/index.m3u8\n")
	return b.String()
}

var (
	benchSinkMedia   hlsMedia
	benchSinkVariant string
	benchSinkHandle  Handle
)

// BenchmarkParseHLSMedia1000 parses a 1000-segment media playlist —
// the per-fetch cost of a full-episode VOD manifest.
func BenchmarkParseHLSMedia1000(b *testing.B) {
	b.ReportAllocs()
	body := benchMediaPlaylist(1000)
	base, _ := url.Parse("https://cdn.example/hls/1080/index.m3u8")
	for b.Loop() {
		media, err := parseHLSMedia(body, base)
		if err != nil {
			b.Fatalf("parse media: %v", err)
		}
		benchSinkMedia = media
	}
}

// BenchmarkParseHLSAttrs benchmarks the EXTINF/EXT-X attribute-line
// tokenizer over a realistic attribute set.
func BenchmarkParseHLSAttrs(b *testing.B) {
	b.ReportAllocs()
	line := `#EXT-X-STREAM-INF:BANDWIDTH=3000000,AVERAGE-BANDWIDTH=2900000,RESOLUTION=1920x1080,FRAME-RATE=23.976,CODECS="avc1.640028,mp4a.40.2"`
	for b.Loop() {
		attrs := parseHLSAttrs(line)
		if len(attrs) == 0 {
			b.Fatal("no attrs parsed")
		}
		benchSinkVariant = attrs["RESOLUTION"]
	}
}

// BenchmarkPickHLSVariant selects the best variant from the master
// playlist (the quality-resolution step of every HLS buffer).
func BenchmarkPickHLSVariant(b *testing.B) {
	b.ReportAllocs()
	body := benchMasterPlaylist()
	base, _ := url.Parse("https://cdn.example/hls/master.m3u8")
	for b.Loop() {
		variant, ok, err := pickHLSVariant(body, base)
		if err != nil || !ok {
			b.Fatalf("pick variant: ok=%v err=%v", ok, err)
		}
		benchSinkVariant = variant
	}
}

// benchHLSFixtureServer serves a media playlist plus n in-memory
// segments over loopback.
func benchHLSFixtureServer(b *testing.B, segments int, segmentBytes int) *httptest.Server {
	b.Helper()
	playlist := benchMediaPlaylist(segments)
	segment := []byte(strings.Repeat("a", segmentBytes)) // one copy, reused per response
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(playlist))
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(segment)
	}))
	b.Cleanup(srv.Close)
	return srv
}

// benchBufferDownloader builds the b/t-variant of the test downloader.
func benchBufferDownloader(b interface {
	Helper()
	Fatalf(string, ...interface{})
}) *Downloader {
	net, err := netclient.New(config.Network{
		RequestTimeout: 10 * time.Second,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		b.Fatalf("netclient: %v", err)
	}
	return New(net, &http.Client{Timeout: 30 * time.Second}, discardLogger())
}

// BenchmarkBufferHLSLoopback runs the FULL HLS buffer pipeline (fetch
// playlist → parse → 200 sequential segment fetch+append → cleanup
// path) against a loopback server with 200 × 64KiB segments. ns/op is
// the orchestration overhead floor the pipeline adds on top of real
// network throughput.
func BenchmarkBufferHLSLoopback(b *testing.B) {
	b.ReportAllocs()
	srv := benchHLSFixtureServer(b, 200, 64*1024)
	d := benchBufferDownloader(b)
	ctx := context.Background()
	for b.Loop() {
		handle, err := d.Buffer(ctx, Source{URL: srv.URL + "/hls/1080/index.m3u8"}, nil)
		if err != nil {
			b.Fatalf("buffer: %v", err)
		}
		benchSinkHandle = handle
		handle.Cleanup()
	}
}
