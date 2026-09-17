package torrent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ngosangFixtureSample is captured verbatim from
// https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt
// on 2026-09-17 (tracker URLs are plain facts; the ngosang/trackerslist
// repo publishes them as a freely usable public list). The blank lines
// between entries are part of the real format. Per the PR41 spec the
// sample is then EXTENDED below with whole-line and inline comments,
// an invalid-scheme line, a magnet line and mixed-case duplicates so
// every parser rule is pinned against the format users actually feed in.
const ngosangFixtureSample = `http://tracker.opentrackr.org:1337/announce

udp://tracker.opentrackr.org:1337/announce

udp://open.stealth.si:80/announce

udp://tracker.torrent.eu.org:451/announce

udp://open.demonii.com:1337/announce

http://tracker.qu.ax:6969/announce

udp://exodus.desync.com:6969/announce

https://tracker.onetracker.net:443/announce
`

const ngosangFixtureExtended = `# ngosang/trackerslist — trackers_all (updated daily)
# blank lines and comments must be skipped

http://tracker.opentrackr.org:1337/announce

udp://tracker.opentrackr.org:1337/announce

udp://open.stealth.si:80/announce

UDP://OPEN.STEALTH.SI:80/announce

udp://tracker.torrent.eu.org:451/announce

udp://open.demonii.com:1337/announce

http://tracker.qu.ax:6969/announce # also answers on udp://tracker.qu.ax:6969

HTTP://TRACKER.QU.AX:6969/announce

udp://exodus.desync.com:6969/announce

https://tracker.onetracker.net:443/announce

ws://tracker.example.org:8080/tracker
wss://tracker.example.org/tracker

ftp://tracker.example.org:21/announce

magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567

just some garbage without a scheme

http://
`

// TestParseTrackerListNgosangFixture pins the whole parser against the
// real ngosang format plus the PR41-mandated edge lines: trim, blank
// skip, '#' comments (whole-line and inline), all five accepted
// schemes, case-insensitive scheme+host dedupe and per-line fail-soft
// rejection with reasons.
func TestParseTrackerListNgosangFixture(t *testing.T) {
	t.Parallel()

	trackers, skips := parseTrackerList(ngosangFixtureExtended)

	// The parser is faithful, not deduping: the mixed-case duplicates
	// survive parse and collapse later at merge time (first-seen form
	// kept verbatim).
	wantTrackers := []string{
		"http://tracker.opentrackr.org:1337/announce",
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.stealth.si:80/announce",
		"UDP://OPEN.STEALTH.SI:80/announce",
		"udp://tracker.torrent.eu.org:451/announce",
		"udp://open.demonii.com:1337/announce",
		"http://tracker.qu.ax:6969/announce",
		"HTTP://TRACKER.QU.AX:6969/announce",
		"udp://exodus.desync.com:6969/announce",
		"https://tracker.onetracker.net:443/announce",
		"ws://tracker.example.org:8080/tracker",
		"wss://tracker.example.org/tracker",
	}
	if len(trackers) != len(wantTrackers) {
		t.Fatalf("trackers = %v, want %d entries", trackers, len(wantTrackers))
	}
	for i := range wantTrackers {
		if trackers[i] != wantTrackers[i] {
			t.Errorf("trackers[%d] = %q, want %q", i, trackers[i], wantTrackers[i])
		}
	}

	// Rejected: the ftp line, the magnet line, the garbage line and
	// the host-less http:// — each with a logged reason.
	if len(skips) != 4 {
		t.Fatalf("skips = %v, want 4 entries", skips)
	}
	for _, sk := range skips {
		if sk.Line < 1 {
			t.Errorf("skip %+v: Line must be 1-based and positive", sk)
		}
		if sk.Reason == "" {
			t.Errorf("skip %+v: must carry a rejection reason", sk)
		}
	}
	gotText := map[string]bool{}
	for _, sk := range skips {
		gotText[sk.Text] = true
	}
	for _, want := range []string{
		"ftp://tracker.example.org:21/announce",
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"just some garbage without a scheme",
		"http://",
	} {
		if !gotText[want] {
			t.Errorf("skips miss the rejected line %q (got %v)", want, gotText)
		}
	}
}

// TestParseTrackerListUnmodifiedSample: the pristine capture parses
// without any rejects — the real feed must never produce noise.
func TestParseTrackerListUnmodifiedSample(t *testing.T) {
	t.Parallel()

	trackers, skips := parseTrackerList(ngosangFixtureSample)
	if len(skips) != 0 {
		t.Errorf("skips = %v, want none (real ngosang feed is clean)", skips)
	}
	if len(trackers) != 8 {
		t.Errorf("trackers = %d entries, want 8", len(trackers))
	}
}

func TestParseTrackerListRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		line     string
		want     string // accepted tracker form; empty means not accepted
		silent   bool   // dropped without a skip record (blank/comment)
		inReason string // substring expected in the rejection reason
	}{
		{name: "plain udp", line: "udp://tracker.example.org:1337/announce", want: "udp://tracker.example.org:1337/announce"},
		{name: "plain http", line: "http://tracker.example.org/announce", want: "http://tracker.example.org/announce"},
		{name: "plain https", line: "https://tracker.example.org/announce", want: "https://tracker.example.org/announce"},
		{name: "plain ws", line: "ws://tracker.example.org:8080/tracker", want: "ws://tracker.example.org:8080/tracker"},
		{name: "plain wss", line: "wss://tracker.example.org/tracker", want: "wss://tracker.example.org/tracker"},
		{name: "surrounding whitespace trimmed", line: "  udp://tracker.example.org:1337/announce\t", want: "udp://tracker.example.org:1337/announce"},
		{name: "blank line skipped", line: "   ", silent: true},
		{name: "whole-line comment skipped", line: "# udp://tracker.example.org:1337/announce", silent: true},
		{name: "indented comment skipped", line: "   # comment", silent: true},
		{name: "inline comment stripped", line: "udp://tracker.example.org:1337/announce # dead since 2024", want: "udp://tracker.example.org:1337/announce"},
		{name: "comment-only after strip", line: "   # note", silent: true},
		{name: "url fragment preserved", line: "udp://tracker.example.org:1337/announce#x", want: "udp://tracker.example.org:1337/announce#x"},
		{name: "uppercase scheme kept as-is", line: "UDP://tracker.example.org:1337/announce", want: "UDP://tracker.example.org:1337/announce"},
		{name: "ftp rejected", line: "ftp://tracker.example.org/announce", inReason: "unsupported scheme"},
		{name: "magnet rejected", line: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", inReason: "unsupported scheme"},
		{name: "no scheme rejected", line: "tracker.example.org/announce", inReason: "unsupported scheme"},
		{name: "missing host rejected", line: "http://", inReason: "missing host"},
		{name: "udp missing host rejected", line: "udp://", inReason: "missing host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			trackers, skips := parseTrackerList(tt.line)
			if tt.want != "" {
				if len(trackers) != 1 || trackers[0] != tt.want {
					t.Errorf("trackers = %v, want [%s]", trackers, tt.want)
				}
				if len(skips) != 0 {
					t.Errorf("skips = %v, want none", skips)
				}
				return
			}
			if len(trackers) != 0 {
				t.Errorf("trackers = %v, want none", trackers)
			}
			if tt.silent {
				if len(skips) != 0 {
					t.Errorf("skips = %v, want none (blank/comment lines drop silently)", skips)
				}
				return
			}
			if len(skips) != 1 {
				t.Fatalf("skips = %v, want exactly 1", skips)
			}
			if tt.inReason != "" && !strings.Contains(skips[0].Reason, tt.inReason) {
				t.Errorf("reason = %q, want it to contain %q", skips[0].Reason, tt.inReason)
			}
		})
	}
}

// TestTrackerKeyCaseInsensitive pins the dedupe key: scheme+host,
// case-insensitive, path-insensitive (the PR41 "enough" ruling).
func TestTrackerKeyCaseInsensitive(t *testing.T) {
	t.Parallel()

	a := trackerKey("UDP://Tracker.Example.ORG:1337/announce")
	b := trackerKey("udp://tracker.example.org:1337/other")
	if a != b {
		t.Errorf("trackerKey mismatch: %q vs %q (scheme+host must be case-insensitive)", a, b)
	}
	c := trackerKey("udp://tracker.example.org:1338/announce")
	if a == c {
		t.Errorf("trackerKey %q must differ from %q (port is part of the host)", a, c)
	}
}

// localListFixture renders an ngosang-format list over LOOPBACK
// tracker URLs — the hermetic twin of ngosangFixtureExtended: same
// comment/blank/inline-comment/mixed-case/invalid-line structure, but
// every real announce URL is replaced by local addresses so the
// engine's embedded health check never leaves the machine.
func localListFixture(aliveSrvURL string) string {
	return `# loopback twin of the ngosang trackers_all format
# comments and blanks must be skipped

` + aliveSrvURL + `/announce

udp://127.0.0.1:1/announce

HTTP://` + strings.TrimPrefix(strings.TrimPrefix(aliveSrvURL, "http://"), "https://") + `/announce

ftp://127.0.0.1:21/announce

magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567

just some garbage without a scheme

http://
`
}

// trackerAnnounceStub is a loopback HTTP "tracker": it counts real
// ANNOUNCE requests (the query carries info_hash — the health probe's
// plain GET does not) and answers a minimal bencode failure (any
// response proves the round trip).
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
	return srv.URL, func() int { return int(count.Load()) }
}

