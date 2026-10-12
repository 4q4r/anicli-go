//go:build windows

package player

// newIPCSocketDir degrades on Windows (#166): mpv's IPC transport
// there is a named pipe (\\.\pipe\...), which the Go stdlib cannot
// dial without a third-party dependency — and this port is stdlib
// only. The launch stays plain: no --input-ipc-server flag, no
// position capture, so a redub relaunches without --start. Nothing
// unix-only leaks into the build.
func newIPCSocketDir() (socket, dir string, err error) {
	return "", "", nil
}
