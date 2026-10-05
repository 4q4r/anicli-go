package lua

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/extractors"
	"github.com/an0nx/anicli-go/internal/netclient"
	lua "github.com/yuin/gopher-lua"
)

// The PR116 SDK extensions: http.get opts (custom headers),
// http.get_batch (bounded-parallel fan-out with per-URL soft failure)
// and anicli.extract (the shared extractor factory surfaced to
// scripts — the compiled providers resolve embeds through the same
// extractors.Resolve loop).

// httpResult is one completed (or failed) SDK HTTP fetch in Go
// values — the shared core of http.get and http.get_batch.
type httpResult struct {
	status  int
	headers map[string]string
	body    []byte
	err     error
}

// httpDo runs one request against the wired netclient (or the plain
// fallback client), capping the body at the engine budget.
func (e *Engine) httpDo(ctx context.Context, method, rawURL, body, contentType string, headers map[string]string) httpResult {
	merged := make(map[string]string, len(headers)+1)
	for k, v := range headers {
		merged[k] = v
	}
	if contentType != "" {
		merged["Content-Type"] = contentType
	}

	if e.cfg.HTTP != nil {
		req := netclient.Request{
			Method:  method,
			URL:     rawURL,
			Headers: merged,
			Body:    strings.NewReader(body),
			Op:      "lua",
		}
		nr, err := e.cfg.HTTP.Do(ctx, req)
		if err != nil {
			return httpResult{err: err}
		}
		return httpResult{status: nr.StatusCode, headers: flattenHeaders(nr.Header), body: nr.Body}
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(body))
	if err != nil {
		return httpResult{err: err}
	}
	for k, v := range merged {
		req.Header.Set(k, v)
	}
	rs, err := e.stdClient().Do(req)
	if err != nil {
		return httpResult{err: err}
	}
	defer func() { _ = rs.Body.Close() }()
	resp, err := io.ReadAll(io.LimitReader(rs.Body, e.cfg.BodyLimit+1))
	if err != nil {
		return httpResult{err: err}
	}
	return httpResult{status: rs.StatusCode, headers: flattenHeaders(rs.Header), body: resp}
}

// fallbackClient lazily builds the netclient used when no transport is
// wired (tests and sandboxes): the request budget is the engine's
// invocation timeout — a zero-value-timeout client would expire every
// request instantly (netclient wraps each request in a RequestTimeout
// context).
func (e *Engine) fallbackClient() *netclient.Client {
	e.fbOnce.Do(func() {
		timeout := e.cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		connect := timeout
		if connect > 10*time.Second {
			connect = 10 * time.Second
		}
		e.fbClient, _ = netclient.New(config.Network{
			RequestTimeout: timeout,
			ConnectTimeout: connect,
		})
	})
	return e.fbClient
}

// resolveClient picks the transport for anicli.extract: the wired
// netclient, else the lazily built fallback.
func (e *Engine) resolveClient() *netclient.Client {
	if e.cfg.HTTP != nil {
		return e.cfg.HTTP
	}
	return e.fallbackClient()
}

// sdkHTTPGetWithOpts implements http.get(url[, opts]): opts.headers
// ride on the request (Referer and friends — provider scripts
// re-creating header-gated flows need them).
func (e *Engine) sdkHTTPGetWithOpts(ls *lua.LState) int {
	rawURL := ls.CheckString(1)
	headers := optHeadersTable(ls, 2)
	res := e.httpDo(ls.Context(), http.MethodGet, rawURL, "", "", headers)
	ls.Push(e.luaResponseTable(ls, res, "http get "+rawURL))
	return 1
}

// luaResponseTable renders the result as the script-facing response
// table, raising on the transport error and the body cap. Typed
// transport failures (the netclient sentinel classes) raise under the
// anicli:<kind>: marker so classifyVMError re-attaches the sentinel —
// consumer errors.Is branches hold for Lua providers like they do for
// the compiled ones (PR116).
func (e *Engine) luaResponseTable(ls *lua.LState, r httpResult, what string) *lua.LTable {
	if r.err != nil {
		if kind := transportErrorKind(r.err); kind != "" {
			ls.RaiseError("anicli:%s:%s: %v", kind, what, r.err)
			return nil
		}
		ls.RaiseError("%s: %v", what, r.err)
		return nil
	}
	if int64(len(r.body)) > e.cfg.BodyLimit {
		ls.RaiseError("%s: response body %d bytes exceeds the sandbox cap of %d bytes",
			what, len(r.body), e.cfg.BodyLimit)
		return nil
	}
	out := ls.NewTable()
	out.RawSetString("status", lua.LNumber(r.status))
	hdrTbl := ls.NewTable()
	for k, v := range r.headers {
		hdrTbl.RawSetString(k, lua.LString(v))
	}
	out.RawSetString("headers", hdrTbl)
	out.RawSetString("body", lua.LString(r.body))
	return out
}

// transportErrorKind maps the netclient sentinel classes onto the
// anicli.fail marker kinds ("" = no typed class, raise plain).
func transportErrorKind(err error) string {
	switch {
	case errors.Is(err, contracts.ErrProvider403):
		return "provider_403"
	case errors.Is(err, contracts.ErrGeoBlocked):
		return "geo_blocked"
	case errors.Is(err, contracts.ErrNotFound):
		// The typed miss (netclient maps HTTP 404 here): the animevost
		// live API answers search misses this way — the marker keeps
		// the sentinel re-attachment, so a Lua provider's miss is
		// errors.Is(ErrNotFound) exactly like the compiled ones.
		return "not_found"
	case errors.Is(err, contracts.ErrProviderTimeout):
		return "timeout"
	default:
		return ""
	}
}

