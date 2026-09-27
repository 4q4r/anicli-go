package lua

import (
	"encoding/json"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// --- ToLuaValue: Go -> Lua -------------------------------------------------

func TestToLuaValuePrimitives(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	tests := []struct {
		name string
		in   any
		want lua.LValue
	}{
		{"nil", nil, lua.LNil},
		{"bool true", true, lua.LTrue},
		{"bool false", false, lua.LFalse},
		{"string", "hello", lua.LString("hello")},
		{"int", 42, lua.LNumber(42)},
		{"int64", int64(-7), lua.LNumber(-7)},
		{"float", 1.5, lua.LNumber(1.5)},
		{"json.Number int", json.Number("42"), lua.LNumber(42)},
		{"json.Number float", json.Number("1.25"), lua.LNumber(1.25)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ToLuaValue(ls, tt.in)
			if got.Type() != tt.want.Type() || got.String() != tt.want.String() {
				t.Fatalf("ToLuaValue(%v) = %v (%s), want %v (%s)",
					tt.in, got, got.Type().String(), tt.want, tt.want.Type().String())
			}
		})
	}
}

func TestToLuaValueContainers(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	arr := ToLuaValue(ls, []any{"a", "b"})
	tbl, ok := arr.(*lua.LTable)
	if !ok {
		t.Fatalf("[]any converted to %s, want table", arr.Type().String())
	}
	if tbl.Len() != 2 {
		t.Fatalf("array len = %d, want 2", tbl.Len())
	}
	if tbl.RawGetInt(1).String() != "a" || tbl.RawGetInt(2).String() != "b" {
		t.Fatalf("array elements = [%s, %s], want [a, b]", tbl.RawGetInt(1), tbl.RawGetInt(2))
	}

	m := ToLuaValue(ls, map[string]any{"k": "v", "n": 3.0})
	mt, ok := m.(*lua.LTable)
	if !ok {
		t.Fatalf("map converted to %s, want table", m.Type().String())
	}
	if mt.RawGetH(lua.LString("k")).String() != "v" {
		t.Fatalf("map[k] = %s, want v", mt.RawGetH(lua.LString("k")))
	}
	if n, okk := mt.RawGetH(lua.LString("n")).(lua.LNumber); !okk || n != 3 {
		t.Fatalf("map[n] = %s, want 3", mt.RawGetH(lua.LString("n")))
	}

	// Nested containers must recurse.
	deep := ToLuaValue(ls, map[string]any{"list": []any{map[string]any{"x": true}}})
	dt := deep.(*lua.LTable)
	list := dt.RawGetH(lua.LString("list")).(*lua.LTable)
	inner := list.RawGetInt(1).(*lua.LTable)
	if inner.RawGetH(lua.LString("x")) != lua.LTrue {
		t.Fatal("nested map value lost")
	}
}

// --- ToGoValue: Lua -> Go ---------------------------------------------------

