package cfbrowser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// The chromedp adapter itself runs no browser in tests (see the file
// header of driver_chromedp.go); what IS unit-testable is the
// per-navigation bounding: the derived context must carry the caller's
// solve budget and abort on caller cancellation, so a hung page load
// can never pin the singleflighted solve past its 90s timeout.

func TestNavContextInheritsCallerDeadlineAndCancellation(t *testing.T) {
	nav := &chromedpNav{ctx: context.Background()} // session stand-in

	caller, cancelCaller := context.WithTimeout(context.Background(), 2*time.Second)
	nctx, cancelNav := nav.navContext(caller)

	dl, ok := nctx.Deadline()
	if !ok {
		cancelNav()
		cancelCaller()
		t.Fatal("derived navigation context must carry a deadline")
	}
	if rem := time.Until(dl); rem <= 0 || rem > 2*time.Second {
		cancelNav()
		cancelCaller()
		t.Errorf("navigation budget = %v, want the caller's remaining ~2s", rem)
	}

	// Cancelling the caller must abort the navigation context even
	// before its own deadline fires.
	cancelCaller()
	select {
	case <-nctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("caller cancellation must abort the navigation context")
	}
	cancelNav()
}

func TestNavContextDefaultsToSolveBudget(t *testing.T) {
	nav := &chromedpNav{ctx: context.Background()}
	nctx, cancel := nav.navContext(context.Background())
	defer cancel()
	dl, ok := nctx.Deadline()
	if !ok {
		t.Fatal("navigation context must carry the default solve budget")
	}
	if rem := time.Until(dl); rem <= 0 || rem > DefaultSolveTimeout {
		t.Errorf("navigation budget = %v, want the default %s", rem, DefaultSolveTimeout)
	}
}

func TestNavigateRunsUnderDeadline(t *testing.T) {
	seen := make(chan context.Context, 1)
	nav := &chromedpNav{
		ctx: context.Background(), // session stand-in
		run: func(ctx context.Context, _ ...chromedp.Action) error {
			select {
			case seen <- ctx:
			default:
			}
			return errors.New("stop before harvesting state")
		},
	}
	caller, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := nav.Navigate(caller, "https://challenge.example/"); err == nil {
		t.Fatal("expected the injected run error to surface")
	}
	select {
	case c := <-seen:
		dl, ok := c.Deadline()
		if !ok {
			t.Fatal("chromedp actions must run under a deadline (hung page loads must not outlive the solve budget)")
		}
		if rem := time.Until(dl); rem <= 0 || rem > 5*time.Second {
			t.Errorf("navigation budget = %v, want the caller's remaining ~5s", rem)
		}
	default:
		t.Fatal("run was not invoked")
	}
}

// TestNavContextCancelIdempotent guards the wrapped cancel (bridge
// goroutine + defer path) against double-close panics.
func TestNavContextCancelIdempotent(t *testing.T) {
	nav := &chromedpNav{ctx: context.Background()}
	caller, cancelCaller := context.WithCancel(context.Background())
	nctx, cancel := nav.navContext(caller)
	cancelCaller()
	<-nctx.Done()
	cancel() // must not panic when the bridge already cancelled.
	cancel()
}

// TestNavContextSessionDerivation asserts the derived context still
// carries values from the session context (chromedp actions resolve
// the session through context values on derived contexts).
func TestNavContextSessionDerivation(t *testing.T) {
	type sessionKey struct{}
	session := context.WithValue(context.Background(), sessionKey{}, "session")
	nav := &chromedpNav{ctx: session}
	nctx, cancel := nav.navContext(context.Background())
	defer cancel()
	if v, ok := nctx.Value(sessionKey{}).(string); !ok || v != "session" {
		t.Errorf("derived context must inherit session values, got %q", v)
	}
}
