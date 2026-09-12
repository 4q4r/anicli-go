package skip

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// animeskipFixture wires an AnimeSkipClient at a capturing httptest
// server.
type animeskipFixture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []graphqlRequest

	// respond serves each request in sequence (last one repeats).
	respond []func(w http.ResponseWriter, r *http.Request)
}

// graphqlRequest captures one GraphQL POST.
type graphqlRequest struct {
	Body     string
	ClientID string
	CT       string
}

func (f *animeskipFixture) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	f.mu.Lock()
	f.reqs = append(f.reqs, graphqlRequest{
		Body:     string(body),
		ClientID: r.Header.Get("X-Client-ID"),
		CT:       r.Header.Get("Content-Type"),
	})
	idx := len(f.reqs) - 1
	f.mu.Unlock()

	serve := f.respond[min(idx, len(f.respond)-1)]
	serve(w, r)
}

func (f *animeskipFixture) requests() []graphqlRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]graphqlRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func newAnimeskipFixture(t *testing.T, respond ...func(w http.ResponseWriter, r *http.Request)) *animeskipFixture {
	t.Helper()
	f := &animeskipFixture{respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *animeskipFixture) client(t *testing.T, opts AnimeSkipOptions) *AnimeSkipClient {
	t.Helper()
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	cfg.RequestTimeout = 5 * time.Second
	net, err := netclient.New(cfg, netclient.WithProvider("animeskip-test"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	if opts.Endpoint == "" {
		opts.Endpoint = f.srv.URL
	}
	return NewAnimeSkipClient(net, opts)
}

// TestAnimeSkipFirstQueryWins pins the primary query shape, headers and
// payload mapping: POST with X-Client-ID, variables {malId,
// episodeNumber}, timestamps extracted recursively with op/ed
// classification.
func TestAnimeSkipFirstQueryWins(t *testing.T) {
	t.Parallel()

	f := newAnimeskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"episodeByMalId":{"timestamps":[
			{"skipType":"OP","startTime":0,"endTime":90.5},
			{"skipType":"ENDING","startTime":1300,"endTime":1400}
		]}}}`))
	})
	c := f.client(t, AnimeSkipOptions{ClientID: "cid-1"})

	intervals, err := c.GetSkipTimes(context.Background(), 21, 2)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1 (first query answered)", len(reqs))
	}
	if reqs[0].ClientID != "cid-1" {
		t.Errorf("X-Client-ID = %q, want cid-1", reqs[0].ClientID)
	}
	if reqs[0].CT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", reqs[0].CT)
	}
	if !strings.Contains(reqs[0].Body, "episodeByMalId") {
		t.Errorf("body %q lacks episodeByMalId query", reqs[0].Body)
	}
	if !strings.Contains(reqs[0].Body, `"malId":21`) {
		t.Errorf("body %q lacks malId variable 21", reqs[0].Body)
	}
	if !strings.Contains(reqs[0].Body, `"episodeNumber":2`) {
		t.Errorf("body %q lacks episodeNumber variable 2", reqs[0].Body)
	}

	want := []SkipInterval{
		{SkipType: "op", StartTime: 0, EndTime: 90.5},
		{SkipType: "ed", StartTime: 1300, EndTime: 1400},
	}
	if len(intervals) != 2 {
		t.Fatalf("intervals = %+v, want 2 entries", intervals)
	}
	for i, iv := range intervals {
		if iv != want[i] {
			t.Errorf("intervals[%d] = %+v, want %+v", i, iv, want[i])
		}
	}
}

// TestAnimeSkipFallsThroughQueryShapes pins the fallback chain: an empty
// first answer retries with findEpisodeByMalId and then episode.
func TestAnimeSkipFallsThroughQueryShapes(t *testing.T) {
	t.Parallel()

	f := newAnimeskipFixture(t,
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"episodeByMalId":null}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"findEpisodeByMalId":{"timestamps":[],"skipTimes":[
				{"type":"intro","from":10,"to":95}
			]}}}`))
		},
	)
	c := f.client(t, AnimeSkipOptions{})

	intervals, err := c.GetSkipTimes(context.Background(), 21, 1)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if !strings.Contains(reqs[1].Body, "findEpisodeByMalId") {
		t.Errorf("second body %q lacks findEpisodeByMalId query", reqs[1].Body)
	}

	if len(intervals) != 1 {
		t.Fatalf("intervals = %+v, want 1", intervals)
	}
	// from/to keys + type "intro" classify as op.
	if intervals[0] != (SkipInterval{SkipType: "op", StartTime: 10, EndTime: 95}) {
		t.Errorf("interval = %+v, want op 10-95", intervals[0])
	}
}

