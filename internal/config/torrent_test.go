package config

import (
	"strings"
	"testing"
)

func TestTorrentDefaults(t *testing.T) {
	t.Parallel()

	got := Default().Torrent

	if !got.Enabled {
		t.Error("Torrent.Enabled = false, want true (usable out of the box; empty links keep the engine idle)")
	}
	if len(got.Links) != 0 {
		t.Errorf("Torrent.Links = %v, want empty (engine stays idle until links are configured)", got.Links)
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
}

func TestTorrentLoadFile(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[torrent]
enabled = false
links = [
    "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Title",
    "https://example.org/release.torrent",
]
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
	if len(tc.Links) != 2 {
		t.Fatalf("Torrent.Links = %v, want 2 links", tc.Links)
	}
	if tc.Links[0] != "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Title" {
		t.Errorf("Torrent.Links[0] = %q, want magnet link verbatim", tc.Links[0])
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
}
