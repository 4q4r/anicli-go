package lua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// ErrInvalidScript marks a provider script that failed to load: a
// syntax error, a non-table return, an id mismatch, a missing mandatory
// function or an unknown capability. errors.Is-able so discovery and
// the registry can classify rejections without string matching.
var ErrInvalidScript = errors.New("invalid provider script")

// sourceTypeByCapability maps the script-facing capabilities string
// onto the consumer-side contracts.SourceType catalog assessment.
var sourceTypeByCapability = map[string]contracts.SourceType{
	"video": contracts.SourceTypeVideo,
	"audio": contracts.SourceTypeAudio,
	"both":  contracts.SourceTypeBoth,
}

// Provider is a Lua-backed anime source: the script bytes plus the
// metadata captured at load time. It satisfies contracts.Provider by
// re-running the script in a FRESH sandboxed LState per invocation
// (engine.go) and calling the requested method inside it — scripts
// carry no state between calls and can never observe each other.
type Provider struct {
	engine     *Engine
	id         string
	name       string
	baseURL    string
	sourceType contracts.SourceType
	src        string

	// Optional capability declarations (caps.go): Adapt wraps the
	// provider for the ones the script actually declared.
	contentLang string
	smokeQuery  string
	namePref    contracts.NamePreference
}

// Compile-time proof of the consumer-side contract.
var _ contracts.Provider = (*Provider)(nil)

// loadErrf wraps every load-time rejection in ErrInvalidScript while
// keeping the precise cause visible.
func loadErrf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidScript, fmt.Sprintf(format, args...))
}

// LoadProvider validates a provider script (chunkname
// "providers/<id>/main.lua" so tracebacks point into the user's file)
// and captures its metadata. The mandatory table shape: id (must equal
// the directory name), search, episodes and streams functions; optional
// name, base_url and capabilities ("video"|"audio"|"both", default
// "both").
func (e *Engine) LoadProvider(dirID, src string) (*Provider, error) {
	p := &Provider{
		engine:     e,
		id:         dirID,
		name:       dirID,
		baseURL:    "",
		sourceType: contracts.SourceTypeBoth,
		src:        src,
	}

	sctx, cancel := e.stateCtx(context.Background())
	defer cancel()
	ls := e.NewState(sctx)
	defer ls.Close()

	if err := p.runChunk(ls, sctx, "load"); err != nil {
		return nil, loadErrf("%v", err)
	}
	ret := ls.Get(-1)
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil, loadErrf("provider %q: script returned %s, want the provider table", dirID, typeName(ret))
	}

	idVal := tbl.RawGetH(lua.LString("id"))
	id, ok := idVal.(lua.LString)
	if !ok {
		return nil, loadErrf("provider %q: id: expected string, got %s", dirID, typeName(idVal))
	}
	if string(id) != dirID {
		return nil, loadErrf("provider %q: script declares id %q — the script id must match the directory name", dirID, string(id))
	}
	for _, fnName := range []string{"search", "episodes", "streams"} {
		if _, ok := tbl.RawGetH(lua.LString(fnName)).(*lua.LFunction); !ok {
			return nil, loadErrf("provider %q: missing %s function", dirID, fnName)
		}
	}

	vld := newValidator(dirID, "load")
	if name, present, err := vld.optStr(tbl, "name", "provider"); err != nil {
		return nil, loadErrf("%v", err)
	} else if present {
		p.name = name
	}
	if baseURL, present, err := vld.optStr(tbl, "base_url", "provider"); err != nil {
		return nil, loadErrf("%v", err)
	} else if present {
		p.baseURL = baseURL
	}
	if capability, present, err := vld.optStr(tbl, "capabilities", "provider"); err != nil {
		return nil, loadErrf("%v", err)
	} else if present {
		st, known := sourceTypeByCapability[capability]
		if !known {
			return nil, loadErrf("provider %q: capabilities %q is not one of video|audio|both", dirID, capability)
		}
		p.sourceType = st
	}
	if lang, present, err := vld.optStr(tbl, "content_lang", "provider"); err != nil {
		return nil, loadErrf("%v", err)
	} else if present {
		p.contentLang = lang
	}
	if query, present, err := vld.optStr(tbl, "smoke_query", "provider"); err != nil {
		return nil, loadErrf("%v", err)
	} else if present {
		p.smokeQuery = query
	}
	if pref, present, err := vld.optStr(tbl, "name_preference", "provider"); err != nil {
		return nil, loadErrf("%v", err)
	} else if present {
		switch pref {
		case "latin":
			p.namePref = contracts.NamePrefLatin
		default:
			return nil, loadErrf("provider %q: name_preference %q is not one of latin", dirID, pref)
		}
	}

	return p, nil
}

