package tui

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// SourceEpisodes pairs one source's episode list with its provider id
// for the session merge.
type SourceEpisodes struct {
	SourceID string
	Episodes []contracts.Episode
}

// hasCyrillic matches the Russian alphabet (python has_cyrillic).
var hasCyrillic = regexp.MustCompile(`[а-яА-Я]`)

// MergeEpisodeLists merges per-source episode lists into one map keyed
// by episode number (python session_loop merged_episodes_map port):
// the first source providing a number wins the slot; later sources
// append their embeds under "[provider] dub" keys and compose the raw
// id as "prov1:id1|prov2:id2". The returned order sorts numerically;
// unparseable labels (OVA etc.) key 0.0 and therefore sort FIRST —
// python bug-compatibility (see group_test).
func MergeEpisodeLists(sources []SourceEpisodes) (map[string]contracts.Episode, []string) {
	merged := make(map[string]contracts.Episode)
	for _, src := range sources {
		for _, ep := range src.Episodes {
			if existing, ok := merged[ep.Num]; ok {
				for dub, links := range ep.RawEmbeds {
					existing.RawEmbeds["["+src.SourceID+"] "+dub] = links
				}
				if !strings.Contains(existing.RawID, src.SourceID+":") {
					existing.RawID += "|" + src.SourceID + ":" + ep.RawID
				}
				merged[ep.Num] = existing
				continue
			}
			merged[ep.Num] = contracts.Episode{
				Num:       ep.Num,
				Title:     ep.Title,
				RawID:     src.SourceID + ":" + ep.RawID,
				RawEmbeds: prefixEmbeds(src.SourceID, ep.RawEmbeds),
			}
		}
	}
	order := make([]string, 0, len(merged))
	for num := range merged {
		order = append(order, num)
	}
	// Decorate-sort-undecorate (PR82 P1#4): the sort key is parsed ONCE
	// per entry instead of twice per comparison (~22K ParseFloat calls
	// at the 1178-episode scale). Same key function, same stable sort,
	// same (key, label) tie-break — ordering is identical.
	type keyedNum struct {
		num string
		key float64
	}
	keyed := make([]keyedNum, 0, len(order))
	for _, num := range order {
		keyed = append(keyed, keyedNum{num: num, key: EpisodeSortKey(num)})
	}
	sort.SliceStable(keyed, func(i, j int) bool {
		if keyed[i].key != keyed[j].key {
			return keyed[i].key < keyed[j].key
		}
		return keyed[i].num < keyed[j].num
	})
	for i, k := range keyed {
		order[i] = k.num
	}
	return merged, order
}

// prefixEmbeds namespaces embed keys under "[provider] dub".
func prefixEmbeds(sourceID string, embeds map[string][]string) map[string][]string {
	out := make(map[string][]string, len(embeds))
	for dub, links := range embeds {
		out["["+sourceID+"] "+dub] = links
	}
	return out
}

// EpisodeSortKey parses an episode label; unparseable labels key 0.0
// and so sort first in the callers' ascending orders (python parity).
func EpisodeSortKey(num string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0
	}
	return v
}

// DubStats counts embed-key occurrences across episodes (python
// dub_stats Counter port) — the "[N сер.]" badges of the dub pickers.
func DubStats(episodes []contracts.Episode) map[string]int {
	stats := make(map[string]int)
	for _, ep := range episodes {
		for key := range ep.RawEmbeds {
			stats[key]++
		}
	}
	return stats
}

// FindExactGroup implements rehydrate strategy 1 (python
// _rehydrate_group): the group holding the exact (source_id, url)
// binding of the stored record.
func FindExactGroup(groups [][]contracts.SearchResult, sourceID, url string) []contracts.SearchResult {
	for _, group := range groups {
		for _, res := range group {
			if res.SourceID == sourceID && res.URL == url {
				return group
			}
		}
	}
	return nil
}

// FindBestSimilarGroup implements rehydrate strategy 2 (python
// _find_best_similar_group): among groups, the one whose longest title
// best matches the canonical title wins, provided the ratio clears
// minRatio. The matcher is the CPython SequenceMatcher ratio ported
// for allanime (bug-compatible, goldens in PR6).
func FindBestSimilarGroup(groups [][]contracts.SearchResult, canonicalTitle string, minRatio float64) []contracts.SearchResult {
	target := strings.ToLower(canonicalTitle)
	var best []contracts.SearchResult
	bestRatio := 0.0
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		longest := group[0].Title
		for _, res := range group[1:] {
			if len(res.Title) > len(longest) {
				longest = res.Title
			}
		}
		ratio := providers.SimilarityRatio(target, strings.ToLower(longest))
		if ratio > bestRatio {
			bestRatio = ratio
			best = group
		}
	}
	if best == nil || bestRatio <= minRatio {
		return nil
	}
	return best
}

// BestDisplayTitle picks a group's display title: the longest
// Cyrillic title, else the longest title (python
// _get_best_display_title port).
func BestDisplayTitle(group []contracts.SearchResult) string {
	if len(group) == 0 {
		return "Unknown"
	}
	var cyrillic []string
	for _, r := range group {
		if hasCyrillic.MatchString(r.Title) {
			cyrillic = append(cyrillic, r.Title)
		}
	}
	pool := cyrillic
	if len(pool) == 0 {
		pool = make([]string, 0, len(group))
		for _, r := range group {
			pool = append(pool, r.Title)
		}
	}
	longest := pool[0]
	for _, t := range pool[1:] {
		if len([]rune(t)) > len([]rune(longest)) {
			longest = t
		}
	}
	return longest
}

// ParseRange parses "1-5, 7, 10-12" into a sorted unique episode list
// (python _parse_range port): malformed tokens are skipped, ranges are
// inclusive.
func ParseRange(spec string) []int {
	seen := make(map[int]struct{})
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if start, end, ok := strings.Cut(part, "-"); ok {
			lo, errLo := strconv.Atoi(strings.TrimSpace(start))
			hi, errHi := strconv.Atoi(strings.TrimSpace(end))
			if errLo != nil || errHi != nil || lo > hi {
				continue
			}
			for i := lo; i <= hi; i++ {
				seen[i] = struct{}{}
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			seen[n] = struct{}{}
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
