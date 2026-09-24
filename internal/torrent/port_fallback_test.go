package torrent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// syncBuffer is a concurrency-safe log sink: the engine logs from
// background goroutines (awaitInfo, tracker kicks), not only from the
// caller's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// occupyPort binds 127.0.0.1:0 and KEEPS the listener open for the
// whole test: every later bind of the returned port conflicts
// deterministically (a wildcard :port bind cannot coexist with a
// specific-address bind of the same port). This is the live-incident
// shape: the owner's TUI squatting :42069 while a second anicli starts.
func occupyPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// freePort returns a port that is free right now (best effort: the
// usual listen/close race applies, so callers retry on conflict).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestBindConflictFallsBackToEphemeral pins the PR100 contract: a busy
// configured BT port must NOT kill the client (the live incident — a
// second anicli instance or any app on :42069 degraded EVERY torrent
// provider to FAIL). The engine must start on an OS-assigned ephemeral
// port, log the LOUD fallback WARN (with the actually bound port) and
// stay fully functional.
func TestBindConflictFallsBackToEphemeral(t *testing.T) {
	t.Parallel()

	busy := occupyPort(t)
	logs := &syncBuffer{}
	eng := NewEngine(config.Torrent{
		Enabled:     true,
		Dir:         t.TempDir(),
		Port:        busy,
		ReadaheadMB: 1,
	}, nil, slog.New(slog.NewTextHandler(logs, nil)))
	eng.testNoExternal = true
	t.Cleanup(func() { _ = eng.Close() })

	// The bind conflict fires on the first lazy start — exactly the
	// path that used to fail loud with "first listen: ... address
	// already in use".
	rel, err := eng.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH+"&dn=fallback")
	if err != nil {
		t.Fatalf("AddLink on busy port %d: %v (engine must fall back to an ephemeral port, not die)", busy, err)
	}
	if rel.Status != StatusFetching {
		t.Errorf("Status = %q, want %q (client started, metadata pending)", rel.Status, StatusFetching)
	}

	port, ok := eng.ListenPort()
	if !ok {
		t.Fatal("ListenPort() = false after fallback, want the actually bound ephemeral port")
	}
	if port == busy {
		t.Errorf("ListenPort() = %d, want a port distinct from the busy %d", port, busy)
	}
	if port == 0 {
		t.Error("ListenPort() = 0, want a real ephemeral port")
	}

	// The WARN must be LOUD and carry the mandated wording, the busy
	// port and the actually bound ephemeral port (it is what the user
	// needs to debug inbound-peer limitations and router/NAT setups).
	got := logs.String()
	if !strings.Contains(got, "занят — слушаем на случайном") {
		t.Errorf("fallback WARN missing the mandated wording, logs:\n%s", got)
	}
	if !strings.Contains(got, "входящие пиры ограничены") {
		t.Errorf("fallback WARN missing the inbound-peers limitation note, logs:\n%s", got)
	}
	if !strings.Contains(got, "исходящие DHT/пиры работают с любого порта") {
		t.Errorf("fallback WARN missing the outgoing-still-work note, logs:\n%s", got)
	}
	if !strings.Contains(got, itoa(busy)) {
		t.Errorf("fallback WARN must name the busy port %d, logs:\n%s", busy, got)
	}
	if !strings.Contains(got, "ephemeral_port="+itoa(port)) {
		t.Errorf("fallback WARN must log the actually bound port %d, logs:\n%s", port, got)
	}
}

// TestBindConflictEngineStillStreamsFromPeer proves the fallback
// engine is not just "started" but FUNCTIONAL: it falls back to an
// ephemeral port and still completes a client-to-client transfer
// (seeder on its own ephemeral port, leecher on the conflicted-then-
// ephemeral one) — outgoing peer connections work from any port, which
// is the whole reason the fallback is acceptable.
func TestBindConflictEngineStillStreamsFromPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E in short mode")
	}
	t.Parallel()

	busy := occupyPort(t)
	dirA := t.TempDir()
	data, mi, ih := seedTorrent(t, dirA, 128*1024)

	engA := newTestEngine(t, true)
	engA.cfg.Dir = dirA // storage must see the payload to seed it

	logs := &syncBuffer{}
	engB := NewEngine(config.Torrent{
		Enabled:     true,
		Dir:         t.TempDir(),
		Port:        busy, // the second-instance bind conflict
		ReadaheadMB: 1,
	}, nil, slog.New(slog.NewTextHandler(logs, nil)))
	engB.testNoExternal = true
	t.Cleanup(func() { _ = engB.Close() })

	if _, err := engA.AddMetaInfo(mi); err != nil {
		t.Fatalf("seeder AddMetaInfo: %v", err)
	}
	portA, ok := engA.ListenPort()
	if !ok {
		t.Fatal("seeder has no listen port")
	}

	// AddLink is the lazy-start trigger: the bind conflict fires here,
	// the engine falls back to an ephemeral port and the client starts.
	magnet := "magnet:?xt=urn:btih:" + ih.HexString() +
		"&dn=seed.bin&x.pe=127.0.0.1:" + itoa(portA)
	if _, err := engB.AddLink(context.Background(), magnet); err != nil {
		t.Fatalf("leecher AddLink (fallback engine): %v", err)
	}
	portB, ok := engB.ListenPort()
	if !ok {
		t.Fatal("leecher has no listen port (fallback did not start the client)")
	}
	if portB == busy {
		t.Fatalf("leecher bound the busy port %d, want the ephemeral fallback", busy)
	}

	resolveCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	handle, err := engB.Resolve(resolveCtx, ih, 0)
	if err != nil {
		t.Fatalf("Resolve on fallback engine: %v", err)
	}
	defer func() { _ = handle.Reader.Close() }()

	got, err := io.ReadAll(handle.Reader)
	if err != nil {
		t.Fatalf("read streamed file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("streamed bytes differ from the seeded payload (got %d bytes, want %d)", len(got), len(data))
	}
	if !strings.Contains(logs.String(), "занят — слушаем на случайном") {
		t.Errorf("fallback WARN not logged during the E2E, logs:\n%s", logs.String())
	}
}

