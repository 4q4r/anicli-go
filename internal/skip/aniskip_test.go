package skip

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// aniskipFixture wires an AniSkipClient at a capturing httptest server.
type aniskipFixture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest

	// respond serves the request; nil means 500.
	respond func(w http.ResponseWriter, r *http.Request)
}

// capturedRequest records one incoming request for assertions.
type capturedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func (f *aniskipFixture) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	f.mu.Lock()
	f.reqs = append(f.reqs, capturedRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body),
	})
	f.mu.Unlock()
	if f.respond == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.respond(w, r)
}

func (f *aniskipFixture) requests() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

func newAniskipFixture(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *aniskipFixture {
	t.Helper()
	f := &aniskipFixture{respond: respond}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(f.srv.Close)
	return f
}

// client builds the client under test pointing at the fixture server.
func (f *aniskipFixture) client(t *testing.T, opts AniSkipOptions) *AniSkipClient {
	t.Helper()
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	cfg.RequestTimeout = 5 * time.Second
	net, err := netclient.New(cfg, netclient.WithProvider("aniskip-test"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	if opts.BaseURL == "" {
		opts.BaseURL = f.srv.URL
	}
	return NewAniSkipClient(net, opts)
}

// serveSkipTimes writes a v2 skip-times payload.
func serveSkipTimes(w http.ResponseWriter, found bool, results string) {
	w.Header().Set("Content-Type", "application/json")
	if results == "" {
		results = "[]"
	}
	_, _ = w.Write([]byte(`{"found":` + boolJSON(found) + `,"results":` + results + `}`))
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// TestAniSkipGetSkipTimes pins the v2 GET shape: path
// /v2/skip-times/{mal}/{episode}, query types=op&types=ed, and the
// interval/skipType/episodeLength response mapping (aniskip v2 camelCase).
func TestAniSkipGetSkipTimes(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		serveSkipTimes(w, true, `[
			{"interval":{"startTime":0.0,"endTime":90.5},"skipType":"op","skipId":"a1","episodeLength":1440.0},
			{"interval":{"startTime":1300.25,"endTime":1400.0},"skipType":"ed","skipId":"b2","episodeLength":1440.0}
		]`)
	})
	c := f.client(t, AniSkipOptions{})

	intervals, err := c.GetSkipTimes(context.Background(), 21, 2)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0].Method != http.MethodGet {
		t.Errorf("method = %s, want GET", reqs[0].Method)
	}
	if reqs[0].Path != "/v2/skip-times/21/2" {
		t.Errorf("path = %q, want /v2/skip-times/21/2", reqs[0].Path)
	}
	// PR61 live finding: the aniskip v2 API now REQUIRES the
	// episodeLength query parameter — without it every fetch answers
	// HTTP 400. The fetch length is unknown pre-play, so the neutral
	// 0 (accepted, "unknown") is sent and the authoritative length
	// comes back inside each result.
	if reqs[0].Query != "types=op&types=ed&episodeLength=0" {
		t.Errorf("query = %q, want types=op&types=ed&episodeLength=0", reqs[0].Query)
	}

	want := []Interval{
		{SkipType: "op", StartTime: 0, EndTime: 90.5, EpisodeLength: 1440},
		{SkipType: "ed", StartTime: 1300.25, EndTime: 1400, EpisodeLength: 1440},
	}
	if len(intervals) != len(want) {
		t.Fatalf("intervals = %+v, want %+v", intervals, want)
	}
	for i, iv := range intervals {
		if iv != want[i] {
			t.Errorf("intervals[%d] = %+v, want %+v", i, iv, want[i])
		}
	}
}

// TestAniSkipGetSkipTimesEpisodeNormalization pins fractional episode
// formatting: integers render without ".0", fractions survive.
func TestAniSkipGetSkipTimesEpisodeNormalization(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		serveSkipTimes(w, false, "")
	})
	c := f.client(t, AniSkipOptions{})

	if _, err := c.GetSkipTimes(context.Background(), 1, 2.5); err != nil {
		t.Fatalf("GetSkipTimes(2.5): %v", err)
	}
	if _, err := c.GetSkipTimes(context.Background(), 1, 7); err != nil {
		t.Fatalf("GetSkipTimes(7): %v", err)
	}

	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].Path != "/v2/skip-times/1/2.5" {
		t.Errorf("fractional path = %q, want /v2/skip-times/1/2.5", reqs[0].Path)
	}
	if reqs[1].Path != "/v2/skip-times/1/7" {
		t.Errorf("integer path = %q, want /v2/skip-times/1/7 (no .0)", reqs[1].Path)
	}
}

// TestAniSkipGetSkipTimesNotFound pins: found=false is a clean empty
// result, not an error.
func TestAniSkipGetSkipTimesNotFound(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		serveSkipTimes(w, false, "")
	})
	c := f.client(t, AniSkipOptions{})

	intervals, err := c.GetSkipTimes(context.Background(), 999, 1)
	if err != nil {
		t.Fatalf("GetSkipTimes: %v", err)
	}
	if len(intervals) != 0 {
		t.Errorf("intervals = %+v, want empty", intervals)
	}
}

