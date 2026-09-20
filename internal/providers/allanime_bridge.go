package providers

// AllAnime browser-bridge fallback (robustness tier). When the pure-Go
// crypto derivation fails at ANY step — mask formula drift, unknown
// buildId (AA_CRYPTO_BUILD_MISMATCH), rotated constants — an ephemeral
// cfbrowser session navigates the mkissa.to root (which passes
// Cloudflare silently; verified live 2026-09-13), sandbox-evals the
// CURRENT player chunk with the dossier harness (Proxy-stubbed imports,
// eval'd body, live closures) and hands {buildId, epoch, partB, mask}
// back to the Go path. The browser closes with the session pool; the
// Go path continues offline until the material rotates again.
//
// Gating: the bridge needs the stealth browser (always supplied by
// NewManager since PR80); a nil-solver call leaves the pure-Go typed
// errors in place.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
)

// aaBridgeSource is the extraction seam. The production implementation
// is aaCFBrowserBridge (thin cfbrowser adapter below); tests drive the
// provider ladder with fakes.
type aaBridgeSource interface {
	// ExtractCrypto derives live crypto material from the current
	// mkissa.to player chunk.
	ExtractCrypto(ctx context.Context) (aaBridgeMaterial, error)
}

// aaCFBrowserBridge extracts AllAnime crypto material through an
// ephemeral cfbrowser session. It is deliberately THIN: session
// lifecycle is owned by the solver's pool (Solver.WithSession), page
// navigation by the driver, and every protocol step lives in the
// aaBridgeExtractionJS script evaluated in the page. Verified by
// compilation + the same harness that characterized the protocol live
// (2026-09-13); driving a real stealth Chromium is the parity/ladder
// pass's job, exactly like the solver's chromedp driver.
type aaCFBrowserBridge struct {
	// Solver provides the ephemeral browser session (nil = disabled).
	Solver *cfbrowser.Solver
	// RootURL is the page the bridge navigates (mkissa.to root).
	RootURL string
	// Lane is the bootstrap lane the extraction bootstraps ("k7").
	Lane string
}

// errAABridgeDisabled reports a bridge invocation without a solver
// (PR80: CF is always on — reachable only through anomalous wiring;
// the stealth browser self-installs at startup).
var errAABridgeDisabled = errors.New("allanime bridge: stealth-браузер недоступен — он скачивается автоматически при запуске; при повторении выполните `anicli cf install`")

// ExtractCrypto navigates the root page and runs the extraction script.
func (b *aaCFBrowserBridge) ExtractCrypto(ctx context.Context) (aaBridgeMaterial, error) {
	if b == nil || b.Solver == nil {
		return aaBridgeMaterial{}, errAABridgeDisabled
	}
	var out aaBridgeMaterial
	err := b.Solver.WithSession(ctx, func(sctx context.Context, nav cfbrowser.Naviger) error {
		eval, ok := nav.(cfbrowser.Evaluator)
		if !ok {
			return errors.New("allanime bridge: driver cannot evaluate JS")
		}
		if _, err := nav.Navigate(sctx, b.RootURL); err != nil {
			return fmt.Errorf("navigate root: %w", err)
		}
		var reply string
		script := fmt.Sprintf("(async () => { const LANE = %q; %s })()", b.Lane, aaBridgeExtractionJS)
		if err := eval.Eval(sctx, script, &reply); err != nil {
			return fmt.Errorf("eval extraction: %w", err)
		}
		material, err := parseAABridgeReply(reply)
		if err != nil {
			return err
		}
		out = material
		return nil
	})
	if err != nil {
		return aaBridgeMaterial{}, err
	}
	return out, nil
}