// ID returns the stable provider identifier (the directory name).
func (p *Provider) ID() string { return p.id }

// HTTPClient forwards the engine's per-provider transport (the
// torrent preflight seam — see Engine.HTTPClient; the luaTorrent
// adapter probes the surfaced .torrent links through it).
func (p *Provider) HTTPClient() *netclient.Client { return p.engine.HTTPClient() }

// Name returns the human-readable provider name (falls back to the id).
func (p *Provider) Name() string { return p.name }

// BaseURL returns the provider site root URL ("" when undeclared).
func (p *Provider) BaseURL() string { return p.baseURL }

// SourceType reports the catalog-wide content assessment the script
// declares via capabilities.
func (p *Provider) SourceType() contracts.SourceType { return p.sourceType }

// SetLogger re-routes the engine diagnostics (script print, SDK logs)
// to log — the registry's logger seam (PR62 #4: TUI file sink, never
// stderr in alt-screen). The promoted method also serves the
// capability adapters wrapping the provider.
func (p *Provider) SetLogger(log *slog.Logger) {
	if log == nil {
		return
	}
	p.engine.log = log
}

// Search calls the script's search(query) and validates the result
// array into contracts.SearchResult values.
func (p *Provider) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	ret, err := p.callMethod(ctx, contracts.OpSearch, lua.LString(query))
	if err != nil {
		return nil, err
	}
	return p.decodeSearch(ret)
}

// GetEpisodes calls the script's episodes(anime_url).
func (p *Provider) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	ret, err := p.callMethod(ctx, contracts.OpGetEpisodes, lua.LString(animeURL))
	if err != nil {
		return nil, err
	}
	return p.decodeEpisodes(ret)
}

// ResolveStream calls the script's streams(episode_url, dub) where
// episode_url is the episode's RawID and dub the chosen dub id.
func (p *Provider) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	ret, err := p.callMethod(ctx, contracts.OpResolveStream, lua.LString(episode.RawID), lua.LString(dubID))
	if err != nil {
		return contracts.MediaStream{}, err
	}
	return p.decodeStream(ret)
}

// luaFnByOp maps the contracts op names onto the script-facing
// function names of the provider table.
var luaFnByOp = map[string]string{
	contracts.OpSearch:        "search",
	contracts.OpGetEpisodes:   "episodes",
	contracts.OpResolveStream: "streams",
}

// callMethod runs the script fresh in its own sandboxed LState and
// PCalls the named provider function with args, returning its first
// result. VM errors are wrapped in the provider "<id>" <op> format with
// the script traceback; timeouts are typed contracts.ErrProviderTimeout
// so one broken provider can never wedge the parallel fan-out.
func (p *Provider) callMethod(ctx context.Context, op string, args ...lua.LValue) (lua.LValue, error) {
	sctx, cancel := p.engine.stateCtx(ctx)
	defer cancel()
	ls := p.engine.NewState(sctx)
	defer ls.Close()

	if err := p.runChunk(ls, sctx, op); err != nil {
		return nil, err
	}
	ret := ls.Get(-1)
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil, loadErrf("provider %q %s: script returned %s, want the provider table", p.id, op, typeName(ret))
	}
	method, ok := tbl.RawGetH(lua.LString(luaFnByOp[op])).(*lua.LFunction)
	if !ok {
		// The script re-runs per invocation: it COULD return a
		// different table every time, so the mandatory shape is
		// re-verified here, not just at load.
		return nil, loadErrf("provider %q: missing %s function", p.id, luaFnByOp[op])
	}

	ls.Push(method)
	for _, arg := range args {
		ls.Push(arg)
	}
	if err := ls.PCall(len(args), 1, nil); err != nil {
		return nil, p.classifyVMError(sctx, op, err)
	}
	return ls.Get(-1), nil
}

