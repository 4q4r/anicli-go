//go:build live

// PR71 live probe: the full animepahe chain through the real [cf]
// browser bridge — search (in-page fetch), release walk (in-page
// fetch), play page (#resolutionMenu + #pickDownload), the kwik /e/
// embed attempt (expected to wall on WAF-blocked networks), and the
// pahe.win → kwik /f/ → download-capture stream chain. Every hop is
// logged so the failure classification rests on evidence.
//
// Excluded from the hermetic default suite by the `live` build tag:
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 \
//	  go test -tags live -run TestLivePR71 -count=1 -v ./internal/providers/
//
// Knobs:
//   - ANICLI_LIVE_PROXY   proxy URL; unset = direct route.
//   - AP_QUERY            search probe (default "black lagoon").
package providers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

func apPr71Provider(t *testing.T) (*AnimePahe, *cfbrowser.Manager) {
	t.Helper()

	cfg := config.Default()
	cfg.CF.Enabled = true
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		cfg.Network.ProxyURL = proxy
	}
	mgr, err := cfbrowser.NewManager(cfg)
	if err != nil {
		t.Fatalf("cf manager (is the stealth binary installed? `anicli cf install`): %v", err)
	}
	http, err := netclient.New(cfg.Network)
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	return newAnimePahe(AnimePaheBase, http, buildPaheBridge(mgr)), mgr
}

// TestLivePR71Chain runs search → episodes → resolve through the
// browser bridge and logs every hop of the stream chain.
func TestLivePR71Chain(t *testing.T) {
	p, mgr := apPr71Provider(t)
	defer func() { _ = mgr.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	query := os.Getenv("AP_QUERY")
	if query == "" {
		query = "black lagoon"
	}

	results, err := p.Search(ctx, query)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("search: 0 results")
	}
	t.Logf("[search] %d results, first: %q session=%s", len(results), results[0].Title, results[0].URL)

	episodes, err := p.GetEpisodes(ctx, results[0].URL)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}
	if len(episodes) == 0 {
		t.Fatal("episodes: 0")
	}
	t.Logf("[episodes] %d episodes, ep1 raw_id=%s", len(episodes), episodes[0].RawID)

	stream, err := p.ResolveStream(ctx, episodes[0], "Original (Pahe)")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for quality, src := range stream.Links {
		head := src.URL
		if len(head) > 90 {
			head = head[:90] + "…"
		}
		t.Logf("[stream] %s → %s (%s)", quality, head, src.Type)
	}
	if len(stream.Links) == 0 {
		t.Fatal("resolve: 0 stream links")
	}
}

// TestLivePR71InterstitialHop probes ONLY the stream chain's evidence:
// play page parse (both menus), the interstitial target and the kwik
// form, so a wall anywhere surfaces with its exact hop.
func TestLivePR71InterstitialHop(t *testing.T) {
	p, mgr := apPr71Provider(t)
	defer func() { _ = mgr.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	results, err := p.Search(ctx, "black lagoon")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	episodes, err := p.GetEpisodes(ctx, results[0].URL)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}

	playURL := episodes[0].RawEmbeds["Original (Pahe)"][0]
	body, err := p.browser.PageFetch(ctx, playURL)
	if err != nil {
		t.Fatalf("play fetch: %v", err)
	}
	t.Logf("[play] %d bytes; embeds=%v downloads=%v",
		len(body), animePahePlayLinks(body), animePaheDownloadLinks(body))

	downloads := animePaheDownloadLinks(body)
	if len(downloads) == 0 {
		t.Fatal("play page carries no download menu")
	}
	quality := "720"
	interstitial, ok := downloads[quality]
	if !ok {
		for q, u := range downloads {
			quality, interstitial = q, u
			break
		}
	}
	target := ""
	var pageBody []byte
	pageBody, err = p.browser.PageHTML(ctx, interstitial)
	if err != nil {
		t.Fatalf("interstitial %s: %v", interstitial, err)
	}
	target = animePaheInterstitialTarget(pageBody)
	t.Logf("[interstitial] %s → %q (len=%d)", interstitial, target, len(pageBody))
	if target == "" {
		t.Fatal("interstitial exposes no kwik target")
	}

	fileBody, err := p.browser.PageHTML(ctx, target)
	if err != nil {
		t.Fatalf("kwik file page: %v", err)
	}
	action, _, ok := animePaheKwikForm(fileBody)
	if !ok {
		t.Fatalf("kwik file page %s carries no download form", target)
	}
	t.Logf("[kwik form] action=%s", action)

	media, err := p.browser.SubmitDownload(ctx, target, action)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	head := media
	if len(head) > 120 {
		head = head[:120] + "…"
	}
	t.Logf("[media] %s", head)
	if media == "" {
		t.Fatal("empty media URL")
	}
}