// fetchTrackerListsStatus waits until the engine's tracker-list
// statuses become non-empty (the background fetch merged) and returns
// them; fails the test after the deadline instead of racing.
func fetchTrackerListsStatus(t *testing.T, e *Engine) []TrackerListStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sts := e.TrackerListStatuses(); len(sts) > 0 {
			return sts
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("tracker-list statuses never appeared (background fetch did not run)")
	return nil
}

// TestEngineFetchesAndMergesTrackerLists: the full PR41 pipeline — an
// httptest list server serves an ngosang-format list; the engine
// fetches it, parses with per-line fail-soft, dedupes against the
// static pool (case-insensitive scheme+host) and feeds the merged pool
// into the EXISTING health check, which prunes the dead entries.
func TestEngineFetchesAndMergesTrackerLists(t *testing.T) {
	t.Parallel()

	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer alive.Close()
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(localListFixture(alive.URL)))
	}))
	defer list.Close()

	e := newNetTestEngine(t)
	e.cfg.Trackers = []string{"udp://127.0.0.1:1/announce"} // static dead, list twin must dedupe away
	e.cfg.TrackerLists = []string{list.URL + "/trackers_all.txt"}
	e.probeTimeoutOverride = 300 * time.Millisecond

	e.fetchTrackerLists(context.Background())

	// Per-URL fetch status is visible and healthy.
	sts := e.TrackerListStatuses()
	if len(sts) != 1 {
		t.Fatalf("TrackerListStatuses() = %v, want 1 entry", sts)
	}
	st := sts[0]
	if !st.OK {
		t.Fatalf("list status = %+v, want OK", st)
	}
	// Accepted lines: alive http, dead udp, mixed-case dup (3); the
	// dup-case line counts at parse level — dedupe happens at merge.
	if st.Trackers != 3 {
		t.Errorf("status.Trackers = %d, want 3 accepted lines", st.Trackers)
	}
	if st.Skipped != 4 {
		t.Errorf("status.Skipped = %d, want 4 rejected lines", st.Skipped)
	}

	// Merged pool: static first, then the deduped list additions (the
	// mixed-case twin and the static-colliding udp twin collapse).
	e.mu.Lock()
	pool := append([]string(nil), e.listTrackers...)
	e.mu.Unlock()
	wantNew := []string{alive.URL + "/announce"}
	if len(pool) != len(wantNew) {
		t.Fatalf("merged list additions = %v, want %v", pool, wantNew)
	}
	for i := range wantNew {
		if pool[i] != wantNew[i] {
			t.Errorf("merged[%d] = %q, want %q", i, pool[i], wantNew[i])
		}
	}

	// The existing health-check mechanism consumed the merged pool:
	// the dead entries (static udp twin included) are pruned, the
	// alive loopback tracker survives.
	tiers := e.trackerTiersLocked()
	if len(tiers) != 1 || tiers[0][0] != alive.URL+"/announce" {
		t.Errorf("tiers after prune = %v, want only the alive tracker %s", tiers, alive.URL+"/announce")
	}
}

// TestEngineAddTrackersToLiveTorrent: a torrent added BEFORE the lists
// merge still receives the fetched trackers — the merge retro-attaches
// them via the library (the "with first torrent add" half of the lazy
// contract). Proven behaviorally: the loopback tracker stub observes
// the library announcing with our infohash.
func TestEngineAddTrackersToLiveTorrent(t *testing.T) {
	t.Parallel()

	trackerURL, trackerHits := trackerAnnounceStub(t)
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("http://" + strings.TrimPrefix(trackerURL, "http://") + "/announce\n"))
	}))
	defer list.Close()

	e := newNetTestEngine(t)
	e.probeTimeoutOverride = 300 * time.Millisecond
	// The torrent enters before any tracker exists in the pool.
	if _, err := e.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH); err != nil {
		t.Fatalf("AddLink: %v", err)
	}
	e.cfg.TrackerLists = []string{list.URL + "/list.txt"}
	e.fetchTrackerLists(context.Background())

	// The library must announce the retro-attached tracker with our
	// infohash (first announce fires promptly after AddTrackers).
	deadline := time.Now().Add(15 * time.Second)
	for trackerHits() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("live torrent never announced the retro-attached tracker")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestEngineTrackerListFetchFailTypedError: a failed list URL surfaces
