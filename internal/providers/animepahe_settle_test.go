package providers

import (
	"context"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
)

// scriptedNav replays states per Navigate call (last state repeats).
type scriptedNav struct {
	states   []cfbrowser.NavState
	calls    int
	clicks   int
	onChange func()
}

func (n *scriptedNav) Navigate(_ context.Context, _ string) (cfbrowser.NavState, error) {
	if n.onChange != nil {
		n.onChange()
	}
	i := n.calls
	if i >= len(n.states) {
		i = len(n.states) - 1
	}
	n.calls++
	return n.states[i], nil
}

func (n *scriptedNav) Click(_ context.Context, _, _ float64) error {
	n.clicks++
	return nil
}
func (n *scriptedNav) Close() error { return nil }

// The verbatim fixture's body carries the site-wide Cloudflare beacon
// (challenge-platform/scripts/precursor/main.js — live-verified
// 2026-09-19, present on EVERY real page). The settle gate must NOT
// read the beacon as a challenge, or a real page polls forever
// (live failure: the proxy-route chain run burned its whole budget
// re-polling a settled page).
func TestNavigateSettledRealPageWithBeaconSettles(t *testing.T) {
	body := string(fixture(t, "animepahe_play.html"))
	nav := &scriptedNav{states: []cfbrowser.NavState{
		{Title: "animepahe :: okay-ish anime website", Body: body},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := navigateSettled(ctx, nav, "https://animepahe.pw/"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if nav.calls != 1 {
		t.Errorf("navigations = %d, want 1 (a settled page must not be re-navigated)", nav.calls)
	}
}

// The real interstitial (curl capture 2026-09-19: window._cf_chl +
// challenge-error-text, title "Just a moment...") keeps the loop
// polling until it turns into the real page.
func TestNavigateSettledPollsThroughInterstitial(t *testing.T) {
	interstitial := `<html><head><title>Just a moment...</title></head>` +
		`<body>window._cf_chl=_cf_chl_opt;challenge-error-text</body></html>`
	real := `<html><head><title>animepahe</title></head>` +
		`<body><script src="/cdn-cgi/challenge-platform/scripts/precursor/main.js"></script></body></html>`
	nav := &scriptedNav{states: []cfbrowser.NavState{
		{Title: "Just a moment...", Body: interstitial},
		{Title: "animepahe", Body: real},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := navigateSettled(ctx, nav, "https://animepahe.pw/"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if nav.calls != 2 {
		t.Errorf("navigations = %d, want 2 (interstitial tick + settled tick)", nav.calls)
	}
}

// An interstitial rendering a Turnstile checkbox gets the solver's
// best-effort click — once — while the loop keeps polling (live
// evidence 2026-09-19: the proxy-route managed challenge sits on an
// unchecked checkbox forever without it).
func TestNavigateSettledClicksTurnstileOnce(t *testing.T) {
	interstitial := `<html><head><title>Just a moment...</title></head>` +
		`<body>window._cf_chl;cf-turnstile</body></html>`
	real := `<html><head><title>animepahe</title></head><body>fine</body></html>`
	nav := &scriptedNav{states: []cfbrowser.NavState{
		{Title: "Just a moment...", Body: interstitial, HasClickTarget: true, ClickX: 30, ClickY: 260},
		{Title: "Just a moment...", Body: interstitial, HasClickTarget: true, ClickX: 30, ClickY: 260},
		{Title: "animepahe", Body: real},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := navigateSettled(ctx, nav, "https://animepahe.pw/"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if nav.clicks != 1 {
		t.Errorf("clicks = %d, want exactly one best-effort click", nav.clicks)
	}
}

// paheInterstitial's unit pins: interstitial marker sets, real-page
// negatives.
func TestPaheInterstitialPredicate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		title string
		body  string
		want  bool
	}{
		{"EN interstitial", "Just a moment...", "window._cf_chl", true},
		{"RU title", "Один момент…", "", true},
		{"RU body heading", "", "Выполнение проверки безопасности", true},
		{"challenge-error-text", "", "<div id=\"challenge-error-text\">", true},
		{"orchestrate script", "", "/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page", true},
		// The real page's beacon must stay negative (the PR71 lesson).
		{"real page beacon", "animepahe", `<script src="/cdn-cgi/challenge-platform/scripts/precursor/main.js">`, false},
		{"empty state", "", "", false},
	}
	for _, tc := range cases {
		if got := paheInterstitial(tc.title, tc.body); got != tc.want {
			t.Errorf("%s: paheInterstitial(%q,…)= %v, want %v", tc.name, tc.title, got, tc.want)
		}
	}
}

// scriptedEval replays Eval results in order (last repeats); a
// changing page is the gate-shell/hydration shape.
type scriptedEval struct {
	results []string
	calls   int
}

func (e *scriptedEval) Eval(_ context.Context, _ string, out any) error {
	i := e.calls
	if i >= len(e.results) {
		i = len(e.results) - 1
	}
	e.calls++
	p := out.(*string)
	*p = e.results[i]
	return nil
}

// The pahe.win gate shell bounces itself via location.replace right
// after the first load: the first markup read is the shell, later
// reads are the real page. stabilizedHTML must keep reading until two
// consecutive reads agree, and return the STABLE one.
func TestStabilizedHTMLWaitsThroughGateBounce(t *testing.T) {
	shell := "<html>gate shell</html>"
	real := `<html><body><a href="https://kwik.cx/f/abc">Redirect me</a></body></html>`
	eval := &scriptedEval{results: []string{shell, real, real}}

	got, err := stabilizedHTML(context.Background(), eval)
	if err != nil {
		t.Fatalf("stabilizedHTML: %v", err)
	}
	if got != real {
		t.Errorf("got %q, want the stable post-bounce markup", got)
	}
	if eval.calls < 3 {
		t.Errorf("reads = %d, want at least 3 (shell, real, real-confirm)", eval.calls)
	}
}

// A page that never stabilizes still returns its last read (the
// bounded-degradation posture), never an error and never "".
func TestStabilizedHTMLNeverMutatingPageReturnsLastRead(t *testing.T) {
	eval := &scriptedEval{results: []string{"<html>mutating</html>"}}
	got, err := stabilizedHTML(context.Background(), eval)
	if err != nil {
		t.Fatalf("stabilizedHTML: %v", err)
	}
	if got != "<html>mutating</html>" {
		t.Errorf("got %q, want the last read", got)
	}
}
