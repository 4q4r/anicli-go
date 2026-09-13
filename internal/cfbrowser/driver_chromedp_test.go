package cfbrowser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
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

// --- PR19: bounded navigation (no load-wait, capped poll budget) ---

// navPollCap bounds one Navigate so the solve poll loop always ticks:
// a caller whose remaining budget exceeds the cap must be clamped.
func TestNavContextCappedAtPollCap(t *testing.T) {
	nav := &chromedpNav{ctx: context.Background()}

	cases := []struct {
		name   string
		caller func() (context.Context, context.CancelFunc)
		want   time.Duration
	}{
		{
			name:   "no caller deadline clamps to poll cap",
			caller: func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			want:   navPollCap,
		},
		{
			name: "caller above cap clamps to cap",
			caller: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 90*time.Second)
			},
			want: navPollCap,
		},
		{
			name: "caller below cap keeps remaining budget",
			caller: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 5*time.Second)
			},
			want: 5 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller, cancelCaller := tc.caller()
			defer cancelCaller()
			nctx, cancelNav := nav.navContext(caller)
			defer cancelNav()
			dl, ok := nctx.Deadline()
			if !ok {
				t.Fatal("navigation context must carry a deadline")
			}
			if rem := time.Until(dl); rem <= 0 || rem > tc.want {
				t.Errorf("navigation budget = %v, want ≤ %s", rem, tc.want)
			}
			if rem := time.Until(dl); rem <= tc.want/2 {
				t.Errorf("navigation budget = %v, suspiciously short (want ~%s)", rem, tc.want)
			}
		})
	}
}

func TestNavigateRunsUnderDeadline(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	seen := make(chan context.Context, 1)
	nav := &chromedpNav{
		// Session stand-in carrying the executor the raw
		// Page.navigate command dispatches through (PR19).
		ctx: cdp.WithExecutor(base, exec),
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

// scriptedRun records every run-seam call (context deadline + action
// count) and replays scripted per-call errors (missing entries = nil).
// It stands in for chromedp.Run while Navigate issues raw commands and
// bounded waits (PR19).
type scriptedRun struct {
	mu    sync.Mutex
	calls []runCallRecord
	errs  []error
}

// runCallRecord is one observed run-seam invocation.
type runCallRecord struct {
	deadline    time.Time
	hasDeadline bool
	actions     int
}

func (r *scriptedRun) Do(ctx context.Context, actions ...chromedp.Action) error {
	rec := runCallRecord{actions: len(actions)}
	if dl, ok := ctx.Deadline(); ok {
		rec.deadline, rec.hasDeadline = dl, true
	}
	r.mu.Lock()
	r.calls = append(r.calls, rec)
	i := len(r.calls) - 1
	r.mu.Unlock()
	var err error
	if i < len(r.errs) {
		err = r.errs[i]
	}
	return err
}

func (r *scriptedRun) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *scriptedRun) call(i int) runCallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[i]
}

