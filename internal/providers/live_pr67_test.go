//go:build live

// PR67 root-cause probe: dumps the RAW live answers of the AllAnime
// resolve chain so the failure classification rests on evidence, not
// on typed-error guesses. Excluded from the hermetic default suite by
// the `live` build tag. Run manually:
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 \
//	  go test -tags live -run TestLivePR67 -count=1 -v ./internal/providers/
//
// Knobs:
//   - ANICLI_LIVE_PROXY   proxy URL; unset = direct route.
//   - AA_SHOW             show _id to probe (default: a Black Lagoon id
//     captured from live search).
//   - AA_BURST            N of concurrent episode POSTs after the
//     polite probe (default 0 = off) — characterizes the limiter.
package providers

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

const aaPr67ShowID = "R6YuQPp3tiXagramE" // Black Lagoon — live search _id (parity search, 2026-09-19)

func aaPr67Provider(t *testing.T) *AllAnime {
	t.Helper()
	cfg := config.Default()
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		cfg.Network.ProxyURL = proxy
	}
	http, err := netclient.New(cfg.Network)
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	return newAllAnime(AllAnimeAPIBase, AllAnimeReferer, AllAnimeInternalBase, http, nil, "")
}

func aaPr67EpisodePost(t *testing.T, p *AllAnime, mat *aaMaterial, label string) {
	t.Helper()
	token, err := aaBuildAAReqAt(aaEpisodeQueryHash, mat.Key, mat.Epoch, mat.BuildID, aaContentLane, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("%s: token: %v", label, err)
	}
	body, err := p.graphqlPost(context.Background(), aaGraphqlRequest{
		Query: aaEpisodeQuery,
		Variables: map[string]any{
			"showId":          aaPr67ShowID,
			"translationType": "sub",
			"episodeString":   "1",
		},
		Extensions: map[string]any{
			"persistedQuery": map[string]any{"version": 1, "sha256Hash": aaEpisodeQueryHash},
			"k":              aaContentLane,
			"aaReq":          token,
		},
	}, map[string]string{"x-build-id": mat.BuildID})
	if err != nil {
		t.Fatalf("%s: transport: %v", label, err)
	}
	text := string(body)
	if len(text) > 700 {
		text = text[:700] + "…"
	}
	t.Logf("[%s] raw body: %s", label, text)
}

// TestLivePR67Material probes the crypto plane: bootstrap material
// (buildId/epoch/switchAt) plus ONE polite episode-sources POST with
// the raw body dumped — the search query is posted once as the
// healthy control.
func TestLivePR67Material(t *testing.T) {
	p := aaPr67Provider(t)
	ctx := context.Background()

	mat, err := p.material.get(ctx)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	t.Logf("material: buildId=%q epoch=%d switchAt=%s", mat.BuildID, mat.Epoch, mat.switchAt.Format(time.RFC3339))

	// Control: search (known healthy).
	searchBody, err := p.graphqlPost(ctx, aaGraphqlRequest{
		Query: aaSearchQuery,
		Variables: map[string]any{
			"search":          map[string]any{"allowAdult": false, "allowUnknown": false, "query": "black lagoon"},
			"limit":           aaSearchLimit,
			"page":            1,
			"translationType": "sub",
			"countryOrigin":   "ALL",
		},
	}, nil)
	if err != nil {
		t.Fatalf("search transport: %v", err)
	}
	t.Logf("[search control] %d bytes, has-edges=%s", len(searchBody), fmt.Sprintf("%v", len(searchBody) > 400))

	aaPr67EpisodePost(t, p, mat, "polite-1")
	time.Sleep(4 * time.Second) // kunai dossier: the API rate-limits bursts (~3s)
	aaPr67EpisodePost(t, p, mat, "polite-2")
}

// TestLivePR67Burst fires N concurrent episode POSTs (AA_BURST, off by
// default) to characterize the limiter threshold after the polite
// probes pass.
func TestLivePR67Burst(t *testing.T) {
	n := 0
	fmt.Sscanf(os.Getenv("AA_BURST"), "%d", &n)
	if n <= 0 {
		t.Skip("set AA_BURST=<n> to probe the burst threshold")
	}
	p := aaPr67Provider(t)
	mat, err := p.material.get(context.Background())
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			aaPr67EpisodePost(t, p, mat, fmt.Sprintf("burst-%d", i))
		}(i)
	}
	wg.Wait()
}
