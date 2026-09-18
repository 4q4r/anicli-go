package providers

import (
	crand "crypto/rand"
	"fmt"
	"net"
	"testing"
)

// newDeadListener returns a listener whose port is already closed: every
// dial is refused instantly. Used to exercise the timeout/transport error
// paths of providers without real network egress and without waiting out
// full request timeouts.
//
// The port is drawn from BELOW the ephemeral range (Linux default
// 32768-60999): a closed ephemeral-port listener can be re-issued by the
// kernel to a parallel test's httptest server, which then answers the
// "dead" address with a live HTTP 200 — observed as TestAnilibriaSearchTimeout
// receiving an HTML body instead of a connection refusal (PR53 review). A
// port outside the ephemeral allocator's band is never re-issued that way.
func newDeadListener(t *testing.T) net.Listener {
	t.Helper()

	var l net.Listener
	var err error
	for attempt := 0; attempt < 5 && l == nil; attempt++ {
		// crypto/rand keeps the linter's non-crypto-random rule quiet;
		// nothing here is security-sensitive — any free port below the
		// ephemeral band does.
		var b [2]byte
		if _, cerr := crand.Read(b[:]); cerr != nil {
			t.Fatalf("random port pick: %v", cerr)
		}
		port := 10000 + (int(b[0])<<8|int(b[1]))%22000
		l, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			l = nil // port taken by something else; try another
		}
	}
	if l == nil {
		// Astronomically unlikely fallback: the ephemeral band, as before.
		l, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return l
}
