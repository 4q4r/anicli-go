package torrent

// Plain-text tracker-list feeds ([torrent] tracker_lists, PR41): the
// user points the config at URLs like the ngosang/trackerslist feeds
// (one announce URL per line), the engine fetches each list ONCE per
// start through the shared netclient, parses it fail-soft per line and
// merges the result into the EXISTING [torrent] trackers pool — the
// same pool the PR35 health check prunes. No background refetch: lists
// are a startup-time enrichment, not a live subscription.
//
// Failure semantics:
//   - a failed list URL is a typed, logged, visible status — static
//     trackers keep working (degradation, never failure);
//   - a rejected LINE is logged with its reason and skipped;
//   - dedupe is by case-insensitive scheme+host (the PR41 "enough"
//     ruling) against [torrent] trackers and across lists, first
//     occurrence wins.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// trackerListSchemes is the accepted announce-URL scheme set — ALL
// tracker types the engine understands, matching [torrent] trackers
// validation in the config package.
var trackerListSchemes = map[string]bool{
	"udp":   true,
	"http":  true,
	"https": true,
	"ws":    true,
	"wss":   true,
}

// ErrNoNetclientForLists reports a list fetch attempted by an engine
// that has no shared netclient wired (nothing to GET with).
var errNoNetclientForLists = errors.New("engine has no netclient")

// TrackerListFetchError is the typed failure of ONE list URL: the URL
// plus the underlying transport/status error. Surfaces in
// TrackerListStatus.Err so callers can errors.As it.
type TrackerListFetchError struct {
	// URL is the configured list URL that failed.
	URL string
	// Err is the underlying failure.
	Err error
}

// Error implements error.
func (e *TrackerListFetchError) Error() string {
	return fmt.Sprintf("torrent: fetch tracker list %s: %v", e.URL, e.Err)
}

// Unwrap exposes the underlying failure to errors.Is/As.
func (e *TrackerListFetchError) Unwrap() error { return e.Err }

// TrackerListStatus is the per-URL outcome of one tracker-list fetch —
// the fail-loud surface for failed feeds (alongside the log).
type TrackerListStatus struct {
	// URL is the configured list URL.
	URL string
	// OK reports whether the fetch+parse round succeeded.
	OK bool
	// Trackers is the number of accepted lines (before cross-list
	// dedupe).
	Trackers int
	// Skipped is the number of rejected lines (each logged with its
	// reason).
	Skipped int
	// Err carries the typed failure when OK is false.
	Err error
}

// TrackerListSkip is one rejected list line (fail-soft per line: the
// rest of the list still loads).
type TrackerListSkip struct {
	// Line is the 1-based line number in the list body.
	Line int
	// Text is the original line, trimmed.
	Text string
	// Reason explains why the line was rejected.
	Reason string
}

// parseTrackerList splits a plain-text tracker list into announce URLs
// and per-line rejects. One tracker per line; surrounding whitespace is
// trimmed; blank lines and '#' comments (whole-line and inline, i.e. a
// '#' preceded by whitespace) are dropped; a '#' glued to the URL
// (fragment) is preserved. Only udp/http/https/ws/wss URLs with a host
// are accepted — everything else becomes a TrackerListSkip.
func parseTrackerList(body string) (trackers []string, skips []TrackerListSkip) {
	trackers = make([]string, 0, 64)
	for i, raw := range strings.Split(body, "\n") {
		lineNo := i + 1
		line := stripListComment(raw)
		if line == "" {
			continue // blank or comment-only
		}
		u, err := url.Parse(line)
		if err != nil {
			skips = append(skips, TrackerListSkip{Line: lineNo, Text: line, Reason: fmt.Sprintf("parse url: %v", err)})
			continue
		}
		if !trackerListSchemes[u.Scheme] {
			skips = append(skips, TrackerListSkip{Line: lineNo, Text: line,
				Reason: fmt.Sprintf("unsupported scheme %q (want udp, http, https, ws or wss)", u.Scheme)})
			continue
		}
		if u.Host == "" {
			skips = append(skips, TrackerListSkip{Line: lineNo, Text: line, Reason: "missing host"})
			continue
		}
		trackers = append(trackers, line)
	}
	return trackers, skips
}