// as a typed error in the per-URL status (and the log) while the
// static trackers keep working — degradation, never failure.
func TestEngineTrackerListFetchFailTypedError(t *testing.T) {
	t.Parallel()

	static := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer static.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := dead.URL + "/gone.txt"
	dead.Close() // nothing listens anymore

	e := newNetTestEngine(t)
	e.cfg.Trackers = []string{static.URL + "/announce"}
	e.cfg.TrackerLists = []string{deadURL}
	e.probeTimeoutOverride = 300 * time.Millisecond

	e.fetchTrackerLists(context.Background())

	sts := e.TrackerListStatuses()
	if len(sts) != 1 {
		t.Fatalf("TrackerListStatuses() = %v, want 1 entry", sts)
	}
	st := sts[0]
	if st.OK {
		t.Fatalf("status = %+v, want failure", st)
	}
	var fetchErr *TrackerListFetchError
	if !errors.As(st.Err, &fetchErr) {
		t.Fatalf("status.Err = %v, want a *TrackerListFetchError", st.Err)
	}
	if fetchErr.URL != deadURL {
		t.Errorf("fetchErr.URL = %q, want %q", fetchErr.URL, deadURL)
	}

	// Static trackers are unaffected: the alive one survives the
	// health check the merge kicked.
	tiers := e.trackerTiersLocked()
	if len(tiers) != 1 || tiers[0][0] != static.URL+"/announce" {
		t.Errorf("tiers = %v, want only the static tracker %s", tiers, static.URL+"/announce")
	}
}

// TestEngineTrackerListsWithoutNetclient: an engine built without the
// shared netclient cannot fetch lists — every configured URL gets its
// typed failure, static trackers keep working.
func TestEngineTrackerListsWithoutNetclient(t *testing.T) {
	t.Parallel()

	e := newTestEngine(t, true)
	e.cfg.TrackerLists = []string{"http://example.org/trackers.txt"}

	e.fetchTrackerLists(context.Background())

	sts := e.TrackerListStatuses()
	if len(sts) != 1 {
		t.Fatalf("TrackerListStatuses() = %v, want 1 entry", sts)
	}
	if sts[0].OK {
		t.Fatalf("status = %+v, want failure (no netclient)", sts[0])
	}
	var fetchErr *TrackerListFetchError
	if !errors.As(sts[0].Err, &fetchErr) {
		t.Fatalf("status.Err = %v, want a *TrackerListFetchError", sts[0].Err)
	}
}

// TestEngineIdleDoesNotFetchTrackerLists pins the lazy discipline for
// an ENABLED engine: no torrent added → no client → no fetch, even
// with tracker_lists configured.
func TestEngineIdleDoesNotFetchTrackerLists(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	e := newNetTestEngine(t)
	e.cfg.TrackerLists = []string{srv.URL + "/list.txt"}

	// Give a mis-wired background fetch ample time to (wrongly) fire.
	time.Sleep(300 * time.Millisecond)

	if got := hits.Load(); got != 0 {
		t.Errorf("idle engine fetched tracker lists %d times, want 0", got)
	}
	if sts := e.TrackerListStatuses(); len(sts) != 0 {
		t.Errorf("TrackerListStatuses() = %v, want empty (nothing fetched)", sts)
	}
}

// TestEngineDisabledDoesNotFetchTrackerLists: a disabled engine never
// starts anything — AddLink fails loud with ErrDisabled and the list
// URLs are never touched.
func TestEngineDisabledDoesNotFetchTrackerLists(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	e := newTestEngine(t, false)
	e.cfg.TrackerLists = []string{srv.URL + "/list.txt"}

	if _, err := e.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH); !errors.Is(err, ErrDisabled) {
		t.Errorf("AddLink err = %v, want ErrDisabled", err)
	}
	time.Sleep(300 * time.Millisecond)

	if got := hits.Load(); got != 0 {
		t.Errorf("disabled engine fetched tracker lists %d times, want 0", got)
	}
	if sts := e.TrackerListStatuses(); len(sts) != 0 {
		t.Errorf("TrackerListStatuses() = %v, want empty", sts)
	}
}

// TestEngineStartKicksTrackerListFetch pins the startClientLocked
// wiring: the first torrent add starts the client AND the background
// list fetch — statuses appear without any manual call.
func TestEngineStartKicksTrackerListFetch(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	list := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("udp://127.0.0.1:1/announce\n"))
	}))
	defer list.Close()

	e := newNetTestEngine(t)
	e.probeTimeoutOverride = 300 * time.Millisecond
	e.cfg.TrackerLists = []string{list.URL + "/list.txt"}

	if _, err := e.AddLink(context.Background(), "magnet:?xt=urn:btih:"+testHexIH); err != nil {
		t.Fatalf("AddLink: %v", err)
	}

	fetchTrackerListsStatus(t, e)
	if got := hits.Load(); got != 1 {
		t.Errorf("list fetch count = %d, want exactly 1 (one GET per start)", got)
	}
	time.Sleep(200 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Errorf("list fetched %d times after settle, want exactly 1 (no refetch)", got)
	}
}