// TestNavigateIssuesRawPageNavigate pins the PR19 core: the navigation
// is the RAW Page.navigate CDP command (chromedp's Navigate action
// additionally blocks on the top frame's load event, which a Turnstile
// interstitial never fires — the live-verified 90s hang), followed by
// exactly two run-seam calls: the bounded body-wait, then the state
// harvest.
func TestNavigateIssuesRawPageNavigate(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	sr := &scriptedRun{}
	nav := &chromedpNav{ctx: cdp.WithExecutor(base, exec), run: sr.Do}

	if _, err := nav.Navigate(context.Background(), "https://challenge.example/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	rec := exec.recorded()
	if len(rec) == 0 || rec[0] != "Page.navigate" {
		t.Fatalf("commands = %v, want Page.navigate issued first (raw command, no implicit load-wait)", rec)
	}
	if got := sr.count(); got != 2 {
		t.Fatalf("run calls = %d, want 2 (body-wait + state harvest)", got)
	}
	if bw := sr.call(0); bw.actions != 1 {
		t.Errorf("body-wait call carries %d actions, want exactly 1 (WaitReady)", bw.actions)
	}
	if hv := sr.call(1); hv.actions < 5 {
		t.Errorf("harvest call carries %d actions, want the full pipeline (≥5)", hv.actions)
	}
}

// TestNavigateBodyWaitBoundedAndNonFatal: the body-wait runs under its
// own child context bounded by navBodyWait (strictly inside the
// navigation budget) and its failure is non-fatal — the state harvest
// and cookie harvest proceed regardless.
func TestNavigateBodyWaitBoundedAndNonFatal(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	sr := &scriptedRun{errs: []error{context.DeadlineExceeded}} // body-wait times out
	nav := &chromedpNav{ctx: cdp.WithExecutor(base, exec), run: sr.Do}

	if _, err := nav.Navigate(context.Background(), "https://challenge.example/"); err != nil {
		t.Fatalf("body-wait failure must be non-fatal: %v", err)
	}
	if got := sr.count(); got != 2 {
		t.Fatalf("run calls = %d, want 2 (harvest must run after the failed body-wait)", got)
	}
	bw, hv := sr.call(0), sr.call(1)
	if !bw.hasDeadline || !hv.hasDeadline {
		t.Fatal("both the body-wait and the harvest must run under deadline contexts")
	}
	if rem := time.Until(bw.deadline); rem > navBodyWait {
		t.Errorf("body-wait budget = %v, want ≤ %s", rem, navBodyWait)
	}
	if !bw.deadline.Before(hv.deadline) {
		t.Errorf("body-wait deadline %v must sit strictly inside the navigation budget %v", bw.deadline, hv.deadline)
	}
	rec := exec.recorded()
	if len(rec) != 2 || rec[0] != "Page.navigate" || rec[1] != "Network.getCookies" {
		t.Errorf("commands = %v, want [Page.navigate Network.getCookies] (harvest proceeded after the failed body-wait)", rec)
	}
}

// TestNavigateBudgetExpiryReturnsTypedError: when the per-navigation
// budget dies by deadline mid-harvest, Navigate returns the typed
// ErrNavDeadline (wrapping context.DeadlineExceeded) together with
// whatever partial state completed actions harvested — and skips the
// cookie harvest on the dead context. The budget is driven through the
// session-parent deadline so the expiry is deterministic (no bridge
// race with a caller timer).
func TestNavigateBudgetExpiryReturnsTypedError(t *testing.T) {
	exec := &recordingExecutor{}
	session, cancelSession := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancelSession()
	var calls atomic.Int32
	nav := &chromedpNav{
		ctx: cdp.WithExecutor(session, exec),
		run: func(ctx context.Context, _ ...chromedp.Action) error {
			if calls.Add(1) == 2 {
				<-ctx.Done() // the navigation budget expires mid-harvest
				return ctx.Err()
			}
			return nil
		},
	}
	st, err := nav.Navigate(context.Background(), "https://challenge.example/")
	if !errors.Is(err, ErrNavDeadline) {
		t.Fatalf("want ErrNavDeadline, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ErrNavDeadline must wrap context.DeadlineExceeded, got %v", err)
	}
	if st.Title != "" || st.Body != "" || len(st.Cookies) != 0 {
		t.Errorf("no actions completed before the deadline, state must be empty: %+v", st)
	}
	for _, m := range exec.recorded() {
		if m == "Network.getCookies" {
			t.Fatal("cookie harvest must be skipped after the navigation deadline (context is dead)")
		}
	}
}

// TestNavBudgetExpiredClassification pins the expiry classifier: only
// a deadline death classifies; caller cancellations (the bridge) and
// live contexts do not.
func TestNavBudgetExpiredClassification(t *testing.T) {
	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if !navBudgetExpired(expired) {
		t.Error("deadline-expired context must classify as budget expiry")
	}
	if navBudgetExpired(cancelled) {
		t.Error("cancelled context must not classify as budget expiry (it is a caller cancellation)")
	}
	if navBudgetExpired(context.Background()) {
		t.Error("live context must not classify as budget expiry")
	}
}

// TestErrNavDeadlineWrapsContextDeadlineExceeded: the sentinel must
// satisfy both errors.Is identities the solver relies on.
func TestErrNavDeadlineWrapsContextDeadlineExceeded(t *testing.T) {
	if !errors.Is(ErrNavDeadline, ErrNavDeadline) {
		t.Fatal("sentinel must match itself")
	}
	if !errors.Is(ErrNavDeadline, context.DeadlineExceeded) {
		t.Fatal("ErrNavDeadline must wrap context.DeadlineExceeded")
	}
	if errors.Is(fmt.Errorf("cdp: %w", context.DeadlineExceeded), ErrNavDeadline) {
		t.Fatal("a wrapped raw DeadlineExceeded must NOT masquerade as ErrNavDeadline (non-deadline errors still discard)")
	}
}

// --- PR15: fetch-domain session-context fix ---

// recordingExecutor is a cdp.Executor fake that records command
// methods instead of talking to a browser.
type recordingExecutor struct {
	mu      sync.Mutex
	methods []string
}

func (e *recordingExecutor) Execute(_ context.Context, method string, _ any, _ any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.methods = append(e.methods, method)
	return nil
}

func (e *recordingExecutor) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.methods...)
}

