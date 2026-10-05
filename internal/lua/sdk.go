package lua

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	lua "github.com/yuin/gopher-lua"
)

// openSDK registers the global `anicli` module — the batteries-included
// surface scripts program against. Everything routes through the VM's
// context (deadlines carried) and the engine budgets (body caps).
func (e *Engine) openSDK(ls *lua.LState) {
	mod := ls.NewTable()

	httpTbl := ls.NewTable()
	ls.SetFuncs(httpTbl, map[string]lua.LGFunction{
		"get":          e.sdkHTTPGetWithOpts,
		"get_json":     e.sdkHTTPGetJSON,
		"get_batch":    e.sdkHTTPGetBatch,
		"post":         e.sdkHTTPPost,
		"query_escape": e.sdkQueryEscape,
	})
	mod.RawSetString("http", httpTbl)

	// extract resolves embed URLs through the shared Go extractor
	// factory (the same loop the compiled providers use).
	mod.RawSetString("extract", ls.NewFunction(e.sdkExtract))

	jsonTbl := ls.NewTable()
	ls.SetFuncs(jsonTbl, map[string]lua.LGFunction{
		"decode": e.sdkJSONDecode,
		"encode": e.sdkJSONEncode,
	})
	mod.RawSetString("json", jsonTbl)

	htmlTbl := ls.NewTable()
	ls.SetFuncs(htmlTbl, map[string]lua.LGFunction{
		"parse": e.sdkHTMLParse,
	})
	mod.RawSetString("html", htmlTbl)

	regexpTbl := ls.NewTable()
	ls.SetFuncs(regexpTbl, map[string]lua.LGFunction{
		"match": e.sdkRegexpMatch,
	})
	mod.RawSetString("regexp", regexpTbl)

	base64Tbl := ls.NewTable()
	ls.SetFuncs(base64Tbl, map[string]lua.LGFunction{
		"encode": e.sdkBase64Encode,
		"decode": e.sdkBase64Decode,
	})
	mod.RawSetString("base64", base64Tbl)

	timeTbl := ls.NewTable()
	ls.SetFuncs(timeTbl, map[string]lua.LGFunction{
		"now": e.sdkTimeNow,
	})
	mod.RawSetString("time", timeTbl)

	logTbl := ls.NewTable()
	ls.SetFuncs(logTbl, map[string]lua.LGFunction{
		"info":  e.sdkLog("info"),
		"warn":  e.sdkLog("warn"),
		"error": e.sdkLog("error"),
	})
	mod.RawSetString("log", logTbl)

	mod.RawSetString("version", lua.LString(SDKVersion))

	// fail raises a typed provider failure: fail(kind, message) with
	// kind one of not_found|extract_failed|invalid_input. The VM
	// error's message carries the anicli:<kind>: marker; the adapter's
	// classification (provider.go) re-attaches the matching contracts
	// sentinel so consumer errors.Is branches work identically for
	// Lua providers (PR116).
	mod.RawSetString("fail", ls.NewFunction(e.sdkFail))

	ls.SetGlobal("anicli", mod)
}

// sdkErrorKinds maps the script-facing failure kinds onto the
// contracts sentinels the adapter classification wraps. The three
// script kinds (anicli.fail) plus the transport classes the SDK HTTP
// layer raises under markers itself (transportErrorKind).
var sdkErrorKinds = map[string]error{
	"not_found":      contracts.ErrNotFound,
	"extract_failed": contracts.ErrExtractFailed,
	"invalid_input":  contracts.ErrInvalidInput,
	"provider_403":   contracts.ErrProvider403,
	"geo_blocked":    contracts.ErrGeoBlocked,
	"timeout":        contracts.ErrProviderTimeout,
}

// sdkFail implements anicli.fail(kind, message).
func (e *Engine) sdkFail(ls *lua.LState) int {
	kind := ls.CheckString(1)
	if _, known := sdkErrorKinds[kind]; !known {
		ls.RaiseError("anicli.fail: unknown kind %q (want not_found|extract_failed|invalid_input)", kind)
		return 0
	}
	msg := ls.CheckString(2)
	ls.RaiseError("anicli:%s:%s", kind, msg)
	return 0
}