func TestToGoValuePrimitives(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	tests := []struct {
		name string
		in   lua.LValue
		want any
	}{
		{"nil", lua.LNil, nil},
		{"true", lua.LTrue, true},
		{"string", lua.LString("s"), "s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ToGoValue(tt.in)
			if err != nil {
				t.Fatalf("ToGoValue(%v): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ToGoValue(%v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}

	n, err := ToGoValue(lua.LNumber(2.5))
	if err != nil {
		t.Fatalf("ToGoValue(number): %v", err)
	}
	num, ok := n.(json.Number)
	if !ok || num.String() != "2.5" {
		t.Fatalf("ToGoValue(2.5) = %#v, want json.Number(2.5)", n)
	}
}

func TestToGoValueArrayTable(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	tbl := ls.NewTable()
	tbl.RawSetInt(1, lua.LString("x"))
	tbl.RawSetInt(2, lua.LString("y"))

	got, err := ToGoValue(tbl)
	if err != nil {
		t.Fatalf("ToGoValue(array): %v", err)
	}
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("array table -> %T, want []any", got)
	}
	if len(arr) != 2 || arr[0] != "x" || arr[1] != "y" {
		t.Fatalf("array = %#v, want [x y]", arr)
	}
}

func TestToGoValueMapTable(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	tbl := ls.NewTable()
	tbl.RawSetH(lua.LString("a"), lua.LNumber(1))
	tbl.RawSetH(lua.LString("b"), lua.LTrue)

	got, err := ToGoValue(tbl)
	if err != nil {
		t.Fatalf("ToGoValue(map): %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("map table -> %T, want map[string]any", got)
	}
	if m["a"] != json.Number("1") || m["b"] != true {
		t.Fatalf("map = %#v, want {a:1 b:true}", m)
	}
}

func TestToGoValueCyclicTable(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	tbl := ls.NewTable()
	tbl.RawSetH(lua.LString("self"), tbl) // cycle

	_, err := ToGoValue(tbl)
	if err == nil {
		t.Fatal("cyclic table must error")
	}
	if !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cycle error = %q, want it to mention cyclic", err)
	}
}

func TestToGoValueDepthLimit(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	// Build a nesting chain deeper than maxConvertDepth.
	root := ls.NewTable()
	cur := root
	for range maxConvertDepth + 4 {
		next := ls.NewTable()
		cur.RawSetH(lua.LString("d"), next)
		cur = next
	}

	if _, err := ToGoValue(root); err == nil {
		t.Fatal("over-deep nesting must error")
	}
}

func TestToGoValueUnsupportedType(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	fn := ls.NewFunction(func(*lua.LState) int { return 0 })
	if _, err := ToGoValue(fn); err == nil {
		t.Fatal("function must not convert")
	}
}

// JSON round-trip: a Lua table encoded by encodeValue must survive
// json.Unmarshal back into an equal Go structure.
func TestJSONValueRoundTrip(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	src := `return {a = 1, b = "s", c = {true, 2.5}, d = {nested = "n"}}`
	fn, err := ls.LoadString(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := ls.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
		t.Fatalf("call: %v", err)
	}
	tbl := ls.Get(-1)
	ls.Pop(1)

	encoded, err := EncodeJSON(tbl)
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", encoded, err)
	}
	if back["b"] != "s" || back["d"].(map[string]any)["nested"] != "n" {
		t.Fatalf("round-trip mismatch: %s", encoded)
	}
	if arr, ok := back["c"].([]any); !ok || len(arr) != 2 {
		t.Fatalf("c must be a 2-element array, got %s", encoded)
	}
}

// --- validator: precise field errors ----------------------------------------

func TestValidatorRequiredString(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	v := newValidator("mysite", "search")

	tbl := ls.NewTable()
	tbl.RawSetH(lua.LString("title"), lua.LString("Bebop"))
	got, err := v.str(tbl, "title", "results[1]")
	if err != nil || got != "Bebop" {
		t.Fatalf("str(title) = %q, %v; want Bebop, nil", got, err)
	}

	// Missing field.
	if _, err := v.str(ls.NewTable(), "title", "results[2]"); err == nil {
		t.Fatal("missing required field must error")
	} else if want := `provider "mysite" search: results[2].title: expected string, got nil`; err.Error() != want {
		t.Fatalf("missing field error = %q, want %q", err, want)
	}

	// Wrong type.
	bad := ls.NewTable()
	bad.RawSetH(lua.LString("title"), lua.LNumber(7))
	if _, err := v.str(bad, "title", "results[3]"); err == nil {
		t.Fatal("number title must error")
	} else if want := `provider "mysite" search: results[3].title: expected string, got number`; err.Error() != want {
		t.Fatalf("wrong type error = %q, want %q", err, want)
	}
}

func TestValidatorNumStr(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	v := newValidator("mysite", "episodes")

	// String passes through.
	tbl := ls.NewTable()
	tbl.RawSetH(lua.LString("num"), lua.LString("OVA"))
	if got, err := v.numStr(tbl, "num", "eps[1]"); err != nil || got != "OVA" {
		t.Fatalf("numStr(string) = %q, %v; want OVA, nil", got, err)
	}

	// Integer number coerces Lua-style: 1 -> "1", 1.5 -> "1.5".
	num := ls.NewTable()
	num.RawSetH(lua.LString("num"), lua.LNumber(1))
	if got, err := v.numStr(num, "num", "eps[2]"); err != nil || got != "1" {
		t.Fatalf("numStr(1) = %q, %v; want 1, nil", got, err)
	}
	fnum := ls.NewTable()
	fnum.RawSetH(lua.LString("num"), lua.LNumber(1.5))
	if got, err := v.numStr(fnum, "num", "eps[3]"); err != nil || got != "1.5" {
		t.Fatalf("numStr(1.5) = %q, %v; want 1.5, nil", got, err)
	}

	// Other types error.
	bad := ls.NewTable()
	bad.RawSetH(lua.LString("num"), lua.LTrue)
	want := `provider "mysite" episodes: eps[4].num: expected string or number, got boolean`
	if _, err := v.numStr(bad, "num", "eps[4]"); err == nil || err.Error() != want {
		t.Fatalf("numStr(bool) = %v, want %q", err, want)
	}
}

func TestValidatorOptionalString(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	v := newValidator("mysite", "streams")

	// Missing -> ("", false, nil).
	if got, ok, err := v.optStr(ls.NewTable(), "quality", "links[1]"); got != "" || ok || err != nil {
		t.Fatalf("optStr(missing) = %q, %v, %v; want empty, false, nil", got, ok, err)
	}

	// Present string -> (value, true, nil).
	tbl := ls.NewTable()
	tbl.RawSetH(lua.LString("quality"), lua.LString("1080"))
	if got, ok, err := v.optStr(tbl, "quality", "links[1]"); got != "1080" || !ok || err != nil {
		t.Fatalf("optStr(1080) = %q, %v, %v; want 1080, true, nil", got, ok, err)
	}

	// Wrong type -> precise error.
	bad := ls.NewTable()
	bad.RawSetH(lua.LString("quality"), lua.LTrue)
	want := `provider "mysite" streams: links[1].quality: expected string, got boolean`
	if _, _, err := v.optStr(bad, "quality", "links[1]"); err == nil || err.Error() != want {
		t.Fatalf("optStr(bool) = %v, want %q", err, want)
	}
}

func TestValidatorStrArray(t *testing.T) {
	t.Parallel()

	ls := lua.NewState()
	defer ls.Close()

	v := newValidator("mysite", "episodes")

	tbl := ls.NewTable()
	urls := ls.NewTable()
	urls.RawSetInt(1, lua.LString("https://e/1"))
	urls.RawSetInt(2, lua.LString("https://e/2"))
	tbl.RawSetH(lua.LString("embeds"), urls)

	got, err := v.strArray(tbl, "embeds", "eps[1]")
	if err != nil {
		t.Fatalf("strArray: %v", err)
	}
	if len(got) != 2 || got[0] != "https://e/1" || got[1] != "https://e/2" {
		t.Fatalf("strArray = %#v, want 2 urls", got)
	}

	// Non-string element names the index.
	badURLs := ls.NewTable()
	badURLs.RawSetInt(1, lua.LString("ok"))
	badURLs.RawSetInt(2, lua.LNumber(9))
	bad := ls.NewTable()
	bad.RawSetH(lua.LString("embeds"), badURLs)
	want := `provider "mysite" episodes: eps[2].embeds[2]: expected string, got number`
	if _, err := v.strArray(bad, "embeds", "eps[2]"); err == nil || err.Error() != want {
		t.Fatalf("strArray(bad) = %v, want %q", err, want)
	}
}
