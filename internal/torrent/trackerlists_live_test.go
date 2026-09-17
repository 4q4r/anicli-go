//go:build live

package torrent

// LIVE probe for the PR41 tracker-list feeds. Excluded from the
// hermetic default suite by the `live` build tag (the offline-suite
// ruling: `go test -race ./...` never egresses). Run manually:
//
//	go test -tags live -run TestLiveNgosangTrackerList -count=1 -v ./internal/torrent/
//
// Optional proxy for foreign networks:
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 go test -tags live ...
//
// The probe exercises the REAL production path: netclient GET of the
// ngosang trackers_all feed → parseTrackerList → merge into the pool
// → one real CheckTrackers round (BEP 15 handshakes and HTTP GETs
// against actual public trackers) — and prints the healthy-tracker
// count.

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

const liveNgosangURL = "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt"

func TestLiveNgosangTrackerList(t *testing.T) {
	netCfg := config.Default().Network
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		netCfg.ProxyURL = proxy
		t.Logf("route: via proxy %s", proxy)
	} else {
		t.Log("route: direct")
	}
	net, err := netclient.New(netCfg, netclient.WithProvider("torrent-live"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := config.Torrent{
		Enabled:      true,
		Dir:          t.TempDir(),
		Port:         0,
		TrackerLists: []string{liveNgosangURL},
	}
	e := NewEngine(cfg, net, log)
	defer func() { _ = e.Close() }()

	// Fetch+parse+merge synchronously (the engine does the same in the
	// background at first torrent add) — this probes nothing yet, the
	// merged-pool health check inside the fetch does.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e.fetchTrackerLists(ctx)

	sts := e.TrackerListStatuses()
	if len(sts) != 1 {
		t.Fatalf("TrackerListStatuses() = %d entries, want 1", len(sts))
	}
	st := sts[0]
	if !st.OK {
		t.Fatalf("list fetch failed: %v", st.Err)
	}
	t.Logf("fetched %s: accepted=%d skipped=%d", st.URL, st.Trackers, st.Skipped)

	health := e.TrackerStatuses()
	alive := 0
	for _, h := range health {
		if h.Alive {
			alive++
		}
	}
	t.Logf("health check: %d trackers probed, %d healthy, %d pruned", len(health), alive, len(health)-alive)
	for _, h := range health {
		if !h.Alive {
			t.Logf("pruned: %s (%s)", h.URL, h.Reason)
		}
	}
	if alive == 0 {
		t.Errorf("0 healthy trackers from the live round — network or parser problem")
	}
}
