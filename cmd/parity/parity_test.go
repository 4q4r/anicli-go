package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
)

// parityProvider is a fake provider whose Search rides the real
// netclient transport against an httptest loopback server; episodes and
// resolve are deterministic in-memory answers. No test here touches the
// real network.
type parityProvider struct {
	id    string
	http  *netclient.Client
	srv   *httptest.Server
	fail  bool
	epURL string
}

func newParityProvider(t *testing.T, id string, fail bool) *parityProvider {
	t.Helper()

	p := &parityProvider{id: id, fail: fail}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"title": id + " hit for " + q, "url": "https://" + id + ".example/a"}},
		})
	}))
	t.Cleanup(p.srv.Close)

	cfg := config.Default().Network
	cfg.ProxyURL = ""
	client, err := netclient.New(cfg, netclient.WithProvider(id))
	if err != nil {
		t.Fatalf("netclient.New(%s): %v", id, err)
	}
	p.http = client
	return p
}

func (p *parityProvider) ID() string                       { return p.id }
func (p *parityProvider) Name() string                     { return p.id }
func (p *parityProvider) BaseURL() string                  { return "https://" + p.id + ".example" }
func (p *parityProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

func (p *parityProvider) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	if p.fail {
		return nil, contracts.WrapProvider(p.id, contracts.OpSearch, 0, errors.New("parity fake failure"))
	}
	target := p.srv.URL + "/search?q=" + url.QueryEscape(query)
	resp, err := p.http.Get(ctx, target, nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, err
	}
	out := make([]contracts.SearchResult, 0, len(payload.Results))
	for _, r := range payload.Results {
		out = append(out, contracts.SearchResult{Title: r.Title, URL: r.URL, SourceID: p.id})
	}
	return out, nil
}

func (p *parityProvider) GetEpisodes(_ context.Context, animeURL string) ([]contracts.Episode, error) {
	if p.fail {
		return nil, contracts.WrapProvider(p.id, contracts.OpGetEpisodes, 0, errors.New("parity fake failure"))
	}
	p.epURL = animeURL
	return []contracts.Episode{
		{
			Num:       "1",
			Title:     "Episode 1",
			RawID:     "ep-1",
			RawEmbeds: map[string][]string{"1080": {"https://" + p.id + ".example/embed/1"}},
		},
		{
			Num:       "2",
			Title:     "Episode 2",
			RawID:     "ep-2",
			RawEmbeds: map[string][]string{"720": {"https://" + p.id + ".example/embed/2"}},
		},
	}, nil
}

func (p *parityProvider) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if _, ok := episode.RawEmbeds[dubID]; !ok {
		return contracts.MediaStream{}, contracts.WrapProvider(p.id, contracts.OpResolveStream, 0,
			fmt.Errorf("dub %q not present on episode %s", dubID, episode.Num))
	}
	return contracts.MediaStream{
		DubName: dubID,
		Links: map[string]contracts.VideoSource{
			dubID: {URL: "https://" + p.id + ".example/media/" + episode.Num + ".m3u8", Quality: dubID, Type: "m3u8"},
		},
	}, nil
}

// newToolDeps builds run() dependencies over n fake providers inside a
// temp save directory.
func newToolDeps(t *testing.T, n int, failIdx ...int) deps {
	t.Helper()

	saveDir := t.TempDir()
	fail := map[int]bool{}
	for _, i := range failIdx {
		fail[i] = true
	}
	return deps{
		buildRegistry: func(config.Settings) (*providers.Registry, error) {
			reg := providers.NewEmptyRegistry()
			for i := range n {
				p := newParityProvider(t, fmt.Sprintf("p%02d", i), fail[i])
				if err := reg.Register(p); err != nil {
					return nil, err
				}
			}
			return reg, nil
		},
		now:     func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) },
		saveDir: saveDir,
	}
}

