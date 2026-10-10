package tui

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/storage"
)

// lockedBuffer is a concurrency-safe bytes.Buffer for slog handlers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRealDepsWiresPlayerLogSink (PR163): the production player must
// route mpv's stdout/stderr into the file logger. The owner's #163
// report hid mpv's real failure ("Failed to open …") behind an
// unwired log sink — the TUI status showed a bare "exit status 2" and
// anicli.log carried nothing from mpv.
func TestRealDepsWiresPlayerLogSink(t *testing.T) {
	var buf lockedBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	store, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	settings := config.Default()
	settings.Player.Path = stubPlayerBin(t) // prints one line, exits 0

	real, err := NewRealDeps(settings, store, WithLogger(log))
	if err != nil {
		t.Fatalf("NewRealDeps: %v", err)
	}
	defer real.Close()

	// The stub dies inside the default 2s warmup, so the launch
	// retries and exhausts — irrelevant here: the pin is the log
	// wiring, and the lines must have flowed on every attempt.
	_ = real.Deps.Playback.Play(context.Background(), PlayRequest{URL: "u"})

	if !strings.Contains(buf.String(), "stub player running") {
		t.Errorf("the file logger must receive mpv's output, got:\n%s", buf.String())
	}
}
