//go:build windows

package discord

import (
	"os"
	"time"
)

// defaultPipeDirs lists the Windows named-pipe base directory:
// $DISCORD_IPC_PATH overrides, otherwise the well-known
// \\.\pipe\ root every Discord client listens under.
func defaultPipeDirs() []string {
	if v := os.Getenv("DISCORD_IPC_PATH"); v != "" {
		return []string{v}
	}
	return []string{`\\.\pipe\`}
}

// dialPipe opens one candidate named pipe for reading and writing
// (Discord's pipe is byte-mode; plain file IO speaks the protocol).
func dialPipe(path string) (readWriter, error) {
	f, err := openReadWriteFile(path)
	if err != nil {
		return nil, err
	}
	return pipeConn{file: f}, nil
}

// openReadWriteFile is the os.OpenFile indirection (test seam).
func openReadWriteFile(path string) (*os.File, error) {
	//nolint:gosec // the pipe path is machine-local and built by pipeCandidates
	return os.OpenFile(path, os.O_RDWR, 0)
}

// pipeConn adapts the named-pipe *os.File onto the readWriter seam.
// Named pipes do not support deadlines; the worker's read/write
// budget degrades to none on this platform (same trade-off as the
// reference Go implementations).
type pipeConn struct {
	file *os.File
}

// Read forwards to the pipe file.
func (p pipeConn) Read(b []byte) (int, error) { return p.file.Read(b) }

// Write forwards to the pipe file.
func (p pipeConn) Write(b []byte) (int, error) { return p.file.Write(b) }

// Close forwards to the pipe file.
func (p pipeConn) Close() error { return p.file.Close() }

// SetDeadline is a no-op on named pipes.
func (p pipeConn) SetDeadline(time.Time) error { return nil }
