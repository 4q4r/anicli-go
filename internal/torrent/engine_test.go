package torrent

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

const testHexIH = "0123456789abcdef0123456789abcdef01234567"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newNetTestEngine is newTestEngine with a real netclient wired, so
// URL-ingest tests can fetch from httptest servers (still no DHT/UPnP
// egress: the offline guard stays on).
func newNetTestEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	net, err := netclient.New(cfg, netclient.WithProvider("torrent-test"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	eng := NewEngine(config.Torrent{
		Enabled:     true,
		Dir:         t.TempDir(),
		Port:        0,
		ReadaheadMB: 1,
	}, net, quietLogger())
	eng.testNoExternal = true
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// newTestEngine builds an engine over a temp dir with external network
// surface disabled (no DHT bootstrap, no UPnP, no webtorrent): the
// offline E2E wires peers explicitly via the magnet x.pe parameter.
func newTestEngine(t *testing.T, enabled bool) *Engine {
	t.Helper()
	eng := NewEngine(config.Torrent{
		Enabled:     enabled,
		Dir:         t.TempDir(),
		Port:        0,
		ReadaheadMB: 1,
	}, nil, quietLogger())
	eng.testNoExternal = true
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// TestNewOfflineEngineForTests pins the exported test-only
// constructor: it must produce an engine with every external
// discovery channel stripped, so provider-package tests never egress
// to public DHT/UPnP/webtorrent during the default suite.
func TestNewOfflineEngineForTests(t *testing.T) {
	t.Parallel()

	eng := NewOfflineEngineForTests(config.Torrent{
		Enabled:     true,
		Dir:         t.TempDir(),
		Port:        0,
		ReadaheadMB: 1,
	}, nil, quietLogger())
	t.Cleanup(func() { _ = eng.Close() })

	if !eng.testNoExternal {
		t.Fatal("NewOfflineEngineForTests must set the offline guard (no DHT/UPnP/webtorrent)")
	}
	// Same laziness contract as NewEngine: nothing runs until the
	// first link.
	if eng.client != nil {
		t.Error("offline constructor must not start the client")
	}
	if eng.httpSrv != nil {
		t.Error("offline constructor must not start the stream server")
	}
}

func TestAddLinkMagnetDedupesByInfoHash(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, true)
	ctx := context.Background()

	link := "magnet:?xt=urn:btih:" + testHexIH + "&dn=Test%20Title"
	rel, err := eng.AddLink(ctx, link)
	if err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	if rel.InfoHash.HexString() != testHexIH {
		t.Errorf("InfoHash = %s, want %s", rel.InfoHash.HexString(), testHexIH)
	}
	if rel.DisplayName != "Test Title" {
		t.Errorf("DisplayName = %q, want %q (dn= param)", rel.DisplayName, "Test Title")
	}
	if rel.Status != StatusFetching {
		t.Errorf("Status = %q, want %q (no peers: metadata cannot arrive)", rel.Status, StatusFetching)
	}

	again, err := eng.AddLink(ctx, link)
	if err != nil {
		t.Fatalf("second AddLink: %v", err)
	}
	if again.InfoHash != rel.InfoHash {
		t.Errorf("second AddLink InfoHash = %s, want dedupe to %s", again.InfoHash.HexString(), testHexIH)
	}
	if got := len(eng.Releases()); got != 1 {
		t.Errorf("Releases() = %d entries, want 1 (dedupe by infohash)", got)
	}
}

func TestAddLinkZeroInfoHashTypedError(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, true)

	// The library PANICS on a zero infohash (AddTorrentOpt →
	// panicif.Zero); the engine must convert it to a typed error.
	for _, link := range []string{
		"magnet:?xt=urn:btih:" + hex.EncodeToString(make([]byte, 20)) + "&dn=zero",
		"magnet:?dn=no-hash-here",
		"magnet:?xt=urn:btmv:garbage",
	} {
		_, err := eng.AddLink(context.Background(), link)
		if !errors.Is(err, ErrZeroInfoHash) {
			t.Errorf("AddLink(%q) err = %v, want ErrZeroInfoHash", link, err)
		}
	}
}

func TestAddLinkBareInfohash(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, true)

	rel, err := eng.AddLink(context.Background(), testHexIH)
	if err != nil {
		t.Fatalf("AddLink(infohash): %v", err)
	}
	if rel.InfoHash.HexString() != testHexIH {
		t.Errorf("InfoHash = %s, want %s", rel.InfoHash.HexString(), testHexIH)
	}
	if rel.DisplayName != testHexIH {
		t.Errorf("DisplayName = %q, want the hex itself", rel.DisplayName)
	}
}

