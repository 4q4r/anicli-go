package torrent

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeTrackerHTTPAlive(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A real tracker answers a bencode error for a bogus hash —
		// ANY HTTP response proves the tracker is alive.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := probeTracker(context.Background(), srv.URL+"/announce", nil, time.Second); err != nil {
		t.Errorf("probeTracker(alive http) = %v, want nil", err)
	}
}

func TestProbeTrackerHTTPDead(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens anymore

	if err := probeTracker(context.Background(), url+"/announce", nil, time.Second); err == nil {
		t.Error("probeTracker(dead http) = nil, want transport error")
	}
}

// udpTrackerStub answers the BEP 15 connect handshake: request is
// 16 bytes (magic 0x41727101980, action 0, txid), response 8 bytes
// (action 0, txid).
func udpTrackerStub(t *testing.T) (addr string, close func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	go func() {
		buf := make([]byte, 16)
		for {
			n, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 16 || binary.BigEndian.Uint64(buf[:8]) != 0x41727101980 {
				continue
			}
			txid := binary.BigEndian.Uint32(buf[12:16])
			resp := make([]byte, 8)
			binary.BigEndian.PutUint32(resp[0:4], 0)
			binary.BigEndian.PutUint32(resp[4:8], txid)
			_, _ = pc.WriteTo(resp, raddr)
		}
	}()
	return pc.LocalAddr().String(), func() { _ = pc.Close() }
}

func TestProbeTrackerUDPAlive(t *testing.T) {
	t.Parallel()
	addr, stop := udpTrackerStub(t)
	defer stop()

	if err := probeTracker(context.Background(), "udp://"+addr+"/announce", nil, time.Second); err != nil {
		t.Errorf("probeTracker(alive udp) = %v, want nil", err)
	}
}

func TestProbeTrackerUDPDead(t *testing.T) {
	t.Parallel()
	// Nothing listens on that port; the probe must time out with an
	// error (short timeout to keep the suite fast).
	if err := probeTracker(context.Background(), "udp://127.0.0.1:1/announce", nil, 300*time.Millisecond); err == nil {
		t.Error("probeTracker(dead udp) = nil, want timeout error")
	}
}

func TestProbeTrackerUnsupportedScheme(t *testing.T) {
	t.Parallel()
	err := probeTracker(context.Background(), "ftp://tracker/announce", nil, time.Second)
	if err == nil {
		t.Error("probeTracker(ftp) = nil, want unsupported scheme error")
	}
}

func TestProbeTrackerWSSIsTCPLevel(t *testing.T) {
	t.Parallel()
	// Documented limitation: ws/wss health is verified at the
	// TCP/TLS connect level (no websocket client dependency).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := probeTracker(context.Background(), "wss://"+u.Host+"/tracker", nil, time.Second); err != nil {
		t.Errorf("probeTracker(alive wss host) = %v, want nil (tcp-level check)", err)
	}
	if err := probeTracker(context.Background(), "wss://127.0.0.1:1/tracker", nil, 300*time.Millisecond); err == nil {
		t.Error("probeTracker(dead wss host) = nil, want connect error")
	}
}

func TestCheckTrackersPrunesDeadAndReports(t *testing.T) {
	t.Parallel()
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer alive.Close()

	e := newTestEngine(t, true)
	e.cfg.Trackers = []string{
		alive.URL + "/announce",       // alive
		"http://127.0.0.1:1/announce", // dead (refused)
	}

	statuses := e.CheckTrackers(context.Background())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d entries, want 2", len(statuses))
	}
	byURL := map[string]TrackerHealth{}
	for _, st := range statuses {
		byURL[st.URL] = st
	}
	if !byURL[alive.URL+"/announce"].Alive {
		t.Errorf("alive tracker flagged dead: %+v", byURL[alive.URL+"/announce"])
	}
	dead := byURL["http://127.0.0.1:1/announce"]
	if dead.Alive {
		t.Errorf("dead tracker flagged alive: %+v", dead)
	}
	if dead.Reason == "" {
		t.Error("dead tracker must carry its failure reason")
	}
	// The pruned set is what gets injected into new torrents.
	tiers := e.trackerTiersLocked()
	if len(tiers) != 1 || tiers[0][0] != alive.URL+"/announce" {
		t.Errorf("trackerTiers after prune = %v, want only the alive tracker", tiers)
	}
}

func TestCheckTrackersBoundsConcurrency(t *testing.T) {
	t.Parallel()
	var inFlight, maxInFlight atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
	}))
	defer srv.Close()

	e := newTestEngine(t, true)
	e.cfg.Trackers = make([]string, 0, 16)
	for i := range 16 {
		e.cfg.Trackers = append(e.cfg.Trackers, srv.URL+"/"+string(rune('a'+i)))
	}
	e.CheckTrackers(context.Background())
	if got := maxInFlight.Load(); got > maxTrackerProbes {
		t.Errorf("max in-flight probes = %d, want ≤ %d", got, maxTrackerProbes)
	}
}

func TestTrackerTiersBeforeCheckUsesAll(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, true)
	e.cfg.Trackers = []string{
		"udp://tracker.example.org:1337/announce",
		"wss://tracker.example.org/tracker",
	}
	tiers := e.trackerTiersLocked()
	if len(tiers) != 2 {
		t.Fatalf("tiers = %v, want one tier per tracker before the first check", tiers)
	}
}