func readSaved(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read save dir: %v", err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func TestParitySearchSavesAndPrintsJSON(t *testing.T) {
	d := newToolDeps(t, 1)
	var out, errOut strings.Builder

	code := run([]string{"search", "p00", "test query"}, &out, &errOut, d)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, errOut.String())
	}

	if !strings.Contains(out.String(), `"op": "search"`) ||
		!strings.Contains(out.String(), `"provider": "p00"`) ||
		!strings.Contains(out.String(), "p00 hit for test query") {
		t.Fatalf("stdout missing expected JSON fields:\n%s", out.String())
	}

	saved := readSaved(t, d.saveDir)
	wantName := "p00-search-20260913T120000Z.json"
	raw, ok := saved[wantName]
	if !ok {
		t.Fatalf("expected saved capture %q, got %v", wantName, keysOf(saved))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("saved capture is not JSON: %v", err)
	}
	if payload["provider"] != "p00" || payload["op"] != "search" {
		t.Fatalf("saved capture wrong provider/op: %v", payload)
	}
}

func TestParitySearchUnknownProviderFails(t *testing.T) {
	d := newToolDeps(t, 1)
	var out, errOut strings.Builder

	code := run([]string{"search", "nope", "test"}, &out, &errOut, d)
	if code == 0 {
		t.Fatalf("unknown provider must exit non-zero, stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "nope") {
		t.Fatalf("stderr must name the unknown provider, got: %s", errOut.String())
	}
}

func TestParityEpisodesSavesList(t *testing.T) {
	d := newToolDeps(t, 1)
	var out, errOut strings.Builder

	code := run([]string{"episodes", "p00", "https://p00.example/anime/1"}, &out, &errOut, d)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"op": "episodes"`) || !strings.Contains(out.String(), "ep-1") {
		t.Fatalf("stdout missing episode data:\n%s", out.String())
	}
	saved := readSaved(t, d.saveDir)
	if _, ok := saved["p00-episodes-20260913T120000Z.json"]; !ok {
		t.Fatalf("expected episodes capture file, got %v", keysOf(saved))
	}
}

func TestParityResolveSavesStream(t *testing.T) {
	d := newToolDeps(t, 1)
	var out, errOut strings.Builder

	code := run([]string{"resolve", "p00", "https://p00.example/anime/1", "1080"}, &out, &errOut, d)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"op": "resolve"`) ||
		!strings.Contains(out.String(), "https://p00.example/media/1.m3u8") {
		t.Fatalf("stdout missing resolved stream:\n%s", out.String())
	}
	saved := readSaved(t, d.saveDir)
	if _, ok := saved["p00-resolve-20260913T120000Z.json"]; !ok {
		t.Fatalf("expected resolve capture file, got %v", keysOf(saved))
	}
}

func TestParityResolveUnknownDubFails(t *testing.T) {
	d := newToolDeps(t, 1)
	var out, errOut strings.Builder

	code := run([]string{"resolve", "p00", "https://p00.example/anime/1", "480"}, &out, &errOut, d)
	if code == 0 {
		t.Fatalf("unknown dub must exit non-zero, stdout:\n%s", out.String())
	}
}

func TestParityAllGatePassesWithAllOK(t *testing.T) {
	d := newToolDeps(t, 11)
	var out, errOut strings.Builder

	code := run([]string{"all"}, &out, &errOut, d)
	if code != 0 {
		t.Fatalf("11/11 OK must pass the gate, exit %d, stderr: %s", code, errOut.String())
	}
	table := out.String()
	if !strings.Contains(table, "OK") || !strings.Contains(table, "11/11") {
		t.Fatalf("summary table missing OK rows or total:\n%s", table)
	}
	// Every provider row present.
	for i := range 11 {
		if !strings.Contains(table, fmt.Sprintf("p%02d", i)) {
			t.Fatalf("table missing provider p%02d:\n%s", i, table)
		}
	}
}

func TestParityAllGateFailsBelowEleven(t *testing.T) {
	d := newToolDeps(t, 11, 3) // provider p03 fails both queries.
	var out, errOut strings.Builder

	code := run([]string{"all"}, &out, &errOut, d)
	if code == 0 {
		t.Fatalf("10/11 OK must fail the gate, stdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "10/11") || !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("summary must show 10/11 and a FAIL row:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "gate") {
		t.Fatalf("stderr must name the gate failure, got: %s", errOut.String())
	}
}

func TestParityFlagsAccepted(t *testing.T) {
	d := newToolDeps(t, 1)
	var out, errOut strings.Builder

	code := run([]string{"--proxy", "http://127.0.0.1:10809", "--timeout", "10s", "search", "p00", "x"}, &out, &errOut, d)
	if code != 0 {
		t.Fatalf("flags must be accepted, exit %d, stderr: %s", code, errOut.String())
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
