package lua

import (
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/yuin/gopher-lua"
)

// htmlNode is the value behind the html.parse userdata: a document is
// just the root selection, so find/text/attr/len/each work uniformly.
type htmlNode struct {
	sel *goquery.Selection
}

// sdkHTMLParse parses an HTML document into the minimal selection
// model: find by CSS selector, read text/attr, iterate matches.
func (e *Engine) sdkHTMLParse(L *lua.LState) int {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(L.CheckString(1)))
	if err != nil {
		L.RaiseError("html.parse: %v", err)
		return 0
	}
	L.Push(pushHTMLNode(L, &htmlNode{sel: doc.Selection}))
	return 1
}

// pushHTMLNode wraps a selection as a userdata sharing one metatable
// shape for documents and nested selections.
func pushHTMLNode(L *lua.LState, n *htmlNode) *lua.LUserData {
	ud := L.NewUserData()
	ud.Value = n
	mt := L.NewTable()
	L.SetFuncs(mt, map[string]lua.LGFunction{
		"find": htmlFind,
		"text": htmlText,
		"attr": htmlAttr,
		"len":  htmlLen,
		"each": htmlEach,
	})
	mt.RawSetString("__index", mt)
	ud.Metatable = mt
	return ud
}

// nodeOf asserts the userdata argument is an html node.
func nodeOf(L *lua.LState, i int) *htmlNode {
	ud := L.CheckUserData(i)
	n, ok := ud.Value.(*htmlNode)
	if !ok {
		L.RaiseError("html method called on a non-selection value")
		return nil
	}
	return n
}

func htmlFind(L *lua.LState) int {
	n := nodeOf(L, 1)
	sel := n.sel.Find(L.CheckString(2))
	L.Push(pushHTMLNode(L, &htmlNode{sel: sel}))
	return 1
}

func htmlText(L *lua.LState) int {
	n := nodeOf(L, 1)
	// First-node semantics: a multi-match selection concatenates every
	// node's text, which is never what an extraction script wants.
	L.Push(lua.LString(strings.TrimSpace(n.sel.Eq(0).Text())))
	return 1
}

func htmlAttr(L *lua.LState) int {
	n := nodeOf(L, 1)
	val, ok := n.sel.Attr(L.CheckString(2))
	if !ok {
		L.Push(lua.LString(""))
		return 1
	}
	L.Push(lua.LString(val))
	return 1
}

func htmlLen(L *lua.LState) int {
	n := nodeOf(L, 1)
	L.Push(lua.LNumber(n.sel.Length()))
	return 1
}

// htmlEach calls fn(i, sel) for every matched node (1-based i), the
// Lua-side loop primitive for extraction scripts.
func htmlEach(L *lua.LState) int {
	n := nodeOf(L, 1)
	fn := L.CheckFunction(2)
	n.sel.Each(func(i int, s *goquery.Selection) {
		L.Push(fn)
		L.Push(lua.LNumber(i + 1))
		L.Push(pushHTMLNode(L, &htmlNode{sel: s}))
		L.Call(2, 0)
	})
	return 0
}
