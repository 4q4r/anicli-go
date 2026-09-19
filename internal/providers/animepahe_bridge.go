package providers

// PR71 animepahe browser bridge. The site's Cloudflare front re-challenges
// non-browser fingerprints even with a replayed cf_clearance (PR71
// dossier A/B: curl 200 vs Go 403 on the same cookie set; re-confirmed
// live 2026-09-19 — curl with a freshly solved clearance+UA still draws
// 403 on kwik.cx). The provider therefore moves its site operations
// onto the ephemeral stealth-browser session the [cf] stack already
// owns (the AllAnime bridge precedent): CF solving stays the existing
// ladder, and the GETs run IN-PAGE — page-context fetch with the site
// cookies and the real fingerprint.
//
// Streams ride a two-attempt chain (attempt order per the PR71 task):
//
//  (a) the kwik /e/ embeds from #resolutionMenu through the existing
//      netclient extractor — kept first so networks where kwik is not
//      WAF-blocked keep the direct player flow;
//  (b) the PR49 pahe.win interstitial bypass, because kwik /e/ is hard
//      WAF-blocked network-wide (re-verified live 2026-09-19 with
//      CloakBrowser Pro on BOTH the direct and the proxy route — the
//      block is a WAF deny rule, not a solvable challenge). The chain:
//      play page #pickDownload anchors → pahe.win short link (openly
//      reachable) → kwik.cx/f/<id> file page (challenge; Pro clears it)
//      → plain Laravel _token form → in-page form submit while the
//      driver captures Browser.downloadWillBegin (deny behavior: the
//      file never touches disk) → the post-redirect media URL.
//
// The last hop exists because every in-page JS route to the Location is
// closed (live-verified 2026-09-19): fetch redirect:'manual' is opaque,
// redirect:'follow' dies on the cross-origin CORS check, and a real
// form submit turns the navigation into a download — only the CDP
// download event carries the URL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
)

// pahePollInterval paces the settle and fire-and-poll loops (the
// solver's own poll cadence, scaled down for single-page operations).
const pahePollInterval = 2 * time.Second

// animePaheDownloadLinks parses a play page into quality → pahe.win
// interstitial URL. The live page (verbatim fixture
// testdata/animepahe_play.html, re-verified live 2026-09-19)
// server-renders the download menu:
//
//	<div class="dropdown-menu" id="pickDownload">
//	  <a href="https://pahe.win/wExwm" target="_blank" class="dropdown-item">
//	    OZC &middot; 720p (70MB) <span class="badge badge-primary">BD</span></a>
//
// The quality token ("<NNN>p") lives in the anchor TEXT, not in an
// attribute; anchors without one are skipped. Like animePahePlayLinks,
// the parse is scoped to the menu id so the page's other .dropdown-item
// elements cannot leak in, and later anchors overwrite earlier
// qualities (dict.update convention).
func animePaheDownloadLinks(body []byte) map[string]string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}

	links := map[string]string{}
	doc.Find("#pickDownload a[href]").Each(func(_ int, sel *goquery.Selection) {
		href, ok := sel.Attr("href")
		if !ok || href == "" {
			return
		}
		quality := animePaheQualityToken(sel.Text())
		if quality == "" {
			return
		}
		links[quality] = href // later anchors overwrite, like dict.update
	})
	return links
}

// animePaheQualityToken extracts the "720" out of an anchor label like
// "OZC · 720p (70MB) BD" ("" when the label carries no NNNp token).
func animePaheQualityToken(text string) string {
	const digits = "0123456789"
	for i := 1; i < len(text); i++ {
		if text[i] != 'p' || strings.IndexByte(digits, text[i-1]) < 0 {
			continue
		}
		start := i
		for start > 0 && strings.IndexByte(digits, text[start-1]) >= 0 {
			start--
		}
		return text[start:i]
	}
	return ""
}

