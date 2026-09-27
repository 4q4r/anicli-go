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
func (e *Engine) sdkHTMLParse(ls *lua.LState) int {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(ls.CheckString(1)))
	if err != nil {
		ls.RaiseError("html.parse: %v", err)
		return 0
	}
	ls.Push(pushHTMLNode(ls, &htmlNode{sel: doc.Selection}))
	return 1
}

// pushHTMLNode wraps a selection as a userdata sharing one metatable
// shape for documents and nested selections.
func pushHTMLNode(ls *lua.LState, n *htmlNode) *lua.LUserData {
	ud := ls.NewUserData()
	ud.Value = n
	mt := ls.NewTable()
	ls.SetFuncs(mt, map[string]lua.LGFunction{
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
func nodeOf(ls *lua.LState, i int) *htmlNode {
	ud := ls.CheckUserData(i)
	n, ok := ud.Value.(*htmlNode)
	if !ok {
		ls.RaiseError("html method called on a non-selection value")
		return nil
	}
	return n
}

func htmlFind(ls *lua.LState) int {
	n := nodeOf(ls, 1)
	sel := n.sel.Find(ls.CheckString(2))
	ls.Push(pushHTMLNode(ls, &htmlNode{sel: sel}))
	return 1
}

func htmlText(ls *lua.LState) int {
	n := nodeOf(ls, 1)
	// First-node semantics: a multi-match selection concatenates every
	// node's text, which is never what an extraction script wants.
	ls.Push(lua.LString(strings.TrimSpace(n.sel.Eq(0).Text())))
	return 1
}

func htmlAttr(ls *lua.LState) int {
	n := nodeOf(ls, 1)
	val, ok := n.sel.Attr(ls.CheckString(2))
	if !ok {
		ls.Push(lua.LString(""))
		return 1
	}
	ls.Push(lua.LString(val))
	return 1
}

func htmlLen(ls *lua.LState) int {
	n := nodeOf(ls, 1)
	ls.Push(lua.LNumber(n.sel.Length()))
	return 1
}

// htmlEach calls fn(i, sel) for every matched node (1-based i), the
// Lua-side loop primitive for extraction scripts.
func htmlEach(ls *lua.LState) int {
	n := nodeOf(ls, 1)
	fn := ls.CheckFunction(2)
	n.sel.Each(func(i int, s *goquery.Selection) {
		ls.Push(fn)
		ls.Push(lua.LNumber(i + 1))
		ls.Push(pushHTMLNode(ls, &htmlNode{sel: s}))
		ls.Call(2, 0)
	})
	return 0
}
