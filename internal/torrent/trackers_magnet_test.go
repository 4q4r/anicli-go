package torrent

// PR45 ITEM 2 tests: the engine's tracker pool must reach torrents
// added via synthesized tracker-less magnets (the animetosho/
// anilibria-torrent link shape), both at the spec level (addSpec
// attach) and as a queryable pool for magnet building.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// trackerAnnounceStub is a loopback HTTP tracker that counts real
// announces (info_hash query present); announces get a bencode failure
// body — a dead torrent is fine, the announce itself is the proof.
// The returned tracker URL carries the /announce path.
func trackerAnnounceStub(t *testing.T) (url string, hits func() int) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/announce") && r.URL.Query().Get("info_hash") != "" {
			count.Add(1)
		}
		_, _ = w.Write([]byte("d14:failure reason4:teste"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/announce", func() int { return int(count.Load()) }
}

// TestEngineHealthyTrackersMirrorsPool pins the flat pool accessor:
// configured trackers come back (fail-open before any check, deduped);
// a health-check prune excludes the dead ones; an unconfigured engine
// returns nothing (no invented defaults).
func TestEngineHealthyTrackersMirrorsPool(t *testing.T) {
	t.Parallel()

	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(alive.Close)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL + "/gone"
	dead.Close()

	eng := newTestEngine(t, true)
	if got := eng.HealthyTrackers(); len(got) != 0 {
		t.Errorf("unconfigured pool = %v, want empty", got)
	}

	eng.mu.Lock()
	eng.cfg.Trackers = []string{alive.URL + "/announce", deadURL, alive.URL + "/announce"}
	eng.mu.Unlock()

	if got := eng.HealthyTrackers(); len(got) != 2 {
		t.Errorf("pool before check = %v, want deduped 2 (fail-open)", got)
	}

	if sts := eng.CheckTrackers(context.Background()); len(sts) != 3 {
		t.Errorf("CheckTrackers = %v, want 3 reports", sts)
	}
	got := eng.HealthyTrackers()
	if len(got) != 1 || got[0] != alive.URL+"/announce" {
		t.Errorf("pool after prune = %v, want the single alive tracker", got)
	}
}

// TestEngineMagnetIngestAnnouncesConfiguredTracker verifies the magnet
// path benefits from the spec-level tracker attach: a torrent added
// from a synthesized tracker-less magnet must announce to a configured
// tracker (metadata then arrives via the tracker, not DHT-only).
func TestEngineMagnetIngestAnnouncesConfiguredTracker(t *testing.T) {
	t.Parallel()

	trackerURL, hits := trackerAnnounceStub(t)
	eng := newNetTestEngine(t)
	eng.mu.Lock()
	eng.cfg.Trackers = []string{trackerURL}
	eng.mu.Unlock()

	if _, err := eng.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH+"&dn=PR45"); err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for hits() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("magnet-added torrent never announced the configured tracker")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
