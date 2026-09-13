package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/an0nx/anicli-go/internal/storage"
)

// Shared coercion/normalization helpers for the home/library/card
// builders (ports of the python _coerce_*/_normalize_space/
// _parse_shiki_datetime/_anime_* helpers).

// localViewOfRow projects a storage row into the builder view.
func localViewOfRow(row *storage.AnimeProgress) localRowView {
	view := localRowView{
		Title:     row.Title,
		UpdatedAt: row.UpdatedAt,
		HistoryID: row.ID,
		Has:       true,
	}
	if row.Poster != nil {
		view.Poster = *row.Poster
	}
	if ep, err := strconv.Atoi(strings.TrimSpace(row.CurrentEpisode)); err == nil {
		view.CurrentEpisode = ep
	}
	return view
}

// localOrNil returns a nil-safe pointer for map lookups.
func localOrNil(m map[int64]localRowView, id int64) *localRowView {
	if v, ok := m[id]; ok {
		return &v
	}
	return nil
}

// parseShikiDatetime parses iso-like shikimori timestamps (python
// _parse_shiki_datetime: "Z" suffix normalized, naive values assumed
// UTC).
func parseShikiDatetime(value string) (time.Time, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return time.Time{}, false
	}
	normalized := strings.Replace(v, "Z", "+00:00", 1)
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, normalized); err == nil {
			if t.Location() == nil {
				return t.UTC(), true
			}
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// stringOrEmpty renders a string-ish value ("" when nil or non-string).
func stringOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return strings.Join(strings.Fields(s), " ")
	}
	return ""
}

// nonEmptyOrNil maps "" to nil for nullable card fields.
func nonEmptyOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// coerceFloat converts score-like values (python _coerce_float).
func coerceFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		normalized := strings.Replace(strings.TrimSpace(t), ",", ".", 1)
		if normalized == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(normalized, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// intOf coerces an int-ish value (0 when absent).
func intOf(v any) int {
	if n, ok := coerceID(v); ok {
		return int(n)
	}
	return 0
}

// intOrNil maps numeric values onto the nullable card fields.
func intOrNil(v any) any {
	if n, ok := coerceID(v); ok {
		return n
	}
	return nil
}

// yearOrNil maps a *int year onto the card shape.
func yearOrNil(y *int) any {
	if y == nil {
		return nil
	}
	return *y
}

// animeYear coerces year or aired_on (python _coerce_int over year or
// aired_on).
func animeYear(animeRow map[string]any) any {
	if v := intOrNil(animeRow["year"]); v != nil {
		return v
	}
	aired := stringOrEmpty(animeRow["aired_on"])
	if aired == "" {
		return nil
	}
	digits := firstDigits(aired)
	if digits == "" {
		return nil
	}
	if y, err := strconv.ParseInt(digits, 10, 64); err == nil {
		return y
	}
	return nil
}

// animeTitles resolves ru/en titles with the local fallback (python
// _anime_titles).
func animeTitles(animeRow map[string]any, local *localRowView) (any, any) {
	titleRu := nonEmptyOrNil(stringOrEmpty(animeRow["russian"]))
	titleEn := nonEmptyOrNil(stringOrEmpty(animeRow["name"]))
	if titleRu != nil || titleEn != nil {
		return titleRu, titleEn
	}
	if local != nil && strings.TrimSpace(local.Title) != "" {
		v := strings.Join(strings.Fields(local.Title), " ")
		return v, v
	}
	return nil, nil
}

// animePoster resolves the poster URL: image ladder, then row poster
// field, then the local row poster, then the placeholder (python
// _anime_poster).
func animePoster(animeRow map[string]any, local *localRowView, baseURL string) string {
	if img, ok := animeRow["image"].(map[string]any); ok {
		for _, key := range []string{"original", "main", "preview", "x96", "x48"} {
			if v := stringOrEmpty(img[key]); v != "" {
				return resolveAgainstBase(baseURL, v)
			}
		}
	}
	if poster := resolveAgainstBase(baseURL, stringOrEmpty(animeRow["poster"])); poster != "" {
		return poster
	}
	if local != nil && local.Poster != "" {
		return local.Poster
	}
	return placeholderPoster
}

// genreNames extracts deduplicated genre names, russian preferred
// (python _genre_names).
func genreNames(v any) []any {
	items, ok := v.([]any)
	if !ok {
		return []any{}
	}
	seen := map[string]bool{}
	out := []any{}
	for _, item := range items {
		var title string
		switch t := item.(type) {
		case string:
			title = strings.Join(strings.Fields(t), " ")
		case map[string]any:
			for _, key := range []string{"russian", "name", "kind", "title"} {
				if s := stringOrEmpty(t[key]); s != "" {
					title = s
					break
				}
			}
		}
		if title == "" || seen[title] {
			continue
		}
		seen[title] = true
		out = append(out, title)
	}
	return out
}

// resolveAgainstBase normalizes site-relative URLs (shikimori
// resolveShikimoriURL semantics at the api layer).
func resolveAgainstBase(baseURL, raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(cleaned, "//"):
		return "https:" + cleaned
	case strings.HasPrefix(cleaned, "/"):
		return strings.TrimRight(baseURL, "/") + cleaned
	case strings.HasPrefix(cleaned, "http://"), strings.HasPrefix(cleaned, "https://"):
		return cleaned
	default:
		return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(cleaned, "/")
	}
}

// sortedUniqueIDs sorts and dedups ids (python sorted(set(ids))).
func sortedUniqueIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	// insertion-free path for the common already-sorted case
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// maxInt returns the largest argument.
func maxInt(values ...int) int {
	best := values[0]
	for _, v := range values[1:] {
		if v > best {
			best = v
		}
	}
	return best
}

// parseInt64 parses a decimal id.
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}

// int64ToString renders a decimal id.
func int64ToString(v int64) string {
	return strconv.FormatInt(v, 10)
}

// trimScore renders a one-decimal score label (python f"{score:.1f}").
func trimScore(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}