func TestAddLinkUnsupported(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, true)

	// Not a magnet, not a bare infohash: typed loud rejection before
	// any transport is consulted.
	_, err := eng.AddLink(context.Background(), "ftp://example.org/release")
	if !errors.Is(err, ErrUnsupportedLink) {
		t.Errorf("err = %v, want ErrUnsupportedLink", err)
	}
}

// TestAddLinkURLServesMetainfoWithoutTorrentSuffix pins the PR38
// relaxation the TokyoTosho feed forced: real-world .torrent links
// rarely end in ".torrent" (anirena.com/dl/N, nyaa.si/view/N/torrent),
// so the engine validates the RESPONSE CONTENT (bencode metainfo)
// instead of the URL suffix. A live dandadan search on TokyoTosho
// returned exactly such links for its top Anime hits.
func TestAddLinkURLServesMetainfoWithoutTorrentSuffix(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, mi, ih := seedTorrent(t, dir, 32*1024)
	body, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal metainfo: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	eng := newNetTestEngine(t)
	// A deliberately suffix-less URL (the TokyoTosho shape).
	rel, err := eng.AddLink(context.Background(), srv.URL+"/dl/200716")
	if err != nil {
		t.Fatalf("AddLink(suffix-less metainfo URL): %v", err)
	}
	if rel.InfoHash != ih {
		t.Errorf("InfoHash = %s, want %s (parsed from the served metainfo)", rel.InfoHash.HexString(), ih.HexString())
	}
}

// TestAddLinkURLNonMetainfoTypedError: a URL that answers HTML (a
// topic page, a login wall) must fail loud on the parse — the old
// URL-suffix precheck is gone, the content check is the guard.
func TestAddLinkURLNonMetainfoTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>not a torrent</body></html>"))
	}))
	t.Cleanup(srv.Close)

	eng := newNetTestEngine(t)
	_, err := eng.AddLink(context.Background(), srv.URL+"/forum/viewtopic.php?t=1")
	if err == nil {
		t.Fatal("non-metainfo body must fail loud")
	}
	if !strings.Contains(err.Error(), "parse .torrent") {
		t.Errorf("err = %v, want the metainfo parse failure", err)
	}
}

func TestAddLinkURLEngineWithoutNetclient(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, true)

	_, err := eng.AddLink(context.Background(), "https://example.org/release.torrent")
	if err == nil {
		t.Fatal("URL link without a netclient must fail loud, not pretend success")
	}
}

func TestAddLinkDisabledEngineStartsNothing(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, false)

	if _, err := eng.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
	if got := len(eng.Releases()); got != 0 {
		t.Errorf("Releases() = %d, want 0 (disabled engine stays empty)", got)
	}
	if port, ok := eng.ListenPort(); ok {
		t.Errorf("ListenPort() = (%d, true), want false (no client started)", port)
	}
	if url := eng.StreamURL(metainfo.Hash{}, 0); url != "" {
		t.Errorf("StreamURL = %q, want empty (no stream server)", url)
	}
}

func TestReleasesSortedByDisplayName(t *testing.T) {
	t.Parallel()
	eng := newTestEngine(t, true)
	ctx := context.Background()

	for _, link := range []string{
		"magnet:?xt=urn:btih:" + testHexIH + "&dn=Zeta",
		"magnet:?xt=urn:btih:" + "fedcba9876543210fedcba9876543210fedcba98" + "&dn=Alpha",
	} {
		if _, err := eng.AddLink(ctx, link); err != nil {
			t.Fatalf("AddLink(%q): %v", link, err)
		}
	}
	rels := eng.Releases()
	if len(rels) != 2 {
		t.Fatalf("Releases() = %d entries, want 2", len(rels))
	}
	if rels[0].DisplayName != "Alpha" || rels[1].DisplayName != "Zeta" {
		t.Errorf("order = [%s, %s], want [Alpha, Zeta]", rels[0].DisplayName, rels[1].DisplayName)
	}
}