// animePaheKwikForm parses a kwik /f/ file page into its download form:
// action (the /d/<id> POST target) and the hidden Laravel _token. The
// live page (2026-09-19, kwik.cx/f/4piNxBJf1Qdx) serves the form in
// PLAIN HTML — no kwik packer — so goquery suffices and the packed-page
// regex path of the /e/ extractor is not involved. ok is false when the
// page carries no /d/-posting form or the token input is missing
// (challenge interstitials and shape drift report here, never as an
// empty action).
func animePaheKwikForm(body []byte) (action, token string, ok bool) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return "", "", false
	}

	sel := doc.Find("form").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return strings.Contains(s.AttrOr("action", ""), "/d/")
	}).First()
	if sel.Length() == 0 {
		return "", "", false
	}
	action = sel.AttrOr("action", "")
	token = sel.Find(`input[name="_token"]`).AttrOr("value", "")
	if action == "" || token == "" {
		return "", "", false
	}
	return action, token, true
}

// animePaheInterstitialTarget extracts the kwik file-page URL from a
// pahe.win interstitial: the openly reachable short page carries the
// real target through its "Redirect me" anchor (live capture
// 2026-09-19: pahe.win/wExwm → kwik.cx/f/4piNxBJf1Qdx). The host match
// is kwik-substring based — the operators rotate kwik mirrors (pahe.win
// is itself such a mirror), and the /f/ path is the invariant that
// matters. "" when the page exposes no kwik target.
func animePaheInterstitialTarget(body []byte) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}

	target := ""
	doc.Find("a[href]").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		href, ok := sel.Attr("href")
		if !ok || !strings.Contains(href, "kwik") {
			return true // keep scanning
		}
		target = href
		return false // found: stop
	})
	return target
}

// paheBrowser is the browser-bridge seam the provider consumes. The
// production implementation (paheCFBrowserBridge below) drives the
// shared ephemeral stealth session; tests drive the provider ladder
// with scripted fakes. Every method is one self-contained operation:
// it re-navigates whatever it needs, so session recycling (the pool's
// idle teardown) between calls is harmless.
type paheBrowser interface {
	// PageFetch performs url's GET inside the animepahe page context
	// (site cookies, real fingerprint) and returns the response body.
	// Non-2xx responses are errors carrying the status.
	PageFetch(ctx context.Context, url string) ([]byte, error)
	// PageHTML navigates the browser to url top-level (through any CF
	// challenge) and returns the settled page's full markup. Cross-origin
	// stops (pahe.win, kwik.cx/f) ride here — in-page fetch would be
	// CORS-opaque for them.
	PageHTML(ctx context.Context, url string) ([]byte, error)
	// SubmitDownload navigates to pageURL, submits the token form
	// posting to action, and returns the download's post-redirect
	// target URL (the direct media link).
	SubmitDownload(ctx context.Context, pageURL, action string) (string, error)
}

// errPaheBridgeDisabled reports a bridge invocation without a solver.
var errPaheBridgeDisabled = errors.New("animepahe bridge: no cf browser configured ([cf].enabled required)")

// paheCFBrowserBridge is the production paheBrowser: a thin cfbrowser
// adapter. Session lifecycle is owned by the solver's pool
// (Solver.WithSession), navigation by the driver, protocol steps by the
// in-page scripts below. All operations are serialized: the pool's
// single page target is shared, and a concurrent navigation would yank
// a fetch mid-flight.
type paheCFBrowserBridge struct {
	// Solver provides the ephemeral browser session (nil = disabled).
	Solver *cfbrowser.Solver

	mu  sync.Mutex
	seq int // fire-and-poll result key sequence (mu-guarded)
}

// buildPaheBridge wires the animepahe bridge when [cf].enabled supplies
// a stealth browser (nil otherwise — the provider stays on the
// netclient + CF-ladder path).
func buildPaheBridge(cf *cfbrowser.Manager) paheBrowser {
	if cf == nil || cf.Solver == nil {
		return nil
	}
	return &paheCFBrowserBridge{Solver: cf.Solver}
}

