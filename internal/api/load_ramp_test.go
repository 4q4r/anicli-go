//go:build load

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ramp profile: concurrency climbs 1→2→4→8→16→32→64 virtual users,
// each stage sustained for a fixed window against the REAL chi server
// (login once per VU, then a read journey: me → home/feed → episodes).
// Reports req/s and p50/p95/p99 per stage; the error rate must stay at
// exactly 0 — the ramp is a regression net for connection handling,
// sqlite serialization and auth under load.

const (
	rampStages      = 7 // 1,2,4,8,16,32,64
	rampStageWindow = 4300 * time.Millisecond
	rampStageTotal  = 30 * time.Second // ~sum of stage windows
)

// rampJourney runs read calls until the stage window closes; every
// failure is recorded (the ramp tolerates none).
func rampJourney(client *http.Client, baseURL, token string, deadline time.Time, stats *loadStats) {
	for time.Now().Before(deadline) {
		call := func(ep, method, p string) {
			start := time.Now()
			req, err := http.NewRequest(method, baseURL+p, nil)
			if err != nil {
				stats.record(ep, 0, true)
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.Do(req)
			failed := err != nil
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				failed = resp.StatusCode != http.StatusOK
			}
			stats.record(ep, time.Since(start), failed)
		}
		call("auth/me", http.MethodGet, "/api/v1/auth/me")
		call("home/feed", http.MethodGet, "/api/v1/home/feed")
		call("episodes", http.MethodGet, "/api/v1/episodes?source_id=load&source_url=https%3A%2F%2Fload.example%2Fanime%2Framp")
	}
}

// TestLoadAPIRamp proves the server survives and stays fast while
// concurrency grows 64×; error rate must be 0 across every stage.
func TestLoadAPIRamp(t *testing.T) {
	app := newLoadApp(t)
	t.Cleanup(app.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	baseURL := "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(ctx, ln) }()

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 128,
			MaxConnsPerHost:     0,
		},
	}

	// One shared login per VU count is wasteful; a single token serves
	// the whole ramp (auth/me revalidates it on every call).
	token := rampLogin(t, client, baseURL)

	fmt.Fprintf(os.Stdout, "\n=== API ramp results (stages 1→64 VUs, %s window each) ===\n", rampStageWindow.Round(time.Millisecond))
	fmt.Fprintln(os.Stdout, "vus\treq/s\tp50\tp95\tp99\terrors")

	var sloBroken bool
	for stage := range rampStages {
		vus := 1 << stage
		stats := newLoadStats()
		deadline := time.Now().Add(rampStageWindow)

		var wg sync.WaitGroup
		start := time.Now()
		for range vus {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rampJourney(client, baseURL, token, deadline, stats)
			}()
		}
		wg.Wait()
		wall := time.Since(start)

		calls, failures := stats.total()
		if failures != 0 {
			sloBroken = true
			t.Errorf("stage %d VUs: %d failed requests (want 0)", vus, failures)
		}

		// Aggregate percentiles across the three read endpoints.
		var all []time.Duration
		for _, values := range stats.latency {
			all = append(all, values...)
		}
		sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
		at := func(p float64) time.Duration { return all[int(float64(len(all)-1)*p)] }

		rps := float64(calls) / wall.Seconds()
		fmt.Fprintf(os.Stdout, "%d\t%.0f\t%s\t%s\t%s\t%d\n",
			vus, rps, at(0.50).Round(time.Microsecond), at(0.95).Round(time.Microsecond),
			at(0.99).Round(time.Microsecond), failures)
	}

	cancel()
	if err := <-serveDone; err != nil {
		t.Fatalf("server exit: %v", err)
	}
	if sloBroken {
		t.Fatal("ramp SLO violated (zero errors required)")
	}
}

// rampLogin performs the single ramp-wide login.
func rampLogin(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/auth/login",
		strings.NewReader(fmt.Sprintf(`{"login": %q, "password": %q}`, loadLogin, loadPassword)))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || payload.AccessToken == "" {
		t.Fatalf("login decode: %v", err)
	}
	return payload.AccessToken
}
