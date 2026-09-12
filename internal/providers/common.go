package providers

import (
	"encoding/json"
	"strings"
)

// normalizeProtocolURL ports anicli-py anicli/providers/_common.py:9-23:
// protocol-relative URLs gain the https scheme; empty stays empty (the
// Python None return).
func normalizeProtocolURL(u string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// safeJSONLoads ports anicli-py anicli/providers/_common.py:26-39 for
// its only consumer (kodik search): a body that fails to decode or is
// not a JSON object reports ok=false, mirroring the Python None return
// and the caller's isinstance(dict) rejection in one step.
func safeJSONLoads(raw []byte) (map[string]any, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}