// executorOf safely extracts the cdp executor (ExecutorFromContext
// panics when the key is absent).
func executorOf(ctx context.Context) cdp.Executor {
	defer func() { _ = recover() }()
	return cdp.ExecutorFromContext(ctx)
}

// TestBareChromedpContextLacksCDPExecutor reproduces the bug class:
// chromedp.Run executes actions on cdp.WithExecutor(ctx, target) — a
// DERIVED context — while the bare NewContext output never carries
// the executor, so every direct cdproto Do(ctx) call on it (the old
// resource-diet wiring) failed with cdp.ErrInvalidContext.
func TestBareChromedpContextLacksCDPExecutor(t *testing.T) {
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	if executorOf(ctx) != nil {
		t.Fatal("precondition: bare chromedp context must not carry a CDP executor")
	}
	if err := fetch.Enable().Do(ctx); !errors.Is(err, cdp.ErrInvalidContext) {
		t.Fatalf("fetch.Enable on a bare chromedp context must fail with ErrInvalidContext, got %v", err)
	}
}

// TestWithSessionExecutorDerivesTargetExecutor pins the fix's core:
// a live chromedp session (Context with an attached Target) yields an
// executor-carrying context; a targetless one stays unchanged.
func TestWithSessionExecutorDerivesTargetExecutor(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	c := chromedp.FromContext(base)
	if c == nil {
		t.Fatal("NewContext output must resolve through FromContext")
	}
	target := &chromedp.Target{}
	c.Target = target

	wrapped := withSessionExecutor(base)
	if got := executorOf(wrapped); got == nil || got != cdp.Executor(target) {
		t.Errorf("withSessionExecutor must derive the Target executor, got %v", got)
	}

	targetless, cancelTargetless := chromedp.NewContext(context.Background())
	defer cancelTargetless()
	if got := withSessionExecutor(targetless); got != targetless {
		t.Errorf("targetless context must pass through unchanged")
	}
	if executorOf(withSessionExecutor(targetless)) != nil {
		t.Errorf("targetless context must stay executor-free")
	}
}

// TestEnableResourceDietIssuesFetchEnableOnSessionContext exercises
// the extracted diet wiring: with a session-executor context the
// enable succeeds and issues Fetch.enable; the chromedp context base
// keeps ListenTarget happy (nil Target defers the listener).
func TestEnableResourceDietIssuesFetchEnableOnSessionContext(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	tctx := cdp.WithExecutor(base, exec)

	if err := enableResourceDiet(tctx); err != nil {
		t.Fatalf("resource diet must arm on a session context: %v", err)
	}
	rec := exec.recorded()
	if len(rec) != 1 || rec[0] != "Fetch.enable" {
		t.Errorf("commands = %v, want exactly Fetch.enable", rec)
	}
}

