//go:build !windows

package discord

import (
	"net"
	"os"
	"time"
)

// defaultPipeDirs lists the Unix socket base directories in
// precedence order (official RPC over IPC documentation):
// $DISCORD_IPC_PATH (the C SDK's escape hatch), then the XDG runtime
// dir, then TMPDIR — with /tmp as the hard fallback. Empty values
// drop out.
func defaultPipeDirs() []string {
	var dirs []string
	for _, env := range []string{"DISCORD_IPC_PATH", "XDG_RUNTIME_DIR", "TMPDIR"} {
		if v := os.Getenv(env); v != "" {
			dirs = append(dirs, v)
		}
	}
	return append(dirs, "/tmp")
}

// dialPipe connects one candidate Unix domain socket with a bounded
// timeout (a present-but-dead socket must not hang the worker).
func dialPipe(path string) (readWriter, error) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