// sdkHTTP runs one HTTP request with the VM's context and the engine's
// transport, capping the body before it can reach a script. The
// transport/error branches live in httpDo (sdk_ext.go) — shared with
// http.get / http.get_batch.
func (e *Engine) sdkHTTP(ls *lua.LState, method, rawURL, body, contentType string) *lua.LTable {
	ctx := ls.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return e.luaResponseTable(ls, e.httpDo(ctx, method, rawURL, body, contentType, nil), method+" "+rawURL)
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

func (e *Engine) sdkHTTPGetJSON(ls *lua.LState) int {
	url := ls.CheckString(1)
	resp := e.sdkHTTP(ls, http.MethodGet, url, "", "")
	if resp == nil {
		return 0
	}
	body := resp.RawGetString("body")
	var decoded any
	if err := json.Unmarshal([]byte(body.(lua.LString)), &decoded); err != nil {
		ls.RaiseError("http.get_json %s: invalid json: %v", url, err)
		return 0
	}
	ls.Push(ToLuaValue(ls, decoded))
	return 1
}

func (e *Engine) sdkHTTPPost(ls *lua.LState) int {
	url := ls.CheckString(1)
	body := ls.CheckString(2)
	ct := ls.OptString(3, "")
	ls.Push(e.sdkHTTP(ls, http.MethodPost, url, string(body), ct))
	return 1
}

func (e *Engine) sdkQueryEscape(ls *lua.LState) int {
	ls.Push(lua.LString(url.QueryEscape(ls.CheckString(1))))
	return 1
}

func (e *Engine) sdkJSONDecode(ls *lua.LState) int {
	var decoded any
	if err := json.Unmarshal([]byte(ls.CheckString(1)), &decoded); err != nil {
		ls.RaiseError("json.decode: %v", err)
		return 0
	}
	ls.Push(ToLuaValue(ls, decoded))
	return 1
}

func (e *Engine) sdkJSONEncode(ls *lua.LState) int {
	encoded, err := EncodeJSON(ls.Get(1))
	if err != nil {
		ls.RaiseError("json.encode: %v", err)
		return 0
	}
	ls.Push(lua.LString(encoded))
	return 1
}

func (e *Engine) sdkRegexpMatch(ls *lua.LState) int {
	pattern := ls.CheckString(1)
	s := ls.CheckString(2)
	re, err := regexp.Compile(pattern)
	if err != nil {
		ls.RaiseError("regexp.match: bad pattern: %v", err)
		return 0
	}
	m := re.FindStringSubmatch(s)
	if m == nil {
		ls.Push(lua.LNil)
		return 1
	}
	out := ls.NewTable()
	for i, cap := range m {
		out.RawSetInt(i+1, lua.LString(cap))
	}
	ls.Push(out)
	return 1
}

func (e *Engine) sdkBase64Encode(ls *lua.LState) int {
	ls.Push(lua.LString(base64.StdEncoding.EncodeToString([]byte(ls.CheckString(1)))))
	return 1
}

func (e *Engine) sdkBase64Decode(ls *lua.LState) int {
	decoded, err := base64.StdEncoding.DecodeString(ls.CheckString(1))
	if err != nil {
		ls.RaiseError("base64.decode: %v", err)
		return 0
	}
	ls.Push(lua.LString(decoded))
	return 1
}

func (e *Engine) sdkTimeNow(ls *lua.LState) int {
	ls.Push(lua.LString(time.Now().UTC().Format(time.RFC3339)))
	return 1
}

func (e *Engine) sdkLog(level string) lua.LGFunction {
	return func(ls *lua.LState) int {
		msg := ls.CheckString(1)
		switch level {
		case "warn":
			e.log.Warn("lua: " + msg)
		case "error":
			e.log.Error("lua: " + msg)
		default:
			e.log.Info("lua: " + msg)
		}
		return 0
	}
}
