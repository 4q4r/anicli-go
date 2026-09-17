package config

import (
	"strings"
	"testing"
)

func TestTorrentDefaults(t *testing.T) {
	t.Parallel()

	got := Default().Torrent

	if !got.Enabled {
		t.Error("Torrent.Enabled = false, want true (usable out of the box; the lazy engine stays idle until a torrent provider resolves)")
	}
	if got.Dir != "" {
		t.Errorf("Torrent.Dir = %q, want empty (auto-resolve under DataDir)", got.Dir)
	}
	if got.Port != 42069 {
		t.Errorf("Torrent.Port = %d, want 42069", got.Port)
	}
	if got.NoUpload {
		t.Error("Torrent.NoUpload = true, want false (leech-only clients starve private trackers and localhost seeds)")
	}
	if got.ReadaheadMB != 32 {
		t.Errorf("Torrent.ReadaheadMB = %d, want 32", got.ReadaheadMB)
	}
	if got.Proxy != "" {
		t.Errorf("Torrent.Proxy = %q, want empty (direct by default)", got.Proxy)
	}
	if len(got.Trackers) != 0 {
		t.Errorf("Torrent.Trackers = %v, want empty (no trackers injected by default)", got.Trackers)
	}
	if len(got.TrackerLists) != 0 {
		t.Errorf("Torrent.TrackerLists = %v, want empty (no external lists fetched by default)", got.TrackerLists)
	}
}

func TestTorrentLoadFile(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[torrent]
enabled = false
dir = "/tmp/torrent-data"
port = 42070
no_upload = true
readahead_mb = 64
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tc := got.Torrent
	if tc.Enabled {
		t.Error("Torrent.Enabled = true, want false (file override)")
	}
	if tc.Dir != "/tmp/torrent-data" {
		t.Errorf("Torrent.Dir = %q, want override", tc.Dir)
	}
	if tc.Port != 42070 {
		t.Errorf("Torrent.Port = %d, want 42070", tc.Port)
	}
	if !tc.NoUpload {
		t.Error("Torrent.NoUpload = false, want true (file override)")
	}
	if tc.ReadaheadMB != 64 {
		t.Errorf("Torrent.ReadaheadMB = %d, want 64", tc.ReadaheadMB)
	}
}

func TestTorrentTrackerListsLoadFile(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[torrent]
enabled = true
tracker_lists = [
    "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt",
    "http://example.org/trackers_best.txt",
]
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{
		"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt",
		"http://example.org/trackers_best.txt",
	}
	if len(got.Torrent.TrackerLists) != len(want) {
		t.Fatalf("Torrent.TrackerLists = %v, want %v", got.Torrent.TrackerLists, want)
	}
	for i := range want {
		if got.Torrent.TrackerLists[i] != want[i] {
			t.Errorf("Torrent.TrackerLists[%d] = %q, want %q", i, got.Torrent.TrackerLists[i], want[i])
		}
	}
}

// TestTorrentTrackerListsValidate pins the startup validation of the
// PR41 list URLs: they are fetched with the shared netclient, so only
// http(s) URLs with a host make sense — everything else fails loud at
// startup instead of surfacing as a fetch error at first torrent add.
func TestTorrentTrackerListsValidate(t *testing.T) {
	t.Parallel()

	for u, ok := range map[string]bool{
		"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt": true,
		"http://example.org/trackers_best.txt":                                           true,
		"ftp://example.org/trackers.txt":                                                 false,
		"example.org/trackers.txt":                                                       false,
		"http://":                                                                        false,
	} {
		s := Default()
		s.Torrent.TrackerLists = []string{u}
		err := s.Validate()
		if ok != (err == nil) {
			t.Errorf("Validate tracker_lists %q: err = %v, want valid=%v", u, err, ok)
		}
		if !ok && err != nil && !strings.Contains(err.Error(), "torrent.tracker_lists") {
			t.Errorf("Validate tracker_lists %q: error %v must name torrent.tracker_lists", u, err)
		}
	}
}

// TestTorrentLinksKeyFailsLoud pins the PR40 removal: the `links`
// ingestion list is gone (its only consumer, the «Торренты» menu, was
// superseded by the torrent search providers), so a settings file that
// still carries the key must fail loud with the offending key path —
// never silently ignored.
func TestTorrentLinksKeyFailsLoud(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[torrent]
enabled = true
links = ["magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"]
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with the removed torrent.links key must fail loud")
	}
	if !strings.Contains(err.Error(), "torrent.links") {
		t.Errorf("error = %v, want it to name the removed key torrent.links", err)
	}
}

func TestTorrentUnknownKeyFailsLoud(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[torrent]
enabld = true
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with unknown [torrent] key must fail loud")
	}
	if !strings.Contains(err.Error(), "torrent.enabld") {
		t.Errorf("error = %v, want it to name the offending key torrent.enabld", err)
	}
}

func TestTorrentValidate(t *testing.T) {
	t.Parallel()

	t.Run("port out of range", func(t *testing.T) {
		t.Parallel()
		s := Default()
		s.Torrent.Port = 70000
		if err := s.Validate(); err == nil {
			t.Error("Validate with port 70000 must fail")
		}
	})
	t.Run("port zero ok (ephemeral)", func(t *testing.T) {
		t.Parallel()
		s := Default()
		s.Torrent.Port = 0
		if err := s.Validate(); err != nil {
			t.Errorf("Validate with port 0: %v", err)
		}
	})
	t.Run("negative readahead", func(t *testing.T) {
		t.Parallel()
		s := Default()
		s.Torrent.ReadaheadMB = -1
		if err := s.Validate(); err == nil {
			t.Error("Validate with readahead_mb = -1 must fail")
		}
	})
	t.Run("proxy schemes", func(t *testing.T) {
		t.Parallel()
		for scheme, ok := range map[string]bool{
			"http://127.0.0.1:10809":    true,
			"https://proxy.example.org": true,
			"socks5://127.0.0.1:9050":   true,
			"ftp://127.0.0.1:21":        false,
			"not-a-url":                 false,
		} {
			s := Default()
			s.Torrent.Proxy = scheme
			err := s.Validate()
			if ok != (err == nil) {
				t.Errorf("Validate proxy %q: err = %v, want valid=%v", scheme, err, ok)
			}
		}
	})
	t.Run("tracker schemes", func(t *testing.T) {
		t.Parallel()
		for u, ok := range map[string]bool{
			"udp://tracker.example.org:1337/announce": true,
			"http://tracker.example.org/announce":     true,
			"https://tracker.example.org/announce":    true,
			"wss://tracker.example.org/tracker":       true,
			"wss://tracker.example.org:443/tracker":   true,
			"ftp://tracker.example.org/announce":      false,
			"tracker.example.org/announce":            false,
			"http://":                                 false,
		} {
			s := Default()
			s.Torrent.Trackers = []string{u}
			err := s.Validate()
			if ok != (err == nil) {
				t.Errorf("Validate tracker %q: err = %v, want valid=%v", u, err, ok)
			}
		}
	})
}
