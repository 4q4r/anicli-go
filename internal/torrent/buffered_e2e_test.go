package torrent

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/buffered"
)

// TestBufferedTorrentE2E is the PR43 buffered-over-torrent proof: a
// seeder and a leecher in one process (magnet x.pe), the leecher's
// loopback stream URL buffered through the buffered pipeline to a real
// local file, byte-identical to the seed — and Cleanup (player exit)
// erases the temp dir. This is the path the PR44 pre-play format
// selector's «Буферный» pick rides for torrent sources.
func TestBufferedTorrentE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E in short mode")
	}

	dirA := t.TempDir()
	data, mi, ih := seedTorrent(t, dirA, 384*1024)

	engA := newTestEngine(t, true)
	engA.cfg.Dir = dirA // storage must see the payload to seed it

	engB := newTestEngine(t, true)

	if _, err := engA.AddMetaInfo(mi); err != nil {
		t.Fatalf("seeder AddMetaInfo: %v", err)
	}
	portA, ok := engA.ListenPort()
	if !ok {
		t.Fatal("seeder client has no listen port")
	}
	magnet := "magnet:?xt=urn:btih:" + ih.HexString() +
		"&dn=seed.bin&x.pe=127.0.0.1:" + itoa(portA)
	if _, err := engB.AddLink(context.Background(), magnet); err != nil {
		t.Fatalf("leecher AddLink: %v", err)
	}

	streamURL := engB.StreamURL(ih, 0)
	if streamURL == "" {
		t.Fatal("leecher stream server did not start")
	}

	d := buffered.New(nil, nil, quietLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	handle, err := d.Buffer(ctx, buffered.Source{URL: streamURL}, nil)
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}

	got, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatalf("read buffered file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("buffered bytes differ from the seeded payload (got %d bytes)", len(got))
	}

	// The player-exit cleanup: the temp dir disappears with the file.
	handle.Cleanup()
	if _, err := os.Stat(handle.Path); !os.IsNotExist(err) {
		t.Errorf("buffered file survived cleanup: %v", err)
	}
}