// runChunk loads and executes the script chunk (NRet 1: the provider
// table). The chunkname embeds the provider id so every traceback
// points at providers/<id>/main.lua.
func (p *Provider) runChunk(ls *lua.LState, ctx context.Context, op string) error {
	chunk := "providers/" + p.id + "/main.lua"
	fn, err := ls.Load(strings.NewReader(p.src), chunk)
	if err != nil {
		return fmt.Errorf(`provider %q %s: %s`, p.id, op, err.Error())
	}
	ls.Push(fn)
	if err := ls.PCall(0, 1, nil); err != nil {
		return p.classifyVMError(ctx, op, err)
	}
	return nil
}

// classifyVMError maps VM failures onto the consumer taxonomy: caller
// cancellation stays context.Canceled (the fan-out is aborting on
// purpose), deadline exhaustion becomes the typed
// contracts.ErrProviderTimeout, the anicli.fail markers re-attach
// their contracts sentinel INSIDE a ProviderError (PR116 — the typed
// walls are consumer-visible exactly like the compiled providers'
// contracts.WrapProvider walls) and everything else carries the
// provider id, the operation and the script traceback verbatim.
func (p *Provider) classifyVMError(ctx context.Context, op string, err error) error {
	msg := err.Error()
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || strings.Contains(msg, "context canceled"):
		return context.Canceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(msg, "context deadline exceeded"):
		return fmt.Errorf(`provider %q %s: %w: %s`, p.id, op, contracts.ErrProviderTimeout, msg)
	}
	for marker, sentinel := range sdkErrorKinds {
		if strings.Contains(msg, "anicli:"+marker+":") {
			return contracts.WrapProvider(p.id, op, 0, fmt.Errorf("%w: %s", sentinel, msg))
		}
	}
	return fmt.Errorf(`provider %q %s: %w`, p.id, op, err)
}

// decodeSearch validates the search(query) return: an array of
// {title, url[, poster, meta]} tables. A nil return means zero hits.
func (p *Provider) decodeSearch(ret lua.LValue) ([]contracts.SearchResult, error) {
	vld := newValidator(p.id, contracts.OpSearch)
	if ret == lua.LNil {
		return []contracts.SearchResult{}, nil
	}
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil, vld.errf("results: expected table, got %s", typeName(ret))
	}
	n := tbl.Len()
	out := make([]contracts.SearchResult, 0, n)
	for i := 1; i <= n; i++ {
		path := fmt.Sprintf("results[%d]", i)
		item, ok := tbl.RawGetInt(i).(*lua.LTable)
		if !ok {
			return nil, vld.errf(path, "expected table, got %s", typeName(tbl.RawGetInt(i)))
		}
		title, err := vld.str(item, "title", path)
		if err != nil {
			return nil, err
		}
		url, err := vld.str(item, "url", path)
		if err != nil {
			return nil, err
		}
		poster, _, err := vld.optStr(item, "poster", path)
		if err != nil {
			return nil, err
		}
		meta, err := decodeAnyMap(item, "meta", path)
		if err != nil {
			return nil, fmt.Errorf("provider %q %s: %w", p.id, contracts.OpSearch, err)
		}
		out = append(out, contracts.SearchResult{
			Title:    title,
			URL:      url,
			SourceID: p.id,
			Poster:   poster,
			Meta:     meta,
		})
	}
	return out, nil
}

