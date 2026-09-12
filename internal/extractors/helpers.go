package extractors

import "strings"

// normalizeProtocolRelative ports the `url.startswith("//")` fixup the
// Python extractors apply (extractors.py:41-42, 69-70, 333-335).
func normalizeProtocolRelative(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// firstNonEmptyStr ports Python's `a or b` string fallback.
func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// digitsOnly ports Python's re.sub(r"\D", "", s).
func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