func TestParseRange(t *testing.T) {
	t.Parallel()
	const size = 1000

	tests := []struct {
		name      string
		spec      string
		size      int64
		wantStart int64
		wantLen   int64
		wantErr   error
	}{
		{name: "full closed range", spec: "bytes=0-99", size: size, wantStart: 0, wantLen: 100},
		{name: "second range", spec: "bytes=100-199", size: size, wantStart: 100, wantLen: 100},
		{name: "open end clamps to size", spec: "bytes=900-", size: size, wantStart: 900, wantLen: 100},
		{name: "end beyond size clamps", spec: "bytes=990-2000", size: size, wantStart: 990, wantLen: 10},
		{name: "suffix range", spec: "bytes=-100", size: size, wantStart: 900, wantLen: 100},
		{name: "suffix larger than size", spec: "bytes=-5000", size: size, wantStart: 0, wantLen: 1000},
		{name: "zero bytes suffix unsatisfiable", spec: "bytes=-0", size: size, wantErr: ErrRangeNotSatisfiable},
		{name: "start at size unsatisfiable", spec: "bytes=1000-", size: size, wantErr: ErrRangeNotSatisfiable},
		{name: "start beyond size unsatisfiable", spec: "bytes=1500-1600", size: size, wantErr: ErrRangeNotSatisfiable},
		{name: "malformed ignored", spec: "bytes=zzz", size: size, wantStart: 0, wantLen: size},
		{name: "non-bytes unit ignored", spec: "items=0-1", size: size, wantStart: 0, wantLen: size},
		{name: "multi range serves first", spec: "bytes=0-9,50-59", size: size, wantStart: 0, wantLen: 10},
		{name: "reversed ignored", spec: "bytes=99-0", size: size, wantStart: 0, wantLen: size},
		// Zero-length file: every well-formed range is unsatisfiable —
		// a suffix range must never render "bytes 0--1/0".
		{name: "suffix on zero-size unsatisfiable", spec: "bytes=-5", size: 0, wantErr: ErrRangeNotSatisfiable},
		{name: "open range on zero-size unsatisfiable", spec: "bytes=0-", size: 0, wantErr: ErrRangeNotSatisfiable},
		{name: "closed range on zero-size unsatisfiable", spec: "bytes=0-0", size: 0, wantErr: ErrRangeNotSatisfiable},
		{name: "malformed on zero-size ignored", spec: "bytes=zzz", size: 0, wantStart: 0, wantLen: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, length, err := parseRange(tt.spec, tt.size)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if start != tt.wantStart || length != tt.wantLen {
				t.Errorf("got (%d, %d), want (%d, %d)", start, length, tt.wantStart, tt.wantLen)
			}
		})
	}
}

// seedTorrent writes size random bytes as a single-file torrent
// payload into dir and builds its metainfo (piece hashes computed from
// the file on disk — the same way real .torrent files are made).
func seedTorrent(t *testing.T, dir string, size int) ([]byte, *metainfo.MetaInfo, metainfo.Hash) {
	t.Helper()

	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic test data
	data := make([]byte, size)
	if _, err := rng.Read(data); err != nil {
		t.Fatalf("random data: %v", err)
	}
	path := filepath.Join(dir, "seed.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	var info metainfo.Info
	info.PieceLength = 32 * 1024
	info.Name = "seed.bin"
	if err := info.BuildFromFilePath(path); err != nil {
		t.Fatalf("build metainfo info: %v", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	return data, mi, mi.HashInfoBytes()
}

// TestResolveAfterCloseFailsLoud pins the lifecycle contract: a
// closed engine never hands out a reader, even for a ready release.
func TestResolveAfterCloseFailsLoud(t *testing.T) {
	dir := t.TempDir()
	_, mi, ih := seedTorrent(t, dir, 32*1024)

	eng := newTestEngine(t, true)
	eng.cfg.Dir = dir // storage sees the payload → the release is ready instantly
	if _, err := eng.AddMetaInfo(mi); err != nil {
		t.Fatalf("AddMetaInfo: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := eng.Resolve(context.Background(), ih, 0)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Resolve after Close err = %v, want ErrClosed", err)
	}
}

func TestTrackerCheckStopsOnEngineClose(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
		close(released)
	}))
	defer srv.Close()

	e := newTestEngine(t, true)
	e.cfg.Trackers = []string{srv.URL + "/announce"}
	e.probeTimeoutOverride = 10 * time.Second

	e.kickTrackerCheck()
	<-entered
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Engine shutdown must cancel the in-flight probe instead of
	// letting it ride out its full timeout.
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight tracker probe was not cancelled by engine close")
	}
}

