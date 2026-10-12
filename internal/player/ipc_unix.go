//go:build !windows

package player

import (
	"os"
	"path/filepath"
)

// newIPCSocketDir creates the per-launch IPC socket location: a fresh
// 0700 temp directory holding the socket file. Uniqueness per launch
// is the lifecycle contract — mpv binds the path itself, no other
// instance can collide with it, and the caller removes the whole dir
// when Play returns. The 0700 dir keeps the socket unreachable for
// other local users (a world-connectable media-control socket would
// let anyone pause or seek the user's player).
func newIPCSocketDir() (socket, dir string, err error) {
	dir, err = os.MkdirTemp("", "anicli-mpv-ipc-")
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "mpv.sock"), dir, nil
}
