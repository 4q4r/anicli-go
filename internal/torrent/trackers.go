package torrent

// User-specified tracker health checking ([torrent] trackers): the
// engine probes every configured announce URL with bounded
// concurrency and a per-tracker timeout, keeps the responsive ones
// and logs the dead ones with their reason («отфасовать нерабочие»).
// Recheck happens on demand via CheckTrackers; new torrents get the
// last known healthy set.
//
// Honest per-scheme coverage:
//   - http/https: one GET against the announce URL. ANY HTTP response
//     proves liveness (real trackers answer a bencode error for a
//     bogus hash — still alive).
//   - udp: the full BEP 15 connect handshake (magic, action 0, txid
//     echo) — a real protocol check.
//   - ws/wss: TCP/TLS connect to the host only. A full websocket
//     handshake would need a websocket client dependency; the proxy
//     is not applied to this probe (library limitation, see
//     [torrent] proxy docs).

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// TrackerHealth is one probe result.
type TrackerHealth struct {
	// URL is the announce URL as configured.
	URL string
	// Alive is the probe verdict.
	Alive bool
	// Reason carries the transport failure for dead trackers.
	Reason string
	// CheckedAt is when the probe finished.
	CheckedAt time.Time
}

// maxTrackerProbes bounds the concurrent health checks.
const maxTrackerProbes = 8

// trackerProbeTimeout bounds ONE probe (network dial + response).
const trackerProbeTimeout = 5 * time.Second

// probeTracker health-checks one announce URL. proxyURL routes
// http(s) probes through the user proxy (udp/ws probes stay direct —
// stdlib socks5 is CONNECT-only and ws checks are tcp-level).
func probeTracker(ctx context.Context, raw string, proxyURL *url.URL, timeout time.Duration) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if proxyURL != nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			return fmt.Errorf("no http response: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		return nil
	case "udp":
		host := u.Host // host:port, the announce path is not used by BEP 15
		var d net.Dialer
		conn, err := d.DialContext(ctx, "udp", host)
		if err != nil {
			return fmt.Errorf("dial udp: %w", err)
		}
		defer func() { _ = conn.Close() }()
		deadline, _ := ctx.Deadline()
		_ = conn.SetDeadline(deadline)
		return udpConnectHandshake(ctx, conn)
	case "ws", "wss":
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return fmt.Errorf("tcp connect: %w", err)
		}
		_ = conn.Close()
		return nil
	default:
		return fmt.Errorf("unsupported tracker scheme %q", u.Scheme)
	}
}

// udpConnectHandshake performs the BEP 15 connect request/response.
func udpConnectHandshake(ctx context.Context, conn net.Conn) error {
	var txid [4]byte
	if _, err := rand.Read(txid[:]); err != nil {
		return fmt.Errorf("txid: %w", err)
	}
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], 0x41727101980) // protocol magic
	binary.BigEndian.PutUint32(req[8:12], 0)            // action: connect
	copy(req[12:16], txid[:])
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}
	resp := make([]byte, 8)
	if _, err := readFull(ctx, conn, resp); err != nil {
		return fmt.Errorf("no connect response: %w", err)
	}
	if binary.BigEndian.Uint32(resp[0:4]) != 0 || binary.BigEndian.Uint32(resp[4:8]) != binary.BigEndian.Uint32(txid[:]) {
		return fmt.Errorf("malformed connect response")
	}
	return nil
}

// readFull reads exactly len(buf) bytes, honouring ctx cancellation.
func readFull(ctx context.Context, conn net.Conn, buf []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		total := 0
		for total < len(buf) {
			n, err := conn.Read(buf[total:])
			total += n
			if err != nil {
				done <- result{total, err}
				return
			}
		}
		done <- result{total, nil}
	}()
	select {
	case r := <-done:
		return r.n, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// CheckTrackers probes the whole pool — static [torrent] trackers plus
// the merged tracker_lists additions — with bounded concurrency,
// prunes the dead ones from future torrents (logging each with its
// reason) and returns the full report.
func (e *Engine) CheckTrackers(ctx context.Context) []TrackerHealth {
	e.mu.Lock()
	all := e.trackerPoolLocked()
	proxyRaw := e.cfg.Proxy
	e.mu.Unlock()
	if len(all) == 0 {
		return nil
	}
	var proxyURL *url.URL
	if proxyRaw != "" {
		proxyURL, _ = url.Parse(proxyRaw)
	}

	statuses := make([]TrackerHealth, len(all))
	sem := make(chan struct{}, maxTrackerProbes)
	var wg sync.WaitGroup
	for i, tr := range all {
		wg.Add(1)
		go func(i int, tr string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := probeTracker(ctx, tr, proxyURL, e.probeTimeout())
			st := TrackerHealth{URL: tr, Alive: err == nil, CheckedAt: time.Now().UTC()}
			if err != nil {
				st.Reason = err.Error()
			}
			statuses[i] = st
		}(i, tr)
	}
	wg.Wait()

	e.mu.Lock()
	for _, st := range statuses {
		prev, had := e.trackerHealth[st.URL]
		e.trackerHealth[st.URL] = st // the latest verdict wins
		if had && prev.Alive != st.Alive {
			if st.Alive {
				e.log.Info("torrent: tracker back alive", "url", st.URL)
			} else {
				e.log.Info("torrent: tracker pruned", "url", st.URL, "reason", st.Reason)
			}
		} else if !had && !st.Alive {
			e.log.Info("torrent: tracker pruned", "url", st.URL, "reason", st.Reason)
		}
	}
	e.mu.Unlock()
	return statuses
}

// TrackerStatuses returns the latest probe results.
func (e *Engine) TrackerStatuses() []TrackerHealth {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TrackerHealth, 0, len(e.trackerHealth))
	for _, st := range e.trackerHealth {
		out = append(out, st)
	}
	sortTrackers(out)
	return out
}

// sortTrackers orders statuses by URL (stable reports).
func sortTrackers(sts []TrackerHealth) {
	sort.Slice(sts, func(i, j int) bool { return sts[i].URL < sts[j].URL })
}

// trackerPoolLocked renders the full tracker pool to probe and inject:
// static [torrent] trackers first, then the merged [torrent]
// tracker_lists additions (deduped at merge time). Callers hold mu.
func (e *Engine) trackerPoolLocked() []string {
	all := make([]string, 0, len(e.cfg.Trackers)+len(e.listTrackers))
	all = append(all, e.cfg.Trackers...)
	all = append(all, e.listTrackers...)
	return all
}

// trackerTiersLocked renders the pool as TorrentSpec tiers for new
// torrents: every alive tracker (or ALL of them before the first check
// finished — fail-open, the library tolerates dead announce URLs
// anyway). Callers hold mu.
func (e *Engine) trackerTiersLocked() [][]string {
	all := e.trackerPoolLocked()
	tiers := make([][]string, 0, len(all))
	checked := len(e.trackerHealth) > 0
	for _, tr := range all {
		if checked {
			if st, ok := e.trackerHealth[tr]; ok && !st.Alive {
				continue
			}
		}
		tiers = append(tiers, []string{tr})
	}
	return tiers
}
