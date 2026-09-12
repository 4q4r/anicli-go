// spike: tls-client probe against anime providers (G1 gate).
// Each provider is probed twice: direct and via local proxy (from ~/.bashrc).
// Gate rule: >=11/14 providers reachable on either path -> GO.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const proxyURL = "http://127.0.0.1:10809"

type probe struct {
	name    string
	url     string
	headers map[string]string // per-provider headers ported from anicli-py
}

// G1 probe list: lightweight search/listing endpoint per provider.
// Endpoints + headers verified 2026-09-12 via Exa + live probes.
// Domain intel:
//   - allanime: rotated 2026-07-22 (ani-cli PR#1779) — API api.mkissa.net,
//     Referer mkissa.to (old allmanga.to gets stripped); dynamic AES key
//     derivation (mask XOR partB + aaReq GCM token) at port time.
//   - kodik: API alive at kodik-api.com (former kodakapi.com NXDOMAIN);
//     401 without token = reachable.
//   - animekai: REMOVED — officially shut down 2026-05-10 (DC fire);
//     .to/anikai.to NXDOMAIN, animekai.com = parked domain-for-sale.
//   - anivibe: WATCH — anivibe.ru resolves but times out everywhere today
//     (former anivibe.net hijacked to ad-farm). Recheck periodically.
var probes = []probe{
	{"anilibria", "https://aniliberty.top/api/v1/app/search/releases?query=test", nil},
	{"sovetromantica", "https://sovetromantica.com/anime?query=test", nil},
	{"animevost", "https://api.animevost.org/v1/search", nil}, // POST-only endpoint; GET probe = reachability
	{"anilib", "https://api.cdnlibs.org/api/anime", map[string]string{
		"Authority": "api.cdnlibs.org",
		"Origin":    "https://animelib.me",
		"Referer":   "https://animelib.me/",
	}},
	{"animego", "https://animego.one/search/anime", nil},
	{"allanime", "https://api.mkissa.net/api", map[string]string{
		"Referer": "https://mkissa.to/",
		"Origin":  "https://mkissa.to",
	}},
	{"gogoanime", "https://gogoanime3.co/search.html", nil},
	{"animepahe", "https://animepahe.ru/api", nil},
	{"dreamcast", "https://dreamerscast.com", nil},
	{"yummyanime", "https://api.yani.tv/anime", nil},
	{"kodik", "https://kodik-api.com/search?title=test&limit=1", nil},
	{"sameband", "https://sameband.studio", nil},
	{"anivibe", "https://anivibe.ru/catalog", nil}, // watch-list, expected down today
}

func newClient(proxy string) (tls_client.HttpClient, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(20),
		tls_client.WithClientProfile(profiles.Chrome_150),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		// Known H3-racing data race in tls-client; disable per design spec §2.
		tls_client.WithDisableHttp3(),
	}
	if proxy != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxy))
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
}

func main() {
	only := ""
	if len(os.Args) > 1 {
		only = os.Args[1]
	}

	direct, err := newClient("")
	if err != nil {
		log.Fatalf("direct client init: %v", err)
	}
	proxied, err := newClient(proxyURL)
	if err != nil {
		log.Fatalf("proxy client init: %v", err)
	}

	ok := 0
	for _, p := range probes {
		if only != "" && p.name != only {
			continue
		}
		dStatus, dLen, dErr := fetchRetry(direct, p)
		pStatus, pLen, pErr := fetchRetry(proxied, p)

		dReach := dErr == nil && dStatus < 500
		pReach := pErr == nil && pStatus < 500
		reach := dReach || pReach
		if reach {
			ok++
		}
		mark := "FAIL"
		if reach {
			mark = " OK "
		}
		fmt.Printf("[%s] %-16s direct: %-28s proxy: %s\n", mark, p.name,
			result(dStatus, dLen, dErr), result(pStatus, pLen, pErr))
	}
	fmt.Printf("\nreachable (either path): %d/13 living providers (anivibe on watch-list) — gate: >=11 -> GO\n", ok)
	if ok < 11 {
		os.Exit(1)
	}
}

// fetchRetry: 2 attempts per path — local proxy occasionally drops first
// connection with EOF (observed on sameband/kodik probes).
func fetchRetry(c tls_client.HttpClient, p probe) (int, int, error) {
	var status int
	var bodyLen int
	var err error
	for range 2 {
		status, bodyLen, err = fetch(c, p)
		if err == nil {
			return status, bodyLen, nil
		}
	}
	return status, bodyLen, err
}

func result(status int, bodyLen int, err error) string {
	if err != nil {
		return shorten(err.Error())
	}
	if status >= 400 {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d (len=%d)", status, bodyLen)
}

func shorten(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

func fetch(c tls_client.HttpClient, p probe) (int, int, error) {
	req, err := http.NewRequest(http.MethodGet, p.url, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, 0, err
	}
	_ = time.Now()
	return resp.StatusCode, len(body), nil
}
