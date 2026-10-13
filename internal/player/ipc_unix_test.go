//go:build !windows

package player

// 169-L: the per-launch IPC socket path rides unix sun_path, capped at
// 108 bytes — a long TMPDIR produced a path mpv can never bind (and
// every launch froze 3 seconds on the IPC dial). Beyond the practical
// budget the socket falls back to the canonical /tmp.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIPCSocketPathSunLimit: a long TMPDIR must still yield a bindable
// socket path (≤ 100 bytes), the 0700 unique dir must survive the
// fallback, and a short TMPDIR keeps today's behavior unchanged.
func TestIPCSocketPathSunLimit(t *testing.T) {
	t.Run("long TMPDIR falls back short", func(t *testing.T) {
		long := filepath.Join(t.TempDir(), strings.Repeat("deep/", 30))
		if err := os.MkdirAll(long, 0o750); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", long)

		socket, dir, err := newIPCSocketDir()
		if err != nil {
			t.Fatalf("newIPCSocketDir: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		if len(socket) > 100 {
			t.Fatalf("socket path is %d bytes (%q), over the practical sun_path budget", len(socket), socket)
		}
		if !strings.HasPrefix(socket, "/tmp/") {
			t.Fatalf("fallback socket %q must live under the canonical /tmp", socket)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("fallback dir missing: %v", err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("fallback dir mode = %v, want 0700 (the socket must stay unreachable for other users)", info.Mode().Perm())
		}
		if _, err := os.Stat(socket); !os.IsNotExist(err) {
			t.Fatalf("the socket file must not pre-exist, stat err = %v", err)
		}
	})

	t.Run("short TMPDIR unchanged", func(t *testing.T) {
		// t.TempDir() paths carry the test name and are long by
		// construction — build a genuinely short TMPDIR.
		short, err := os.MkdirTemp("/tmp", "as-")
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("TMPDIR", short)

		socket, dir, err := newIPCSocketDir()
		if err != nil {
			t.Fatalf("newIPCSocketDir: %v", err)
		}
		t.Cleanup(func() {
			_ = os.RemoveAll(dir)
			_ = os.RemoveAll(short)
		})

		if !strings.HasPrefix(socket, short) {
			t.Fatalf("socket %q must stay under the configured TMPDIR %q", socket, short)
		}
		if want := filepath.Join(dir, "mpv.sock"); socket != want {
			t.Fatalf("socket = %q, want %q", socket, want)
		}
	})
}
