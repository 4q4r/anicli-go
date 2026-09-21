//go:build load

package torrent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/buffered"
)

// PR81 client-to-client throughput profile: the buffered-torrent E2E
// pattern (seeder + leecher in one process over magnet x.pe) scaled to
// an 8 MiB payload. The leecher's loopback stream URL is buffered to a
// local file through the production buffered pipeline; MB/s reported
// over the buffer window. Wall-clock honest (`make load` skips -race).

const torrentThroughputBytes = 8 << 20 // 8 MiB payload

// TestLoadTorrentClientToClientThroughput seeds 8 MiB into engine A,
// drains it through engine B's stream server via the buffered
// pipeline, asserts byte fidelity and reports MB/s.
func TestLoadTorrentClientToClientThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput load in short mode")
	}

	dirA := t.TempDir()
	data, mi, ih := seedTorrent(t, dirA, torrentThroughputBytes)

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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	handle, err := d.Buffer(ctx, buffered.Source{URL: streamURL}, nil)
	wall := time.Since(start)
	if err != nil {
		t.Fatalf("Buffer: %v", err)
	}
	t.Cleanup(handle.Cleanup)

	got, err := os.ReadFile(handle.Path)
	if err != nil {
		t.Fatalf("read buffered file: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("buffered payload is not byte-identical to the seed")
	}

	mbps := float64(torrentThroughputBytes) / (1 << 20) / wall.Seconds()
	fmt.Fprintf(os.Stdout, "\n=== torrent client-to-client throughput (buffered pipeline) ===\n")
	fmt.Fprintf(os.Stdout, "payload\t%d MiB\twall %s\tthroughput\t%.1f MB/s\n\n",
		torrentThroughputBytes/(1<<20), wall.Round(time.Millisecond), mbps)
	if mbps < 0.5 {
		t.Errorf("throughput %.2f MB/s suspiciously low (< 0.5 MB/s)", mbps)
	}
}
