// Package rules is the declarative jmespath extraction engine, ported from
// the frozen Python original (anicli-py anicli/core/parser.py together with
// the rule blocks of sources.toml).
//
// Python's per-source rule fields (search_path / search_title / search_link
// with link_prefix / link_postfix, episode_context_path / episode_num /
// episode_link) map onto the generalized Rule model below: the rule with
// Multiple set is the context iterator (python's *_path over the whole
// document); the remaining rules select fields inside each record.
// ExtractSearch / ExtractEpisodes reproduce the python post-processing
// (required-field filtering, URL assembly, protocol-relative fix and
// best-effort numeric episode sort).
package rules

import "fmt"

// Attr names the output slot a rule fills.
type Attr string

const (
	// AttrTitle fills Result.Title (python search_title).
	AttrTitle Attr = "title"
	// AttrURL fills Result.URL (python search_link / episode_link).
	AttrURL Attr = "url"
	// AttrNum fills Result.Num (python episode_num).
	AttrNum Attr = "num"
)

// Transform names a post-extraction transform applied to a field value.
type Transform string

const (
	// TransformNone leaves the extracted string as-is.
	TransformNone Transform = ""
	// TransformProtocolRelative prefixes "//..." URLs with "https:"
	// (python: `if str(url).startswith("//"): url = "https:" + url`).
	TransformProtocolRelative Transform = "protocol_relative"
)

// Rule is one declarative extraction rule. Exactly one rule in a set must
// carry Multiple; its Path selects the record list, every other rule is
// evaluated against each record.
type Rule struct {
	// Path is the jmespath expression (compiled once and cached).
	Path string
	// Attr is the output slot; required on non-multiple rules.
	Attr Attr
	// Multiple marks the context/iterator rule.
	Multiple bool
	// Transform post-processes the extracted value.
	Transform Transform
	// Prefix and Postfix assemble URLs: value = Prefix + value + Postfix
	// (python base_url + link_prefix + link + link_postfix). Applied
	// before Transform and only to non-empty values.
	Prefix  string
	Postfix string
}

// Validate rejects construction-time configuration errors: an unknown
// Transform is a sources.toml typo and must fail loud, not pass through.
func (r Rule) Validate() error {
	switch r.Transform {
	case TransformNone, TransformProtocolRelative:
		return nil
	default:
		return &RuleError{
			Path: r.Path, Attr: r.Attr,
			Err: fmt.Errorf("unknown transform %q (want %q or %q)",
				r.Transform, TransformNone, TransformProtocolRelative),
		}
	}
}

// RuleError annotates a rule failure with the rule's context so mispaired
// rules and typoed paths surface with their origin.
type RuleError struct {
	// Path is the jmespath expression that failed.
	Path string
	// Attr of the failing rule (empty for the multiple/context rule).
	Attr Attr
	// Err is the underlying cause.
	Err error
}

// Error implements error.
func (e *RuleError) Error() string {
	if e.Attr != "" {
		return fmt.Sprintf("rule %q (attr %q): %v", e.Path, e.Attr, e.Err)
	}
	return fmt.Sprintf("rule %q: %v", e.Path, e.Err)
}

// Unwrap exposes the cause for errors.Is / errors.As.
func (e *RuleError) Unwrap() error { return e.Err }