// TestAniSkipGetSkipTimesHTTPError pins error propagation on a final
// non-retriable status.
func TestAniSkipGetSkipTimesHTTPError(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := f.client(t, AniSkipOptions{})

	if _, err := c.GetSkipTimes(context.Background(), 1, 1); err == nil {
		t.Fatal("GetSkipTimes on 404 returned nil error, want error")
	}
}

// TestAniSkipSubmitPostsSixFieldPayload pins the submit call shape: POST
// /v2/skip-times/{mal}/{episode} with the six payload fields (skipType,
// providerName, startTime, endTime, episodeLength, submitterId — v2
// camelCase, verified against the live API 2026-09-18).
func TestAniSkipSubmitPostsSixFieldPayload(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":"created"}`))
	})
	c := f.client(t, AniSkipOptions{SubmitterID: "sub-1", ProviderName: "anicli-go"})

	err := c.SubmitSkipTime(context.Background(), SubmitRequest{
		ShikimoriID:   21,
		EpisodeNum:    2,
		SkipType:      "op",
		StartTime:     0,
		EndTime:       90,
		EpisodeLength: 1440,
	})
	if err != nil {
		t.Fatalf("SubmitSkipTime: %v", err)
	}

	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.Path != "/v2/skip-times/21/2" {
		t.Errorf("path = %q, want /v2/skip-times/21/2", req.Path)
	}
	wantFields := map[string]any{
		"skipType":      "op",
		"providerName":  "anicli-go",
		"startTime":     float64(0),
		"endTime":       float64(90),
		"episodeLength": float64(1440),
		"submitterId":   "sub-1",
	}
	var got map[string]any
	if err := jsonUnmarshalString(req.Body, &got); err != nil {
		t.Fatalf("submit body is not JSON: %v", err)
	}
	if len(got) != len(wantFields) {
		t.Errorf("submit body = %q, want exactly %d fields", req.Body, len(wantFields))
	}
	for k, v := range wantFields {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("submit body missing %q", k)
			continue
		}
		num, isNum := v.(float64)
		if isNum {
			gotNum, err := toFloat(gotV)
			if err != nil || gotNum != num {
				t.Errorf("submit body[%q] = %v, want %v", k, gotV, num)
			}
			continue
		}
		if gotV != v {
			t.Errorf("submit body[%q] = %v, want %v", k, gotV, v)
		}
	}
}

// TestAniSkipSubmitUnconfiguredSubmitterIsNoop pins the python guard:
// without a submitter id the submission is a deliberate no-op.
func TestAniSkipSubmitUnconfiguredSubmitterIsNoop(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unconfigured submitter must not touch the network")
	})
	c := f.client(t, AniSkipOptions{SubmitterID: ""})

	if err := c.SubmitSkipTime(context.Background(), SubmitRequest{
		ShikimoriID: 21, EpisodeNum: 2, SkipType: "op",
		StartTime: 0, EndTime: 90, EpisodeLength: 1440,
	}); err != nil {
		t.Fatalf("SubmitSkipTime without submitter: %v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

// TestAniSkipSubmitRejectsUnsupportedType pins: the remote strictly
// supports op/ed; anything else fails loudly instead of being sent.
func TestAniSkipSubmitRejectsUnsupportedType(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsupported skip type must not touch the network")
	})
	c := f.client(t, AniSkipOptions{SubmitterID: "sub-1"})

	err := c.SubmitSkipTime(context.Background(), SubmitRequest{
		ShikimoriID: 21, EpisodeNum: 2, SkipType: "recap",
		StartTime: 10, EndTime: 30, EpisodeLength: 1440,
	})
	if err == nil {
		t.Fatal("SubmitSkipTime with recap returned nil, want ErrUnsupportedSkipType")
	}
	if !isErrUnsupportedSkipType(err) {
		t.Fatalf("SubmitSkipTime err = %v, want ErrUnsupportedSkipType", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

// TestAniSkipSubmitPropagatesHTTPError pins loud error propagation on a
// failed submit (deliberate divergence: python swallowed submit errors).
func TestAniSkipSubmitPropagatesHTTPError(t *testing.T) {
	t.Parallel()

	f := newAniskipFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	c := f.client(t, AniSkipOptions{SubmitterID: "sub-1"})

	err := c.SubmitSkipTime(context.Background(), SubmitRequest{
		ShikimoriID: 21, EpisodeNum: 2, SkipType: "ed",
		StartTime: 1300, EndTime: 1400, EpisodeLength: 1440,
	})
	if err == nil {
		t.Fatal("SubmitSkipTime on 403 returned nil error, want error")
	}
}