// stripListComment trims the line and cuts an inline comment: the first
// '#' that starts the line or is preceded by whitespace. A '#' glued to
// the URL (a fragment) is left alone.
func stripListComment(raw string) string {
	line := strings.TrimSpace(raw)
	if strings.HasPrefix(line, "#") {
		return ""
	}
	for i := 1; i < len(line); i++ {
		if line[i] == '#' && (line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

// trackerKey renders the case-insensitive dedupe identity of an
// announce URL: lowered scheme + "://" + lowered host (host includes
// the port). Path is deliberately ignored (the PR41 "enough" ruling).
func trackerKey(tracker string) string {
	u, err := url.Parse(strings.TrimSpace(tracker))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(tracker))
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// TrackerListStatuses returns the latest per-URL fetch outcomes
// (empty until the first fetch ran — the lazy contract). Sorted by URL
// for stable rendering next to TrackerStatuses.
func (e *Engine) TrackerListStatuses() []TrackerListStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TrackerListStatus, len(e.trackerListStatuses))
	copy(out, e.trackerListStatuses)
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

// kickTrackerListFetch runs the one-shot list fetch on a context
// derived from the engine lifecycle (shutdown cancels in-flight GETs),
// mirroring kickTrackerCheck.
func (e *Engine) kickTrackerListFetch() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		<-e.closed
	}()
	go func() {
		defer cancel()
		e.fetchTrackerLists(ctx)
	}()
}

// fetchTrackerLists is the whole PR41 pipeline, run ONCE per engine
// start: one GET per configured list URL through the shared netclient
// (network.proxy_url applies — github is foreign), fail-soft parse per
// line, dedupe into the static pool (case-insensitive scheme+host,
// first occurrence wins), retro-attach the new announce URLs to every
// already-added torrent, then one CheckTrackers round over the merged
// pool so the existing pruning mechanism consumes it. A failed URL is
// a typed, logged status — static trackers keep working.
func (e *Engine) fetchTrackerLists(ctx context.Context) {
	statuses := make([]TrackerListStatus, 0, len(e.cfg.TrackerLists))
	var parsed [][]string
	for _, listURL := range e.cfg.TrackerLists {
		if ctx.Err() != nil {
			return // engine closing: nothing left to merge into
		}
		st := TrackerListStatus{URL: listURL, OK: true}
		trackers, skips, err := e.fetchOneTrackerList(ctx, listURL)
		if err != nil {
			st.OK = false
			st.Err = err
			e.log.Error("torrent: tracker list fetch failed", "url", listURL, "err", err)
		} else {
			st.Trackers = len(trackers)
			st.Skipped = len(skips)
			for _, sk := range skips {
				e.log.Info("torrent: tracker list line skipped",
					"url", listURL, "line", sk.Line, "text", sk.Text, "reason", sk.Reason)
			}
			e.log.Info("torrent: tracker list loaded",
				"url", listURL, "trackers", st.Trackers, "skipped", st.Skipped)
			parsed = append(parsed, trackers)
		}
		statuses = append(statuses, st)
	}
	if ctx.Err() != nil {
		return
	}
	e.mergeTrackerLists(statuses, parsed)
	e.CheckTrackers(ctx)
}

// fetchOneTrackerList GETs one list URL and parses it. The typed
// TrackerListFetchError wraps every transport-level failure.
func (e *Engine) fetchOneTrackerList(ctx context.Context, listURL string) ([]string, []TrackerListSkip, error) {
	if e.net == nil {
		return nil, nil, &TrackerListFetchError{URL: listURL, Err: errNoNetclientForLists}
	}
	resp, err := e.net.Get(ctx, listURL, nil)
	if err != nil {
		return nil, nil, &TrackerListFetchError{URL: listURL, Err: err}
	}
	trackers, skips := parseTrackerList(string(resp.Body))
	return trackers, skips, nil
}

// mergeTrackerLists stores the fetch outcomes and dedupes the parsed
// lists into the pool: against [torrent] trackers first, then across
// lists, case-insensitive scheme+host, first occurrence wins. New
// trackers are retro-attached to every already-added torrent so a
// first add that raced the fetch still gets the feeds.
func (e *Engine) mergeTrackerLists(statuses []TrackerListStatus, parsed [][]string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	seen := make(map[string]bool, len(e.cfg.Trackers))
	for _, tr := range e.cfg.Trackers {
		seen[trackerKey(tr)] = true // static entries always win
	}
	newTrackers := make([]string, 0, 64)
	for _, list := range parsed {
		for _, tr := range list {
			key := trackerKey(tr)
			if seen[key] {
				continue
			}
			seen[key] = true
			newTrackers = append(newTrackers, tr)
		}
	}
	e.listTrackers = newTrackers
	e.trackerListStatuses = statuses

	// Retro-attach (the "with first torrent add" half of the lazy
	// contract): the library dedupes per tier and starts announcing
	// the new URLs immediately. Same lock ordering as addSpec
	// (e.mu → library lock); torrents are never removed from the map.
	if len(newTrackers) > 0 {
		tiers := make([][]string, len(newTrackers))
		for i, tr := range newTrackers {
			tiers[i] = []string{tr}
		}
		for _, t := range e.torrents {
			t.AddTrackers(tiers)
		}
	}
	e.log.Info("torrent: tracker lists merged", "lists", len(statuses), "new", len(newTrackers))
}