// optHeadersTable reads the optional opts table at position idx and
// extracts its string:string headers map (absent opts/headers → nil).
func optHeadersTable(ls *lua.LState, idx int) map[string]string {
	if ls.GetTop() < idx {
		return nil
	}
	tbl, ok := ls.Get(idx).(*lua.LTable)
	if !ok {
		return nil
	}
	hdrs, ok := tbl.RawGetH(lua.LString("headers")).(*lua.LTable)
	if !ok {
		return nil
	}
	out := make(map[string]string, hdrs.Len())
	hdrs.ForEach(func(k, v lua.LValue) {
		ks, kok := k.(lua.LString)
		vs, vok := v.(lua.LString)
		if kok && vok {
			out[string(ks)] = string(vs)
		}
	})
	return out
}

// sdkHTTPGetBatch implements http.get_batch(urls, max_parallel): the
// bounded-parallel fan-out (netclient.Parallel) for the per-dub page
// fetches a provider script needs. Results align by index; a dead URL
// carries {error = "..."} — soft per-URL failure, one dead leg never
// kills the batch. max_parallel ≤ 0 clamps to 1: netclient.Parallel
// treats ≤0 as unbounded and that must not leak into scripts (the
// wave-A review F3 clamp precedent).
func (e *Engine) sdkHTTPGetBatch(ls *lua.LState) int {
	urlsTbl, ok := ls.Get(1).(*lua.LTable)
	if !ok {
		ls.RaiseError("http.get_batch: expected a table of urls, got %s", typeName(ls.Get(1)))
		return 0
	}
	maxParallel := ls.CheckInt(2)
	if maxParallel <= 0 {
		maxParallel = 1
	}

	urls := make([]string, 0, urlsTbl.Len())
	for i := 1; i <= urlsTbl.Len(); i++ {
		u, ok := urlsTbl.RawGetInt(i).(lua.LString)
		if !ok {
			ls.RaiseError("http.get_batch: urls[%d]: expected string, got %s", i, typeName(urlsTbl.RawGetInt(i)))
			return 0
		}
		urls = append(urls, string(u))
	}

	ctx := ls.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	results := make([]httpResult, len(urls))
	indices := make([]int, len(urls))
	for i := range indices {
		indices[i] = i
	}
	// Per-URL failures are the SOFT path (the error field); a Parallel
	// error means the shared context died — the VM is being torn down
	// anyway, and the result table then simply never gets read.
	_ = netclient.Parallel(ctx, indices, maxParallel, func(_ context.Context, i int) error {
		results[i] = e.httpDo(ctx, http.MethodGet, urls[i], "", "", nil)
		return nil
	})

	out := ls.NewTable()
	for i, res := range results {
		entry := ls.NewTable()
		if res.err != nil {
			entry.RawSetString("error", lua.LString(res.err.Error()))
		} else {
			entry.RawSetString("status", lua.LNumber(res.status))
			hdrTbl := ls.NewTable()
			for k, v := range res.headers {
				hdrTbl.RawSetString(k, lua.LString(v))
			}
			entry.RawSetString("headers", hdrTbl)
			entry.RawSetString("body", lua.LString(res.body))
		}
		out.RawSetInt(i+1, entry)
	}
	ls.Push(out)
	return 1
}

// sdkExtract implements anicli.extract(links): links is one embed URL
// or an array of them. The shared extractor factory resolves the
// embeds (the .mp4/.m3u8 fast path, the dict.update merge and the
// only-when-nothing-resolved error rule live in extractors.Resolve —
// the compiled providers' resolveEmbeds delegates to the same loop).
// Raises on total failure: an extract that yields nothing is a typed
// failure, never a silent empty table.
func (e *Engine) sdkExtract(ls *lua.LState) int {
	var links []string
	switch arg := ls.Get(1).(type) {
	case lua.LString:
		links = []string{string(arg)}
	case *lua.LTable:
		n := arg.Len()
		links = make([]string, 0, n)
		for i := 1; i <= n; i++ {
			u, ok := arg.RawGetInt(i).(lua.LString)
			if !ok {
				ls.RaiseError("extract: links[%d]: expected string, got %s", i, typeName(arg.RawGetInt(i)))
				return 0
			}
			links = append(links, string(u))
		}
	default:
		ls.RaiseError("extract: expected a url string or a table of urls, got %s", typeName(ls.Get(1)))
		return 0
	}

	ctx := ls.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	sources, err := extractors.Resolve(ctx, e.resolveClient(), links)
	if err != nil {
		ls.RaiseError("extract: %v", err)
		return 0
	}
	if len(sources) == 0 {
		ls.RaiseError("extract: no extractor yielded links for %d link(s)", len(links))
		return 0
	}

	out := ls.NewTable()
	for quality, src := range sources {
		out.RawSetH(lua.LString(quality), videoSourceTable(ls, src))
	}
	ls.Push(out)
	return 1
}

// videoSourceTable renders one contracts.VideoSource as the
// script-facing quality entry; empty fields stay absent.
func videoSourceTable(ls *lua.LState, src contracts.VideoSource) *lua.LTable {
	t := ls.NewTable()
	t.RawSetString("url", lua.LString(src.URL))
	t.RawSetString("quality", lua.LString(src.Quality))
	if src.Type != "" {
		t.RawSetString("type", lua.LString(src.Type))
	}
	if len(src.Headers) > 0 {
		h := ls.NewTable()
		for k, v := range src.Headers {
			h.RawSetString(k, lua.LString(v))
		}
		t.RawSetString("headers", h)
	}
	if len(src.ExtraMPVOpts) > 0 {
		o := ls.NewTable()
		for i, v := range src.ExtraMPVOpts {
			o.RawSetInt(i+1, lua.LString(v))
		}
		t.RawSetString("extra_mpv_opts", o)
	}
	return t
}
