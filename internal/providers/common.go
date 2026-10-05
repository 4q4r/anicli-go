package providers

import (
	"encoding/json"
	"sort"
	"strconv"
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

// amdAllDigits/amdSortNumeric are the package's shared numeric-sort
// helpers (PR116: relocated from the migrated animedia.go — anifilm
// and the Lua scripts' numeric ordering share the semantics).
func amdAllDigits(values []string) bool {
	for _, v := range values {
		if _, err := strconv.Atoi(v); err != nil {
			return false
		}
	}
	return len(values) > 0
}

func amdSortNumeric(values []string) {
	keys := make([]int, len(values))
	for i, v := range values {
		n, _ := strconv.Atoi(v)
		keys[i] = n
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// keys lists a string-map's keys sorted (test diagnostics).
func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