// decodeEpisodes validates the episodes(anime_url) return: an array of
// {num, raw_id[, title, raw_embeds]} tables. Numbers coerce Lua-style.
func (p *Provider) decodeEpisodes(ret lua.LValue) ([]contracts.Episode, error) {
	vld := newValidator(p.id, contracts.OpGetEpisodes)
	if ret == lua.LNil {
		return []contracts.Episode{}, nil
	}
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil, vld.errf("episodes: expected table, got %s", typeName(ret))
	}
	n := tbl.Len()
	out := make([]contracts.Episode, 0, n)
	for i := 1; i <= n; i++ {
		path := fmt.Sprintf("episodes[%d]", i)
		item, ok := tbl.RawGetInt(i).(*lua.LTable)
		if !ok {
			return nil, vld.errf(path, "expected table, got %s", typeName(tbl.RawGetInt(i)))
		}
		num, err := vld.numStr(item, "num", path)
		if err != nil {
			return nil, err
		}
		rawID, err := vld.numStr(item, "raw_id", path)
		if err != nil {
			return nil, err
		}
		title, _, err := vld.optStr(item, "title", path)
		if err != nil {
			return nil, err
		}
		ep := contracts.Episode{Num: num, Title: title, RawID: rawID}

		embeds, present, err := vld.optTable(item, "raw_embeds", path)
		if err != nil {
			return nil, err
		}
		if present {
			ep.RawEmbeds = make(map[string][]string, embeds.Len())
			var embedErr error
			embeds.ForEach(func(dub, urls lua.LValue) {
				if embedErr != nil {
					return
				}
				dubName, ok := dub.(lua.LString)
				if !ok {
					embedErr = vld.errf(path+".raw_embeds", "expected string dub name key, got %s", typeName(dub))
					return
				}
				arr, ok := urls.(*lua.LTable)
				if !ok {
					embedErr = vld.errf(path+".raw_embeds."+string(dubName), "expected table, got %s", typeName(urls))
					return
				}
				list, err := decodeStringArray(vld, arr, path+".raw_embeds."+string(dubName))
				if err != nil {
					embedErr = err
					return
				}
				ep.RawEmbeds[string(dubName)] = list
			})
			if embedErr != nil {
				return nil, embedErr
			}
		}
		out = append(out, ep)
	}
	return out, nil
}

// decodeStream validates the streams(episode_url, dub) return: one
// {dub_name, links = {quality = {url[, quality, headers,
// extra_mpv_opts, type]}}} table. A nil return is an extract failure —
// a stream was explicitly requested.
func (p *Provider) decodeStream(ret lua.LValue) (contracts.MediaStream, error) {
	vld := newValidator(p.id, contracts.OpResolveStream)
	if ret == lua.LNil {
		return contracts.MediaStream{}, vld.errf("stream: returned nil (extract failed)")
	}
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return contracts.MediaStream{}, vld.errf("stream: expected table, got %s", typeName(ret))
	}

	const path = "stream"
	dubName, err := vld.str(tbl, "dub_name", path)
	if err != nil {
		return contracts.MediaStream{}, err
	}
	links, err := vld.table(tbl, "links", path)
	if err != nil {
		return contracts.MediaStream{}, err
	}

	stream := contracts.MediaStream{DubName: dubName, Links: make(map[string]contracts.VideoSource, links.Len())}
	var linkErr error
	links.ForEach(func(quality, src lua.LValue) {
		if linkErr != nil {
			return
		}
		q, ok := quality.(lua.LString)
		if !ok {
			linkErr = vld.errf("%s: expected string quality key, got %s", path+".links", typeName(quality))
			return
		}
		source, ok := src.(*lua.LTable)
		if !ok {
			linkErr = vld.errf("%s: expected table, got %s", path+".links."+string(q), typeName(src))
			return
		}
		spath := path + ".links[" + string(q) + "]"
		vs, err := decodeVideoSource(vld, source, spath, string(q))
		if err != nil {
			linkErr = err
			return
		}
		stream.Links[string(q)] = vs
	})
	if linkErr != nil {
		return contracts.MediaStream{}, linkErr
	}
	return stream, nil
}

