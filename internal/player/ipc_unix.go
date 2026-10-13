//go:build !windows

package player

import (
	"os"
	"path/filepath"
)

// maxUnixSocketPath is the practical budget for a unix socket address:
// sun_path caps at 108 bytes; anything near that is one long username
// or TMPDIR away from an unbindable socket.
const maxUnixSocketPath = 100

// newIPCSocketDir creates the per-launch IPC socket location: a fresh
// 0700 temp directory holding the socket file. Uniqueness per launch
// is the lifecycle contract — mpv binds the path itself, no other
// instance can collide with it, and the caller removes the whole dir
// when Play returns. The 0700 dir keeps the socket unreachable for
// other local users (a world-connectable media-control socket would
// let anyone pause or seek the user's player).
//
// 169-L: a long TMPDIR pushed the resolved path past the kernel limit
// — mpv could never bind it, and every launch froze 3 seconds on the
// IPC dial. Beyond maxUnixSocketPath the dir falls back to the
// canonical /tmp (os.TempDir() honors TMPDIR and inherits its length;
// the kernel limit forces the short well-known dir). MkdirTemp keeps
// the name unique and the dir 0700, so the fallback preserves both the
// no-collision and the not-world-connectable contracts.
//
// Known degradation (review m5): systems with a long-TMPDIR default
// AND no writable /tmp (Termux) lose the IPC position tracker
// silently — MkdirTemp("/tmp", …) fails and the caller degrades to no
// position capture; nothing else breaks.
func newIPCSocketDir() (socket, dir string, err error) {
	dir, err = os.MkdirTemp("", "anicli-mpv-ipc-")
	if err != nil {
		return "", "", err
	}
	socket = filepath.Join(dir, "mpv.sock")
	if len(socket) <= maxUnixSocketPath {
		return socket, dir, nil
	}
	_ = os.Remove(dir)
	dir, err = os.MkdirTemp("/tmp", "anicli-mpv-ipc-")
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "mpv.sock"), dir, nil
}
