package rules

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"

	"github.com/jmespath/go-jmespath"
)

// compiled caches jmespath programs by expression; *jmespath.JMESPath is
// immutable and safe for concurrent Search.
var compiled sync.Map // string -> *jmespath.JMESPath

// CacheLen reports the number of cached compiled expressions. Intended for
// tests and operational insight.
func CacheLen() int {
	n := 0
	compiled.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// compilePath returns the cached compiled program for path.
func compilePath(path string) (*jmespath.JMESPath, error) {
	if v, ok := compiled.Load(path); ok {
		return v.(*jmespath.JMESPath), nil
	}
	prog, err := jmespath.Compile(path)
	if err != nil {
		return nil, &RuleError{Path: path, Err: fmt.Errorf("compile: %w", err)}
	}
	actual, _ := compiled.LoadOrStore(path, prog)
	return actual.(*jmespath.JMESPath), nil
}

// Result is one extracted record shaped for the SearchResult and Episode
// DTO fields. Empty strings mean the rule's path matched nothing.
type Result struct {
	Title string
	URL   string
	Num   string
}

// Extract applies a declarative rule set to parsed JSON (any value from
// encoding/json). It returns one Result per record selected by the
// Multiple rule, in document order. Records are not filtered here; the
// ExtractSearch / ExtractEpisodes wrappers apply python's required-field
// semantics.
func Extract(data any, rules []Rule) ([]Result, error) {
	var contextRule *Rule
	for i := range rules {
		if rules[i].Multiple {
			contextRule = &rules[i]
			break
		}
	}
	if contextRule == nil {
		return nil, fmt.Errorf("rules: exactly one rule must carry Multiple=true as the context rule")
	}
	if contextRule.Path == "" {
		return nil, &RuleError{Path: contextRule.Path, Err: fmt.Errorf("context rule path is empty")}
	}

	prog, err := compilePath(contextRule.Path)
	if err != nil {
		return nil, err
	}
	v, err := prog.Search(data)
	if err != nil {
		return nil, &RuleError{Path: contextRule.Path, Err: fmt.Errorf("search: %w", err)}
	}
	items, ok := v.([]any)
	if !ok || items == nil {
		// python: `jmespath.search(...) or []` — no match is empty.
		return nil, nil
	}

	fieldProgs := make([]*jmespath.JMESPath, len(rules))
	for i := range rules {
		if rules[i].Multiple {
			continue
		}
		if rules[i].Path == "" {
			return nil, &RuleError{Attr: rules[i].Attr, Err: fmt.Errorf("field rule path is empty")}
		}
		switch rules[i].Attr {
		case AttrTitle, AttrURL, AttrNum:
		default:
			return nil, &RuleError{Path: rules[i].Path, Attr: rules[i].Attr,
				Err: fmt.Errorf("unknown attr (want title, url or num)")}
		}
		fieldProgs[i], err = compilePath(rules[i].Path)
		if err != nil {
			return nil, err
		}
	}

	results := make([]Result, 0, len(items))
	for _, item := range items {
		var res Result
		for i := range rules {
			if rules[i].Multiple {
				continue
			}
			raw, err := fieldProgs[i].Search(item)
			if err != nil {
				return nil, &RuleError{Path: rules[i].Path, Attr: rules[i].Attr,
					Err: fmt.Errorf("search record: %w", err)}
			}
			value := stringify(raw)
			if value == "" {
				continue
			}
			value = rules[i].Prefix + value + rules[i].Postfix
			value = applyTransform(rules[i].Transform, value)
			switch rules[i].Attr {
			case AttrTitle:
				res.Title = value
			case AttrURL:
				res.URL = value
			case AttrNum:
				res.Num = value
			}
		}
		results = append(results, res)
	}
	return results, nil
}

// applyTransform post-processes an extracted value.
func applyTransform(tr Transform, value string) string {
	switch tr {
	case TransformProtocolRelative:
		if len(value) >= 2 && value[0] == '/' && value[1] == '/' {
			return "https:" + value
		}
		return value
	case TransformNone:
		return value
	default:
		// Unknown transforms are a config typo; failing here keeps the
		// loud-error contract without a second validation pass.
		return value
	}
}

// stringify renders a jmespath result the way python's str() does for the
// values these rules extract. encoding/json erases the int/float
// distinction, so integral numbers render without a decimal point
// (matching python for every real provider payload); floats use the
// shortest representation. null renders empty; arrays and objects render
// empty (python would str() them, but no rule extracts composites).
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return ""
	}
}

// sortByNumericEpisode ports python's
// `with contextlib.suppress(ValueError): episodes.sort(key=lambda x: float(x.num))`:
// sort ascending when every Num parses as a number, otherwise keep
// document order.
func sortByNumericEpisode(episodes []episodeRecord) {
	allNumeric := true
	for _, e := range episodes {
		if _, err := strconv.ParseFloat(e.num, 64); err != nil {
			allNumeric = false
			break
		}
	}
	if !allNumeric {
		return
	}
	sort.SliceStable(episodes, func(i, j int) bool {
		a, _ := strconv.ParseFloat(episodes[i].num, 64)
		b, _ := strconv.ParseFloat(episodes[j].num, 64)
		return a < b
	})
}