// decodeVideoSource validates one quality entry of a stream's links.
func decodeVideoSource(vld *validator, tbl *lua.LTable, path, quality string) (contracts.VideoSource, error) {
	url, err := vld.str(tbl, "url", path)
	if err != nil {
		return contracts.VideoSource{}, err
	}
	vs := contracts.VideoSource{URL: url, Quality: quality}

	if q, present, err := vld.optStr(tbl, "quality", path); err != nil {
		return contracts.VideoSource{}, err
	} else if present {
		vs.Quality = q
	}
	if typ, present, err := vld.optStr(tbl, "type", path); err != nil {
		return contracts.VideoSource{}, err
	} else if present {
		vs.Type = typ
	}
	if headers, present, err := vld.optTable(tbl, "headers", path); err != nil {
		return contracts.VideoSource{}, err
	} else if present {
		hdrs, err := decodeStringMap(vld, headers, path+".headers")
		if err != nil {
			return contracts.VideoSource{}, err
		}
		vs.Headers = hdrs
	}
	if opts, present, err := vld.optTable(tbl, "extra_mpv_opts", path); err != nil {
		return contracts.VideoSource{}, err
	} else if present {
		list, err := decodeStringArray(vld, opts, path+".extra_mpv_opts")
		if err != nil {
			return contracts.VideoSource{}, err
		}
		vs.ExtraMPVOpts = list
	}
	return vs, nil
}

// decodeStringArray validates a Lua array of strings.
func decodeStringArray(vld *validator, arr *lua.LTable, path string) ([]string, error) {
	n := arr.Len()
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		v := arr.RawGetInt(i)
		s, ok := v.(lua.LString)
		if !ok {
			return nil, vld.errf("%s[%d]: expected string, got %s", path, i, typeName(v))
		}
		out = append(out, string(s))
	}
	return out, nil
}

// decodeStringMap validates a string-keyed table of string values
// (playback headers).
func decodeStringMap(vld *validator, tbl *lua.LTable, path string) (map[string]string, error) {
	out := make(map[string]string, tbl.Len())
	var mapErr error
	tbl.ForEach(func(k, v lua.LValue) {
		if mapErr != nil {
			return
		}
		key, ok := k.(lua.LString)
		if !ok {
			mapErr = vld.errf("%s: expected string key, got %s", path, typeName(k))
			return
		}
		val, ok := v.(lua.LString)
		if !ok {
			mapErr = vld.errf("%s.%s: expected string, got %s", path, key, typeName(v))
			return
		}
		out[string(key)] = string(val)
	})
	if mapErr != nil {
		return nil, mapErr
	}
	return out, nil
}

// typeName renders a Lua value type the way Lua's type() does — the
// name embedded in validation errors.
func typeName(v lua.LValue) string {
	return v.Type().String()
}

// decodeAnyMap validates an optional string-keyed table of JSON-ish
// values (contracts.SearchResult.Meta). Cycles and unsupported value
// types are errors, never silent loss.
func decodeAnyMap(tbl *lua.LTable, field, path string) (map[string]any, error) {
	raw := tbl.RawGetH(lua.LString(field))
	if raw == lua.LNil {
		return nil, nil
	}
	metaTbl, ok := raw.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("%s.%s: expected table, got %s", path, field, typeName(raw))
	}
	g, err := ToGoValue(metaTbl)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", path, field, err)
	}
	m, ok := g.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s: expected table, got %T", path, field, g)
	}
	return m, nil
}

// LoadProviderBytes is the package-level entry used by discovery:
// it builds an engine from the given config and loads one script.
func LoadProviderBytes(cfg Config, log *slog.Logger, id string, src []byte) (*Provider, error) {
	return NewEngine(cfg, log).LoadProvider(id, string(src))
}