// parseAABridgeReply decodes the JSON the extraction script returns:
// {"ok":true,"buildId":..,"epoch":..,"partB":..,"mask":"b64.."} or
// {"ok":false,"error":".."}.
func parseAABridgeReply(reply string) (aaBridgeMaterial, error) {
	var parsed struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		BuildID string `json:"buildId"`
		Epoch   int64  `json:"epoch"`
		PartB   string `json:"partB"`
		MaskB64 string `json:"mask"`
	}
	if err := json.Unmarshal([]byte(reply), &parsed); err != nil {
		return aaBridgeMaterial{}, fmt.Errorf("allanime bridge: bad reply: %w", err)
	}
	if !parsed.OK {
		return aaBridgeMaterial{}, fmt.Errorf("allanime bridge: %s", parsed.Error)
	}
	mat := aaBridgeMaterial{BuildID: parsed.BuildID, Epoch: parsed.Epoch, PartB: parsed.PartB}
	if parsed.MaskB64 != "" {
		mask, err := base64.StdEncoding.DecodeString(parsed.MaskB64)
		if err != nil {
			return aaBridgeMaterial{}, fmt.Errorf("allanime bridge: mask b64: %w", err)
		}
		mat.Mask = mask
	}
	return mat, nil
}

// aaBridgeExtractionJS is the in-page extraction harness (the dossier
// sandbox), ending with `return JSON.stringify(result)`. Steps:
//
//  1. fetch the mkissa.to root HTML, find the /entry/app.*.js URL;
//  2. fetch the entry bundle, collect its chunk imports (quote-
//     delimited specifiers resolved against the entry URL — the live
//     imports are "../chunks/x.js", not "./x.js");
//  3. find the crypto chunk (marker: the ST error literal
//     "invalid_part_b" — a stable string in every observed build; the
//     bootPrefix literal is char-coded in some builds, verified live
//     2026-09-13 when the source representation rotated under our
//     feet between two sessions);
//  4. strip/stub the ES-module syntax (Proxy stubs), eval the body,
//     keep the live closures {cy, iT, gT, mT, gy, sd};
//  5. read the buildId (sd), derive the mask (cy), and bootstrap with
//     the bT epoch candidates until the server accepts x-aa-boot;
//  6. return {buildId, epoch, partB, mask} as JSON.
//
// Rotation-proof by construction: every constant comes from the LIVE
// chunk, not from this repo's ported tables.
const aaBridgeExtractionJS = `
const __out = {ok: false, error: 'incomplete'};
try {
  // 1. entry bundle URL from the root HTML
  const html = await (await fetch('/', {credentials: 'include'})).text();
  const appMatch = html.match(/https?:\/\/[^"'\s]+\/entry\/app\.[A-Za-z0-9_.-]+\.js/);
  if (!appMatch) throw new Error('entry bundle not found on root page');
  const appURL = appMatch[0];

  // 2. chunk list from the entry bundle (quote-delimited specifiers,
  //    resolved against the entry URL)
  const entry = await (await fetch(appURL)).text();
  const chunkURLs = [...new Set([...entry.matchAll(/"([^"']*\.js)"/g)].map(m => m[1]))]
    .map(s => new URL(s, appURL).href)
    .filter(u => u.includes('/chunks/'));

  // 3. the crypto chunk carries the stable error literals
  let cryptoChunk = null;
  for (const u of chunkURLs.slice(0, 60)) {
    const t = await (await fetch(u)).text();
    if (t.includes('invalid_part_b')) { cryptoChunk = t; break; }
  }
  if (!cryptoChunk) throw new Error('crypto chunk not found in entry imports');

  // 4. sandbox-eval the chunk (dossier harness)
  let body = cryptoChunk;
  body = body.replace(/import\s*"[^"]*"\s*;?/g, '');
  body = body.replace(/import\s*\{([^}]*)\}\s*from\s*"[^"]*"\s*;?/g, (full, names) => {
    return names.split(',').map(s => s.trim()).filter(Boolean).map(pair => {
      const parts = pair.split(/\s+as\s+/).map(x => x.trim());
      return 'const ' + (parts[1] || parts[0]) + ' = __STUB.' + parts[0] + ';';
    }).join('');
  });
  body = body.replace(/import\s+([A-Za-z_$][\w$]*)\s+from\s*"[^"]*"\s*;?/g, 'const $1 = __STUB.default;');
  body = body.replace(/export\s*\{[\s\S]*\}\s*;?\s*$/, '');
  body = body.replace(/import\.meta/g, '__IMPORT_META');
  const mkStubCode = [
    'function mkStub(path) {',
    '  const fn = function(){ return STUB; };',
    '  const STUB = new Proxy(fn, {',
    '    get(t, p) {',
    '      if (p === Symbol.iterator) return function*(){};',
    '      if (p === Symbol.asyncIterator) return async function*(){};',
    '      if (p === Symbol.toPrimitive) return () => 0;',
    "      if (p === 'toString' || p === 'name') return 'stub:' + path;",
    "      if (p === 'then') return undefined;",
    "      if (p === 'prototype') return fn.prototype;",
    '      return mkStub(path + "." + String(p));',
    '    },',
    "    apply() { return mkStub(path + '()'); },",
    "    construct() { return mkStub('new:' + path); },",
    '    has() { return true; },',
    '    set() { return true; },',
    '  });',
    '  return STUB;',
    '}',
    "const __STUB = mkStub('mod');",
  ].join('\n');
  const factory = new Function([
    "const __IMPORT_META = {url: " + JSON.stringify(appURL) + ", env: {}, glob: {}};",
    mkStubCode,
    body,
    'return {cy, iT, gT, mT, gy, sd};',
  ].join('\n'));
  const bag = factory();

  // 5. buildId + mask + bootstrap over the bT epoch candidates
  const buildId = typeof bag.sd === 'string' && bag.sd !== '' ? bag.sd : null;
  if (!buildId) throw new Error('buildId (sd) not exposed by chunk');
  const mask = bag.cy(buildId);
  if (!mask) throw new Error('cy(buildId) returned null');
  const host = location.hostname;
  const group = bag.gT(host);
  const candidates = [...new Set([bag.mT(), bag.gy()])];
  // PINNED FALLBACK ORIGIN [M5]: when the entry bundle is served from
  // a host other than the page origin, the API lives at
  // https://api.mkissa.net (the post-2026-07-22 rotation value; the
  // AllAnimeAPIBase Go constant mirrors it). If the site rotates the
  // API host again, the bootstrap fetch below fails LOUDLY (non-ok
  // status or empty partB -> lastErr -> thrown) and the error surfaces
  // through the bridge reply to the Go path — no silent wrong-host
  // guessing here. Update this literal together with
  // AllAnimeAPIBase on rotation.
  const apiOrigin = new URL(appURL).origin === location.origin ? location.origin : 'https://api.mkissa.net';
  const bootstrap = new URL('/client-crypto/v1/bootstrap', apiOrigin);
  let lastErr = 'no epoch candidate accepted';
  for (const epoch of candidates) {
    const boot = await bag.iT({buildId, epoch, keyGroup: group, refererHost: host, contentLane: LANE});
    const resp = await fetch(bootstrap + '?buildId=' + encodeURIComponent(buildId) + '&k=' + encodeURIComponent(LANE), {
      method: 'GET', credentials: 'include', cache: 'no-store',
      headers: {'x-build-id': buildId, 'x-aa-boot': boot},
    });
    if (!resp.ok) { lastErr = 'bootstrap http ' + resp.status + ' (epoch ' + epoch + ')'; continue; }
    const js = await resp.json();
    if (!js || !js.partB) { lastErr = 'bootstrap empty'; continue; }
    if (js.k && js.k !== LANE) { lastErr = 'bootstrap lane mismatch'; continue; }
    __out.ok = true;
    __out.error = '';
    __out.buildId = buildId;
    __out.epoch = js.epoch;
    __out.partB = js.partB;
    __out.mask = btoa(String.fromCharCode(...Array.from(mask)));
    break;
  }
  if (!__out.ok) throw new Error(lastErr);
} catch (e) {
  __out.ok = false;
  __out.error = e instanceof Error ? e.message : String(e);
}
return JSON.stringify(__out);`