// WithSession runs fn inside one ephemeral browser slot with the
// bridge mutex held (ops never interleave on the shared page target).
func (b *paheCFBrowserBridge) withSession(ctx context.Context, fn func(ctx context.Context, nav cfbrowser.Naviger, eval cfbrowser.Evaluator) error) error {
	if b == nil || b.Solver == nil {
		return errPaheBridgeDisabled
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Solver.WithSession(ctx, func(sctx context.Context, nav cfbrowser.Naviger) error {
		eval, ok := nav.(cfbrowser.Evaluator)
		if !ok {
			return errors.New("animepahe bridge: driver cannot evaluate JS")
		}
		return fn(sctx, nav, eval)
	})
}

// navigateSettled navigates nav to rawURL and polls until the page is
// NOT a challenge interstitial — the bridge's settle gate is the PAGE
// TRUTH, deliberately not the solver's SolvedState cookie shortcut: a
// warm profile can carry a cf_clearance that is stale for the current
// egress IP (solved via another route), and trusting it would fetch
// from a live challenge page (live-verified 2026-09-19: in-page fetch
// under a stale cookie → 403). ErrNavDeadline ticks keep polling (the
// solver's semantics: a capped navigation is a slow page, not a dead
// session), bounded by ctx.
func navigateSettled(ctx context.Context, nav cfbrowser.Naviger, rawURL string) error {
	clicked := false
	for {
		state, err := nav.Navigate(ctx, rawURL)
		if err != nil && !errors.Is(err, cfbrowser.ErrNavDeadline) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("navigate %s: %w", rawURL, err)
		}
		if errors.Is(err, cfbrowser.ErrNavDeadline) && !navStateSignal(state) {
			// Capped before ANY state landed: blind tick, re-poll.
		} else if !paheInterstitial(state.Title, state.Body) {
			return nil
		}

		// The solver's Turnstile posture: one best-effort click on the
		// checkbox when the interstitial renders one — a managed
		// challenge sits on an unchecked checkbox forever otherwise
		// (live evidence 2026-09-19, proxy route). Non-fatal by
		// design, exactly like solveHost.
		if !clicked && state.HasClickTarget {
			clicked = true
			_ = nav.Click(ctx, state.ClickX, state.ClickY)
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := sleepCtx(ctx, pahePollInterval); err != nil {
			return ctx.Err()
		}
	}
}

// navStateSignal reports whether a NavState carries any
// challenge-relevant signal (the solver's navStateHasSignal, local
// copy: that helper is unexported in cfbrowser).
func navStateSignal(st cfbrowser.NavState) bool {
	return st.Title != "" || st.Body != "" || len(st.Cookies) > 0
}

// sleepCtx waits for d unless ctx finishes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// paheInterstitial reports whether a harvested page state is the
// Cloudflare interstitial — the bridge's settle gate.
//
// Deliberately NOT cfbrowser.IsChallengePage: its body marker list
// includes "challenge-platform", and THIS site embeds a
// challenge-platform beacon script (precursor/main.js) on every REAL
// page (verbatim fixture, live-verified 2026-09-19) — the generic
// predicate reads every settled page as a challenge and polls forever
// (the live proxy-route chain failure). The markers here are the ones
// the interstitial carries and real pages verifiably do not (curl
// capture of the raw 403 interstitial vs the real-page fixture):
// the window._cf_chl config object, the challenge-error-text hook and
// the orchestrate/chl_page loader, plus the localized challenge
// titles (EN + the RU interstitial observed live).
func paheInterstitial(title, body string) bool {
	titles := []string{
		"just a moment", "attention required", "checking your browser",
		"verify you are human", "доступ ограничен", "один момент",
		"выполнение проверки безопасности",
	}
	tl := strings.ToLower(title)
	for _, m := range titles {
		if strings.Contains(tl, m) {
			return true
		}
	}
	bodies := []string{
		"window._cf_chl", "_cf_chl_opt", "challenge-error-text",
		"orchestrate/chl_page", "cf-turnstile", "turnstile.js",
		"cf_chl", "cf-chl", "выполнение проверки безопасности",
	}
	bl := strings.ToLower(body)
	for _, m := range bodies {
		if strings.Contains(bl, m) {
			return true
		}
	}
	return false
}

// paheFetchKick is the FIRE half of the fire-and-poll fetch: it starts
// the async fetch, stashes the outcome under a per-call window key and
// returns SYNCHRONOUSLY (chromedp v0.16 never awaits promises — PR67
// dossier, verified in the vendored evaluate.go: no AwaitPromise — so
// an async IIFE handed to Evaluate would return a useless serialized
// Promise). The key is unique per bridge operation so parallel
// operations on the shared page cannot cross-contaminate. Failed
// replies stash the cf-mitigated header and a body head — a 403 from
// this site is either a challenge retry or a stale clearance, and the
// error text is the only place that difference is visible.
func paheFetchKick(key, url string) string {
	return `(function () {
  window[` + jsString(key) + `] = undefined;
  (async function () {
    try {
      const r = await fetch(` + jsString(url) + `, {credentials: 'include', cache: 'no-store'});
      const body = await r.text();
      window[` + jsString(key) + `] = JSON.stringify({
        status: r.status,
        mitigated: r.headers.get('cf-mitigated') || '',
        head: body.slice(0, 160),
        body: body
      });
    } catch (e) {
      window[` + jsString(key) + `] = JSON.stringify({status: 0, error: String(e)});
    }
  })();
  return true;
})()`
}

// pahePollScript is the POLL half: a synchronous read of the result
// key (null until the fetch landed).
func pahePollScript(key string) string {
	return `(function () {
  const v = window[` + jsString(key) + `];
  return v === undefined ? null : v;
})()`
}

// paheHTMLScript reads the settled page's full markup synchronously.
const paheHTMLScript = `document.documentElement.outerHTML`

// paheFormSubmitScript submits the page's token form posting to action
// (the real DOM form: its hidden _token rides along).
func paheFormSubmitScript(action string) string {
	return `(function () {
  const target = ` + jsString(action) + `;
  const form = [...document.querySelectorAll('form')].find(f =>
    f.getAttribute('action') === target || f.action === target);
  if (!form) return 'no-form';
  form.submit();
  return 'submitted';
})()`
}

// jsString renders s as a JS single-quoted string literal (the bridge
// only interpolates Go-built keys and URLs — never page content).
func jsString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`)
	return "'" + r.Replace(s) + "'"
}

// PageFetch implements paheBrowser: navigate the site origin (clearing
// any challenge), then run the in-page fetch fire-and-poll.
func (b *paheCFBrowserBridge) PageFetch(ctx context.Context, url string) ([]byte, error) {
	origin := originOf(url)
	if origin == "" {
		return nil, fmt.Errorf("animepahe bridge: %q has no origin", url)
	}
	var body []byte
	err := b.withSession(ctx, func(sctx context.Context, nav cfbrowser.Naviger, eval cfbrowser.Evaluator) error {
		if err := navigateSettled(sctx, nav, origin+"/"); err != nil {
			return fmt.Errorf("settle origin: %w", err)
		}
		b.seq++
		key := fmt.Sprintf("__apaheFetch%d", b.seq)

		var fired bool
		if err := eval.Eval(sctx, paheFetchKick(key, url), &fired); err != nil {
			return fmt.Errorf("fetch kick: %w", err)
		}
		raw, err := pollWindowKey(sctx, eval, key)
		if err != nil {
			return err
		}
		var reply struct {
			Status    int    `json:"status"`
			Mitigated string `json:"mitigated"`
			Head      string `json:"head"`
			Error     string `json:"error"`
			Body      string `json:"body"`
		}
		if err := json.Unmarshal([]byte(raw), &reply); err != nil {
			return fmt.Errorf("fetch reply: %w", err)
		}
		if reply.Error != "" {
			return fmt.Errorf("in-page fetch %s: %s", url, reply.Error)
		}
		if reply.Status < 200 || reply.Status >= 300 {
			return fmt.Errorf("in-page fetch %s: status %d (cf-mitigated=%q body-head=%q)",
				url, reply.Status, reply.Mitigated, reply.Head)
		}
		body = []byte(reply.Body)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// PageHTML implements paheBrowser: top-level navigation (the CORS-free
// route for cross-origin stops) plus a stabilized markup read.
func (b *paheCFBrowserBridge) PageHTML(ctx context.Context, url string) ([]byte, error) {
	var body []byte
	err := b.withSession(ctx, func(sctx context.Context, nav cfbrowser.Naviger, eval cfbrowser.Evaluator) error {
		if err := navigateSettled(sctx, nav, url); err != nil {
			return fmt.Errorf("settle page: %w", err)
		}
		html, err := stabilizedHTML(ctx, eval)
		if err != nil {
			return err
		}
		body = []byte(html)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// stabilizedHTML reads the settled page's markup until it STABILIZES:
// two consecutive identical non-empty reads. Gate-shell pages bounce
// themselves via location.replace right after the first load (the
// pahe.win interstitial requires its own URL as the referrer — live
// evidence 2026-09-19) and hydrating pages mutate; a one-shot read
// races all of them. Bounded by reads; ctx bounds the whole wait. A
// page that never stabilizes degrades to its last read.
func stabilizedHTML(ctx context.Context, eval cfbrowser.Evaluator) (string, error) {
	const reads = 6
	prev := ""
	for i := range reads {
		var html string
		if err := eval.Eval(ctx, paheHTMLScript, &html); err != nil {
			return "", fmt.Errorf("read page html: %w", err)
		}
		if html != "" && html == prev {
			return html, nil
		}
		prev = html
		if i < reads-1 {
			if err := sleepCtx(ctx, pahePollInterval); err != nil {
				return "", ctx.Err()
			}
		}
	}
	if prev == "" {
		return "", errors.New("page markup is empty")
	}
	return prev, nil
}

// SubmitDownload implements paheBrowser: navigate the kwik /f/ page
// (its challenge clears in the settle loop), then submit the token
// form under the driver's download capture. The capture is what sees
// the post-redirect media URL — no in-page JS route can (CORS/opaque
// redirect; see the file comment).
func (b *paheCFBrowserBridge) SubmitDownload(ctx context.Context, pageURL, action string) (string, error) {
	var media string
	err := b.withSession(ctx, func(sctx context.Context, nav cfbrowser.Naviger, eval cfbrowser.Evaluator) error {
		if err := navigateSettled(sctx, nav, pageURL); err != nil {
			return fmt.Errorf("settle kwik page: %w", err)
		}
		observer, ok := nav.(cfbrowser.DownloadObserver)
		if !ok {
			return errors.New("animepahe bridge: driver cannot capture downloads")
		}
		url, err := observer.CaptureDownload(sctx, func(ictx context.Context) error {
			var state string
			if err := eval.Eval(ictx, paheFormSubmitScript(action), &state); err != nil {
				return fmt.Errorf("form submit: %w", err)
			}
			if state != "submitted" {
				return fmt.Errorf("form submit: %s", state)
			}
			return nil
		})
		if err != nil {
			return err
		}
		media = url
		return nil
	})
	if err != nil {
		return "", err
	}
	return media, nil
}

// pollWindowKey polls the fire-and-poll result key until it lands or
// ctx ends (the PR67-dossier proven pattern). The poll expression
// returns null until the fetch stashes its result, so the raw value is
// read through a []byte (chromedp hands null back as literal bytes —
// a *string target would surface ErrJSNull instead).
func pollWindowKey(ctx context.Context, eval cfbrowser.Evaluator, key string) (string, error) {
	for {
		var raw []byte
		if err := eval.Eval(ctx, pahePollScript(key), &raw); err != nil {
			return "", fmt.Errorf("fetch poll: %w", err)
		}
		if string(raw) != "null" {
			var inner string
			if err := json.Unmarshal(raw, &inner); err != nil {
				return "", fmt.Errorf("fetch poll decode: %w", err)
			}
			return inner, nil
		}
		if err := sleepCtx(ctx, pahePollInterval); err != nil {
			return "", fmt.Errorf("fetch poll: %w", ctx.Err())
		}
	}
}

// originOf renders scheme://host[:port]/ for the page-context
// navigation target ("" when url has no scheme).
func originOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i <= 0 {
		return ""
	}
	rest := rawURL[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		return ""
	}
	return rawURL[:i] + "://" + rest
}