// seeds a generated file, client B (same process, distinct data dir
// and port) ingests a magnet with x.pe pointing at A, receives the
// metadata, streams the whole file through Resolve and serves it over
// the engine's loopback HTTP server with full + Range requests.
// TestClientToClientE2E is the offline end-to-end proof: client A
// seeds a generated file, client B (same process, distinct data dir
// and port) ingests a magnet with x.pe pointing at A, receives the
// metadata, streams the whole file through Resolve and serves it over
// the engine's loopback HTTP server with full + Range requests.
func TestClientToClientE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E in short mode")
	}

	dirA := t.TempDir()
	data, mi, ih := seedTorrent(t, dirA, 384*1024)

	engA := newTestEngine(t, true)
	engA.cfg.Dir = dirA // storage must see the payload to seed it

	engB := newTestEngine(t, true)

	ctx := context.Background()
	if _, err := engA.AddMetaInfo(mi); err != nil {
		t.Fatalf("seeder AddMetaInfo: %v", err)
	}
	portA, ok := engA.ListenPort()
	if !ok {
		t.Fatal("seeder client has no listen port")
	}

	magnet := "magnet:?xt=urn:btih:" + ih.HexString() +
		"&dn=seed.bin&x.pe=127.0.0.1:" + itoa(portA)
	if _, err := engB.AddLink(ctx, magnet); err != nil {
		t.Fatalf("leecher AddLink: %v", err)
	}

	// Resolve blocks until the metadata arrives from A, then returns
	// a reader over the file (priorities set by the engine).
	resolveCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	handle, err := engB.Resolve(resolveCtx, ih, 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer func() { _ = handle.Reader.Close() }()

	if handle.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", handle.Size, len(data))
	}
	if handle.Name != "seed.bin" {
		t.Errorf("Name = %q, want seed.bin", handle.Name)
	}
	if handle.MimeType != "application/octet-stream" {
		t.Errorf("MimeType = %q (no known extension), want application/octet-stream", handle.MimeType)
	}

	got, err := io.ReadAll(handle.Reader)
	if err != nil {
		t.Fatalf("read streamed file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("streamed bytes differ from the seeded payload (got %d bytes)", len(got))
	}

	// The HTTP stream server: full request.
	base := engB.StreamURL(ih, 0)
	if base == "" {
		t.Fatal("StreamURL empty after playback request")
	}
	resp, err := http.Get(base) //nolint:gosec,noctx // loopback test server
	if err != nil {
		t.Fatalf("GET %s: %v", base, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("full GET status = %d, want 200", resp.StatusCode)
	}
	if cl := resp.Header.Get("Content-Length"); cl != itoa(len(data)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(data))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !bytes.Equal(body, data) {
		t.Errorf("HTTP body differs from payload (got %d bytes)", len(body))
	}

	// HTTP Range: 0-99.
	expectRange(t, base, "bytes=0-99", http.StatusPartialContent, data[0:100], "bytes 0-99/"+itoa(len(data)))
	// HTTP Range: 100-199.
	expectRange(t, base, "bytes=100-199", http.StatusPartialContent, data[100:200], "bytes 100-199/"+itoa(len(data)))
	// Suffix range: last 100 bytes.
	expectRange(t, base, "bytes=-100", http.StatusPartialContent, data[len(data)-100:], "bytes "+itoa(len(data)-100)+"-"+itoa(len(data)-1)+"/"+itoa(len(data)))
	// Out of range: 416 with the sizing header.
	expectRange(t, base, "bytes="+itoa(len(data))+"-", http.StatusRequestedRangeNotSatisfiable, nil, "bytes */"+itoa(len(data)))
}

// expectRange issues one Range request against base and checks the
// status, served bytes and Content-Range.
func expectRange(t *testing.T, base, spec string, wantStatus int, wantBody []byte, wantCR string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, base, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Range", spec)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s (%s): %v", base, spec, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body (%s): %v", spec, err)
	}
	if resp.StatusCode != wantStatus {
		t.Errorf("%s: status = %d, want %d", spec, resp.StatusCode, wantStatus)
	}
	if wantBody != nil && !bytes.Equal(body, wantBody) {
		t.Errorf("%s: body = %d bytes, want %d exact bytes", spec, len(body), len(wantBody))
	}
	if cr := resp.Header.Get("Content-Range"); cr != wantCR {
		t.Errorf("%s: Content-Range = %q, want %q", spec, cr, wantCR)
	}
}

// itoa avoids strconv imports leaking into every test signature.
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