// TestEphemeralEnginesGetDistinctPorts is the ephemeral-vs-ephemeral
// proof behind the fallback: two engines both configured port=0 (or
// both fallen back) get DISTINCT OS-assigned ports, and a
// client-to-client transfer between them works end to end — trackers
// and peers see whatever port the client actually bound (the library
// announces from its own listeners).
func TestEphemeralEnginesGetDistinctPorts(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E in short mode")
	}
	t.Parallel()

	dirA := t.TempDir()
	data, mi, ih := seedTorrent(t, dirA, 128*1024)

	engA := newTestEngine(t, true) // Port: 0
	engA.cfg.Dir = dirA
	engB := newTestEngine(t, true) // Port: 0

	if _, err := engA.AddMetaInfo(mi); err != nil {
		t.Fatalf("seeder AddMetaInfo: %v", err)
	}
	portA, ok := engA.ListenPort()
	if !ok || portA == 0 {
		t.Fatalf("seeder ListenPort() = (%d, %v), want a real ephemeral port", portA, ok)
	}

	magnet := "magnet:?xt=urn:btih:" + ih.HexString() +
		"&dn=seed.bin&x.pe=127.0.0.1:" + itoa(portA)
	if _, err := engB.AddLink(context.Background(), magnet); err != nil {
		t.Fatalf("leecher AddLink: %v", err)
	}
	portB, ok := engB.ListenPort()
	if !ok || portB == 0 {
		t.Fatalf("leecher ListenPort() = (%d, %v), want a real ephemeral port", portB, ok)
	}
	if portA == portB {
		t.Errorf("both ephemeral engines bound port %d, want distinct ports", portA)
	}

	resolveCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	handle, err := engB.Resolve(resolveCtx, ih, 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	defer func() { _ = handle.Reader.Close() }()

	got, err := io.ReadAll(handle.Reader)
	if err != nil {
		t.Fatalf("read streamed file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("streamed bytes differ from the seeded payload (got %d bytes, want %d)", len(got), len(data))
	}
}

// TestConfiguredPortBindsWhenFree is the regression guard: the
// fallback must never hijack the happy path — with the configured port
// free, the client binds EXACTLY that port (no WARN, no drift).
func TestConfiguredPortBindsWhenFree(t *testing.T) {
	t.Parallel()

	// Two shots: the listen/close race (someone else grabbing the port
	// between freePort and the engine start) must not fail the test —
	// a stolen port now produces the fallback, detectable as
	// ListenPort() != configured.
	for range 2 {
		want := freePort(t)
		logs := &syncBuffer{}
		eng := NewEngine(config.Torrent{
			Enabled:     true,
			Dir:         t.TempDir(),
			Port:        want,
			ReadaheadMB: 1,
		}, nil, slog.New(slog.NewTextHandler(logs, nil)))
		eng.testNoExternal = true

		_, err := eng.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH)
		if err != nil {
			t.Fatalf("AddLink on free port %d: %v", want, err)
		}
		got, ok := eng.ListenPort()
		if !ok {
			t.Fatal("ListenPort() = false, want the configured port")
		}
		if got == want {
			if strings.Contains(logs.String(), "занят — слушаем на случайном") {
				t.Errorf("configured-port bind must not log the fallback WARN, logs:\n%s", logs.String())
			}
			return // bound exactly the configured port
		}
		// Port stolen in the race window: the engine legitimately fell
		// back — retry with a fresh free port.
		_ = eng.Close()
	}
	t.Fatal("twice failed to observe the configured-port bind (port race window)")
}
