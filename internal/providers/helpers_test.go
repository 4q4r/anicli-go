package providers

import (
	"net"
	"testing"
)

// newDeadListener returns a listener whose port is already closed: every
// dial is refused instantly. Used to exercise the timeout/transport error
// paths of providers without real network egress and without waiting out
// full request timeouts.
func newDeadListener(t *testing.T) net.Listener {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return l
}