// TestEnableResourceDietFailsWarnOnlyOnBareContext documents the
// warn-only posture: without a session executor the enable fails
// (the factory logs a warning and the solve proceeds full-resource).
func TestEnableResourceDietFailsWarnOnlyOnBareContext(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	if err := enableResourceDiet(base); !errors.Is(err, cdp.ErrInvalidContext) {
		t.Fatalf("bare context enable must fail with ErrInvalidContext (warn-only at the factory), got %v", err)
	}
}

// TestNavigateRunsOnSessionExecutorContext: through a session-executor
// context, chromedp actions AND the cookie harvest both run on the
// executor-carrying context (GetCookies previously failed silently
// with "invalid context").
func TestNavigateRunsOnSessionExecutorContext(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	seen := make(chan context.Context, 1)
	nav := &chromedpNav{
		ctx: cdp.WithExecutor(base, exec),
		run: func(ctx context.Context, _ ...chromedp.Action) error {
			select {
			case seen <- ctx:
			default:
			}
			return nil // successful navigation: harvest proceeds
		},
	}
	if _, err := nav.Navigate(context.Background(), "https://challenge.example/"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	select {
	case c := <-seen:
		if executorOf(c) == nil {
			t.Fatal("Navigate must hand the run seam a session-executor context (cdproto Do calls on it fail otherwise)")
		}
	default:
		t.Fatal("run was not invoked")
	}
	// The cookie harvest runs on the same executor context; since
	// PR19 the raw Page.navigate also flows through it.
	rec := exec.recorded()
	if len(rec) != 2 || rec[0] != "Page.navigate" || rec[1] != "Network.getCookies" {
		t.Errorf("commands after run = %v, want [Page.navigate Network.getCookies] on the executor", rec)
	}
}

// TestClickDispatchesOnSessionExecutorContext: input dispatch runs on
// the executor context (previously failed with "invalid context" —
// the Turnstile click could never land).
func TestClickDispatchesOnSessionExecutorContext(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	nav := &chromedpNav{ctx: cdp.WithExecutor(base, exec)}

	if err := nav.Click(context.Background(), 10, 20); err != nil {
		t.Fatalf("Click must dispatch through the session executor: %v", err)
	}
	rec := exec.recorded()
	if len(rec) != 2 {
		t.Fatalf("commands = %v, want press+release", rec)
	}
	for _, m := range rec {
		if m != "Input.dispatchMouseEvent" {
			t.Errorf("command %q, want Input.dispatchMouseEvent", m)
		}
	}
}

// TestHandlePausedRequestRoutesOnSessionExecutor pins the interceptor
// routing: blocked types fail, everything else continues — both via
// the session executor context.
func TestHandlePausedRequestRoutesOnSessionExecutor(t *testing.T) {
	base, cancel := chromedp.NewContext(context.Background())
	defer cancel()
	exec := &recordingExecutor{}
	ctx := cdp.WithExecutor(base, exec)

	handlePausedRequest(ctx, &fetch.EventRequestPaused{
		RequestID:    fetch.RequestID("1"),
		ResourceType: network.ResourceTypeImage,
	})
	handlePausedRequest(ctx, &fetch.EventRequestPaused{
		RequestID:    fetch.RequestID("2"),
		ResourceType: network.ResourceTypeScript,
	})
	rec := exec.recorded()
	if len(rec) != 2 || rec[0] != "Fetch.failRequest" || rec[1] != "Fetch.continueRequest" {
		t.Errorf("commands = %v, want [Fetch.failRequest Fetch.continueRequest]", rec)
	}
}