// TestAnimeSkipAllQueriesEmpty pins: every query shape answered cleanly
// with no timestamps -> empty result, no error, all three shapes tried.
func TestAnimeSkipAllQueriesEmpty(t *testing.T) {
	t.Parallel()

	f := newAnimeskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	c := f.client(t, AnimeSkipOptions{})

	intervals, err := c.GetSkipTimes(context.Background(), 21, 1)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 0 {
		t.Errorf("intervals = %+v, want empty", intervals)
	}
	if n := len(f.requests()); n != 3 {
		t.Errorf("requests = %d, want 3 (all query shapes)", n)
	}
}

// TestAnimeSkipUntypedInference pins the start-time heuristic: neutral
// types classify as op before 360s and ed after.
func TestAnimeSkipUntypedInference(t *testing.T) {
	t.Parallel()

	f := newAniskipFixtureForAnimeSkip(t, `{"data":{"episodeByMalId":{"timestamps":[
		{"startTime":40,"endTime":120},
		{"startTime":1290,"endTime":1390}
	]}}}`)
	c := f.client(t, AnimeSkipOptions{})

	intervals, err := c.GetSkipTimes(context.Background(), 21, 1)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 2 {
		t.Fatalf("intervals = %+v, want 2", intervals)
	}
	if intervals[0].SkipType != "op" || intervals[1].SkipType != "ed" {
		t.Errorf("inferred types = %s/%s, want op/ed", intervals[0].SkipType, intervals[1].SkipType)
	}
}

// TestAnimeSkipDedupeAndClamp pins post-collection hygiene: duplicate
// (type, ms) pairs collapse, invalid intervals drop.
func TestAnimeSkipDedupeAndClamp(t *testing.T) {
	t.Parallel()

	f := newAniskipFixtureForAnimeSkip(t, `{"data":{"episodeByMalId":{"timestamps":[
		{"skipType":"op","startTime":0,"endTime":90},
		{"skipType":"op","startTime":0.0,"endTime":90.0},
		{"skipType":"ed","startTime":500,"endTime":500},
		{"skipType":"ed","startTime":600,"endTime":590}
	]}}}`)
	c := f.client(t, AnimeSkipOptions{})

	intervals, err := c.GetSkipTimes(context.Background(), 21, 1)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 1 {
		t.Fatalf("intervals = %+v, want only the deduped op", intervals)
	}
	if intervals[0] != (SkipInterval{SkipType: "op", StartTime: 0, EndTime: 90}) {
		t.Errorf("interval = %+v, want op 0-90", intervals[0])
	}
}

// TestAnimeSkipTransportErrorsPropagate pins: when every query shape
// fails on the transport/HTTP layer the error surfaces (python swallowed
// it; the Go manager needs the distinction).
func TestAnimeSkipTransportErrorsPropagate(t *testing.T) {
	t.Parallel()

	f := newAnimeskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	c := f.client(t, AnimeSkipOptions{})

	if _, err := c.GetSkipTimes(context.Background(), 21, 1); err == nil {
		t.Fatal("GetSkipTimes on 403 returned nil error, want error")
	}
}

// TestAnimeSkipInvalidJSONFailsLoud pins: a non-JSON payload is an
// error, not a silent empty result.
func TestAnimeSkipInvalidJSONFailsLoud(t *testing.T) {
	t.Parallel()

	f := newAnimeskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>gateway error</html>`))
	})
	c := f.client(t, AnimeSkipOptions{})

	if _, err := c.GetSkipTimes(context.Background(), 21, 1); err == nil {
		t.Fatal("GetSkipTimes on HTML returned nil error, want error")
	}
}

// newAniskipFixtureForAnimeSkip builds a single-response fixture.
func newAniskipFixtureForAnimeSkip(t *testing.T, payload string) *animeskipFixture {
	t.Helper()
	return newAnimeskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	})
}
