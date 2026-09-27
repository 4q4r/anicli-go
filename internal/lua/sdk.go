package lua

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/yuin/gopher-lua"
)

// openSDK registers the global `anicli` module — the batteries-included
// surface scripts program against. Everything routes through the VM's
// context (deadlines carried) and the engine budgets (body caps).
func (e *Engine) openSDK(L *lua.LState) {
	mod := L.NewTable()

	httpTbl := L.NewTable()
	L.SetFuncs(httpTbl, map[string]lua.LGFunction{
		"get":          e.sdkHTTPGet,
		"get_json":     e.sdkHTTPGetJSON,
		"post":         e.sdkHTTPPost,
		"query_escape": e.sdkQueryEscape,
	})
	mod.RawSetString("http", httpTbl)

	jsonTbl := L.NewTable()
	L.SetFuncs(jsonTbl, map[string]lua.LGFunction{
		"decode": e.sdkJSONDecode,
		"encode": e.sdkJSONEncode,
	})
	mod.RawSetString("json", jsonTbl)

	htmlTbl := L.NewTable()
	L.SetFuncs(htmlTbl, map[string]lua.LGFunction{
		"parse": e.sdkHTMLParse,
	})
	mod.RawSetString("html", htmlTbl)

	regexpTbl := L.NewTable()
	L.SetFuncs(regexpTbl, map[string]lua.LGFunction{
		"match": e.sdkRegexpMatch,
	})
	mod.RawSetString("regexp", regexpTbl)

	base64Tbl := L.NewTable()
	L.SetFuncs(base64Tbl, map[string]lua.LGFunction{
		"encode": e.sdkBase64Encode,
		"decode": e.sdkBase64Decode,
	})
	mod.RawSetString("base64", base64Tbl)

	timeTbl := L.NewTable()
	L.SetFuncs(timeTbl, map[string]lua.LGFunction{
		"now": e.sdkTimeNow,
	})
	mod.RawSetString("time", timeTbl)

	logTbl := L.NewTable()
	L.SetFuncs(logTbl, map[string]lua.LGFunction{
		"info":  e.sdkLog("info"),
		"warn":  e.sdkLog("warn"),
		"error": e.sdkLog("error"),
	})
	mod.RawSetString("log", logTbl)

	mod.RawSetString("version", lua.LString(SDKVersion))

	L.SetGlobal("anicli", mod)
}

// sdkHTTP runs one HTTP request with the VM's context and the engine's
// transport, capping the body before it can reach a script.
func (e *Engine) sdkHTTP(L *lua.LState, method, rawURL, body, contentType string) *lua.LTable {
	ctx := L.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	var (
		status  int
		headers map[string]string
		resp    []byte
		err     error
	)
	if e.cfg.HTTP != nil {
		hdrs := map[string]string{}
		if contentType != "" {
			hdrs["Content-Type"] = contentType
		}
		req := netclient.Request{
			Method:  method,
			URL:     rawURL,
			Headers: hdrs,
			Body:    strings.NewReader(body),
			Op:      "lua",
		}
		nr, doErr := e.cfg.HTTP.Do(ctx, req)
		if doErr != nil {
			err = doErr
		} else {
			status, headers, resp = nr.StatusCode, flattenHeaders(nr.Header), nr.Body
		}
	} else {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(body))
		if err == nil {
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			hc := e.stdClient()
			var rs *http.Response
			rs, err = hc.Do(req)
			if err == nil {
				defer rs.Body.Close()
				status = rs.StatusCode
				headers = flattenHeaders(rs.Header)
				resp, err = io.ReadAll(io.LimitReader(rs.Body, e.cfg.BodyLimit+1))
			}
		}
	}
	if err != nil {
		L.RaiseError("http %s %s: %v", method, rawURL, err)
		return nil
	}
	if int64(len(resp)) > e.cfg.BodyLimit {
		L.RaiseError("http %s %s: response body %d bytes exceeds the sandbox cap of %d bytes",
			method, rawURL, len(resp), e.cfg.BodyLimit)
		return nil
	}

	out := L.NewTable()
	out.RawSetString("status", lua.LNumber(status))
	hdrTbl := L.NewTable()
	for k, v := range headers {
		hdrTbl.RawSetString(k, lua.LString(v))
	}
	out.RawSetString("headers", hdrTbl)
	out.RawSetString("body", lua.LString(resp))
	return out
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

func (e *Engine) sdkHTTPGet(L *lua.LState) int {
	url := L.CheckString(1)
	L.Push(e.sdkHTTP(L, http.MethodGet, url, "", ""))
	return 1
}

func (e *Engine) sdkHTTPGetJSON(L *lua.LState) int {
	url := L.CheckString(1)
	resp := e.sdkHTTP(L, http.MethodGet, url, "", "")
	if resp == nil {
		return 0
	}
	body := resp.RawGetString("body")
	var decoded any
	if err := json.Unmarshal([]byte(body.(lua.LString)), &decoded); err != nil {
		L.RaiseError("http.get_json %s: invalid json: %v", url, err)
		return 0
	}
	L.Push(ToLuaValue(L, decoded))
	return 1
}

func (e *Engine) sdkHTTPPost(L *lua.LState) int {
	url := L.CheckString(1)
	body := L.CheckString(2)
	ct := L.OptString(3, "")
	L.Push(e.sdkHTTP(L, http.MethodPost, url, string(body), ct))
	return 1
}

func (e *Engine) sdkQueryEscape(L *lua.LState) int {
	L.Push(lua.LString(url.QueryEscape(L.CheckString(1))))
	return 1
}

func (e *Engine) sdkJSONDecode(L *lua.LState) int {
	var decoded any
	if err := json.Unmarshal([]byte(L.CheckString(1)), &decoded); err != nil {
		L.RaiseError("json.decode: %v", err)
		return 0
	}
	L.Push(ToLuaValue(L, decoded))
	return 1
}

func (e *Engine) sdkJSONEncode(L *lua.LState) int {
	encoded, err := EncodeJSON(L.Get(1))
	if err != nil {
		L.RaiseError("json.encode: %v", err)
		return 0
	}
	L.Push(lua.LString(encoded))
	return 1
}

func (e *Engine) sdkRegexpMatch(L *lua.LState) int {
	pattern := L.CheckString(1)
	s := L.CheckString(2)
	re, err := regexp.Compile(pattern)
	if err != nil {
		L.RaiseError("regexp.match: bad pattern: %v", err)
		return 0
	}
	m := re.FindStringSubmatch(s)
	if m == nil {
		L.Push(lua.LNil)
		return 1
	}
	out := L.NewTable()
	for i, cap := range m {
		out.RawSetInt(i+1, lua.LString(cap))
	}
	L.Push(out)
	return 1
}

func (e *Engine) sdkBase64Encode(L *lua.LState) int {
	L.Push(lua.LString(base64.StdEncoding.EncodeToString([]byte(L.CheckString(1)))))
	return 1
}

func (e *Engine) sdkBase64Decode(L *lua.LState) int {
	decoded, err := base64.StdEncoding.DecodeString(L.CheckString(1))
	if err != nil {
		L.RaiseError("base64.decode: %v", err)
		return 0
	}
	L.Push(lua.LString(decoded))
	return 1
}

func (e *Engine) sdkTimeNow(L *lua.LState) int {
	L.Push(lua.LString(time.Now().UTC().Format(time.RFC3339)))
	return 1
}

func (e *Engine) sdkLog(level string) lua.LGFunction {
	return func(L *lua.LState) int {
		msg := L.CheckString(1)
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
