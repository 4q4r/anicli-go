package lua

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/yuin/gopher-lua"
)

// maxConvertDepth bounds Lua→Go conversion nesting; scripts cannot
// recurse the converter into stack death.
const maxConvertDepth = 32

// ToLuaValue converts a plain Go value into its Lua representation for
// script consumption: scalars map 1:1, []any becomes an array table
// (1..n) and map[string]any a string-keyed table. Values arriving from
// encoding/json (map[string]any / []any / json.Number) therefore pass
// through losslessly.
func ToLuaValue(ls *lua.LState, v any) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case string:
		return lua.LString(val)
	case int:
		return lua.LNumber(val)
	case int64:
		return lua.LNumber(val)
	case float64:
		return lua.LNumber(val)
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return lua.LNumber(f)
		}
		return lua.LString(val.String())
	case []any:
		t := ls.NewTable()
		for i, item := range val {
			t.RawSetInt(i+1, ToLuaValue(ls, item))
		}
		return t
	case map[string]any:
		t := ls.NewTable()
		for k, item := range val {
			t.RawSetString(k, ToLuaValue(ls, item))
		}
		return t
	default:
		return lua.LString(fmt.Sprint(val))
	}
}

// ToGoValue converts a Lua value into plain Go: tables become
// map[string]any or []any (dense 1..n arrays), numbers become
// json.Number (no float rounding surprises for script-produced ids),
// functions and userdata are refused. Cyclic tables and nesting past
// maxConvertDepth error out.
func ToGoValue(v lua.LValue) (any, error) {
	return toGo(v, 0, map[*lua.LTable]bool{})
}

func toGo(v lua.LValue, depth int, seen map[*lua.LTable]bool) (any, error) {
	if depth > maxConvertDepth {
		return nil, fmt.Errorf("lua convert depth exceeded %d", maxConvertDepth)
	}
	switch lv := v.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(lv), nil
	case lua.LString:
		return string(lv), nil
	case lua.LNumber:
		return json.Number(strconv.FormatFloat(float64(lv), 'f', -1, 64)), nil
	case *lua.LTable:
		if seen[lv] {
			return nil, fmt.Errorf("cyclic lua table cannot be converted")
		}
		seen[lv] = true
		defer delete(seen, lv)

		if n := lv.Len(); n > 0 && isArrayTable(lv, n) {
			arr := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				item, err := toGo(lv.RawGetInt(i), depth+1, seen)
				if err != nil {
					return nil, err
				}
				arr = append(arr, item)
			}
			return arr, nil
		}
		m := make(map[string]any)
		var convErr error
		lv.ForEach(func(key, val lua.LValue) {
			if convErr != nil {
				return
			}
			item, err := toGo(val, depth+1, seen)
			if err != nil {
				convErr = err
				return
			}
			m[key.String()] = item
		})
		if convErr != nil {
			return nil, convErr
		}
		return m, nil
	default:
		return nil, fmt.Errorf("cannot convert lua %s to a Go value", lv.Type().String())
	}
}

// isArrayTable reports whether the table's keys are exactly 1..n.
func isArrayTable(t *lua.LTable, n int) bool {
	dense := true
	t.ForEach(func(key, _ lua.LValue) {
		k, ok := key.(lua.LNumber)
		if !ok || float64(k) != float64(int64(k)) || int64(k) < 1 || int64(k) > int64(n) {
			dense = false
		}
	})
	return dense
}

// EncodeJSON encodes a Lua value as JSON: numbers keep their script
// formatting via json.Number, arrays stay arrays.
func EncodeJSON(v lua.LValue) ([]byte, error) {
	g, err := ToGoValue(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(g)
}

// validator builds the precise per-provider, per-operation field
// errors: provider "<id>" <op>: <path>.<field>: expected …, got ….
type validator struct {
	provider string
	op       string
}

func newValidator(provider, op string) *validator {
	return &validator{provider: provider, op: op}
}

func (v *validator) errf(format string, args ...any) error {
	return fmt.Errorf("provider %q %s: %s", v.provider, v.op,
		fmt.Sprintf(format, args...))
}

// joinPath renders a field reference: "results[1].title" — the field
// alone at the top level.
func joinPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

// str reads a required string field.
func (v *validator) str(t *lua.LTable, field, path string) (string, error) {
	val := t.RawGetH(lua.LString(field))
	s, ok := val.(lua.LString)
	if !ok {
		return "", v.errf("%s: expected string, got %s", joinPath(path, field), val.Type().String())
	}
	return string(s), nil
}

// numStr reads a string field that scripts may legitimately fill from
// a Lua number: 1 -> "1", 1.5 -> "1.5" (episode numbers).
func (v *validator) numStr(t *lua.LTable, field, path string) (string, error) {
	val := t.RawGetH(lua.LString(field))
	switch lv := val.(type) {
	case lua.LString:
		return string(lv), nil
	case lua.LNumber:
		return strconv.FormatFloat(float64(lv), 'f', -1, 64), nil
	default:
		return "", v.errf("%s: expected string or number, got %s", joinPath(path, field), val.Type().String())
	}
}

// optStr reads an optional string field: missing -> ("", false, nil).
func (v *validator) optStr(t *lua.LTable, field, path string) (string, bool, error) {
	val := t.RawGetH(lua.LString(field))
	if val == lua.LNil {
		return "", false, nil
	}
	s, ok := val.(lua.LString)
	if !ok {
		return "", false, v.errf("%s: expected string, got %s", joinPath(path, field), val.Type().String())
	}
	return string(s), true, nil
}

// strArray reads a required 1..n array of strings; a bad element names
// its index.
func (v *validator) strArray(t *lua.LTable, field, path string) ([]string, error) {
	arr := t.RawGetH(lua.LString(field))
	tbl, ok := arr.(*lua.LTable)
	if !ok {
		return nil, v.errf("%s: expected table, got %s", joinPath(path, field), arr.Type().String())
	}
	n := tbl.Len()
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		el := tbl.RawGetInt(i)
		s, ok := el.(lua.LString)
		if !ok {
			return nil, v.errf("%s[%d]: expected string, got %s", joinPath(path, field), i, el.Type().String())
		}
		out = append(out, string(s))
	}
	return out, nil
}

// table reads a required nested table field.
func (v *validator) table(t *lua.LTable, field, path string) (*lua.LTable, error) {
	val := t.RawGetH(lua.LString(field))
	tbl, ok := val.(*lua.LTable)
	if !ok {
		return nil, v.errf("%s: expected table, got %s", joinPath(path, field), val.Type().String())
	}
	return tbl, nil
}

// optTable reads an optional nested table field.
func (v *validator) optTable(t *lua.LTable, field, path string) (*lua.LTable, bool, error) {
	val := t.RawGetH(lua.LString(field))
	if val == lua.LNil {
		return nil, false, nil
	}
	tbl, ok := val.(*lua.LTable)
	if !ok {
		return nil, false, v.errf("%s.%s: expected table, got %s", path, field, val.Type().String())
	}
	return tbl, true, nil
}
