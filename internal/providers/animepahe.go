package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// AnimePaheBase is the site root (anicli-py anicli/providers/
// animepahe.py:18). The domain is flaky and bounces .ru → .su at
// runtime; following redirects is the netclient's job (verified
// 2026-09-12).
const AnimePaheBase = "https://animepahe.ru"

// animePaheDub is the fixed single dub resolve_stream would have used
// (animepahe.py:150 hardcodes dub_name="Original (Pahe)").
const animePaheDub = "Original (Pahe)"

// animePaheDropRe captures the quality dropdown anchors of a play page:
// href first, then the class attribute, then a "NNNp" label inside the
// anchor (animepahe.py:113).
var animePaheDropRe = regexp.MustCompile(`<a href="([^"]+)"[^>]+class="dropdown-item"[^>]*>.*?(\d+)p.*?</a>`)

// AnimePahe is the port of anicli-py anicli/providers/animepahe.py: a
// JSON /api face for search and the episode listing, and a scraped
// /play/<anime>/<episode> page whose kwik.cx dropdown links feed the
// extractor factory.
type AnimePahe struct {
	Base
}

// newAnimePahe builds the provider against baseURL.
//
// Divergence from Python (task ruling): the original defines the
// User-Agent+Referer header set (animepahe.py:24) but forgets to pass it
// to any request; the port actually sends the Referer (the UA is already
// applied by the netclient on every request).
func newAnimePahe(baseURL string, http *netclient.Client) *AnimePahe {
	return &AnimePahe{Base: Base{
		id:         "animepahe",
		name:       "AnimePahe",
		baseURL:    baseURL,
		sourceType: contracts.SourceTypeVideo,
		headers:    map[string]string{"Referer": baseURL},
		http:       http,
	}}
}

// animePaheSearch mirrors the fields consumed by animepahe.py:36-43.
type animePaheSearch struct {
	Data []struct {
		Title   string `json:"title"`
		Session string `json:"session"`
		Poster  string `json:"poster"`
	} `json:"data"`
}

// animePaheRelease mirrors the fields consumed by animepahe.py:62-87.
type animePaheRelease struct {
	LastPage int `json:"last_page"`
	Data     []struct {
		Episode json.Number `json:"episode"`
		Session string      `json:"session"`
	} `json:"data"`
}

// Search queries the internal /api endpoint (anicli-py
// animepahe.py:26-46). Python wraps http.get and json.loads in one
// except-block returning []: transport and decode failures both surface
// as an empty result set here, verbatim (documented quirk).
func (p *AnimePahe) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	params := url.Values{}
	params.Set("m", "search")
	params.Set("q", query)

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/api?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpSearch,
	})
	if err != nil {
		return []contracts.SearchResult{}, nil
	}

	var data animePaheSearch
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return []contracts.SearchResult{}, nil
	}

	results := make([]contracts.SearchResult, 0, len(data.Data))
	for _, item := range data.Data {
		results = append(results, contracts.SearchResult{
			Title:    item.Title,
			URL:      item.Session, // session id doubles as the anime id
			SourceID: p.ID(),
			Poster:   item.Poster,
		})
	}
	return results, nil
}

// GetEpisodes pages through the m=release listing (anicli-py
// animepahe.py:48-91). animeURL is the search session id; the first
// response carries last_page and page one, remaining pages are fetched
// sequentially. Failures anywhere in the flow return an empty list (the
// Python whole-call except-block), verbatim.
//
// Divergence from Python (task ruling): Python stored raw_embeds={} and
// derived the play URL inside resolve_stream; the Go contract enumerates
// dubs from RawEmbeds keys, so each episode stashes its deterministic
// play URL under the fixed dub name resolve_stream would have used.
func (p *AnimePahe) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	episodes, lastPage, ok := p.fetchReleasePage(ctx, animeURL, 1)
	if !ok {
		return []contracts.Episode{}, nil
	}

	for page := 2; page <= lastPage; page++ {
		more, _, ok := p.fetchReleasePage(ctx, animeURL, page)
		if !ok {
			return []contracts.Episode{}, nil
		}
		episodes = append(episodes, more...)
	}
	return episodes, nil
}

// fetchReleasePage loads one m=release page, reporting its episodes and
// the listing's last_page (0 when the decode fails).
func (p *AnimePahe) fetchReleasePage(ctx context.Context, animeURL string, page int) ([]contracts.Episode, int, bool) {
	params := url.Values{}
	params.Set("m", "release")
	params.Set("id", animeURL)
	params.Set("sort", "episode_asc")
	params.Set("page", strconv.Itoa(page))

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     p.baseURL + "/api?" + params.Encode(),
		Headers: p.headers,
		Op:      contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, 0, false
	}

	var data animePaheRelease
	if jsonErr := json.Unmarshal(resp.Body, &data); jsonErr != nil {
		return nil, 0, false
	}

	episodes := make([]contracts.Episode, 0, len(data.Data))
	for _, item := range data.Data {
		// Python str(item.get("episode", 0)): the JSON wire literal is
		// kept ("2.5" stays "2.5"); a missing field yields "0".
		num := "0"
		if item.Episode != "" {
			num = pythonStr(item.Episode)
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: "Episode " + num,
			RawID: animeURL + "|" + item.Session,
			RawEmbeds: map[string][]string{
				animePaheDub: {fmt.Sprintf("%s/play/%s/%s", p.baseURL, animeURL, item.Session)},
			},
		})
	}

	return episodes, data.LastPage, true
}

// ResolveStream scrapes the play page's quality dropdown and feeds each
// href to the extractor factory (port of animepahe.py:93-150). Kwik
// embed URLs stay unresolved (the kwik extractor is disabled in the
// Python factory too), so a page of pure kwik links yields an empty
// stream, not an error; direct media hrefs resolve via the fallback.
func (p *AnimePahe) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		// Python hardcodes the dub name, ignoring dub_id (animepahe.py:150).
		DubName: animePaheDub,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 {
		return stream, nil
	}

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "GET",
		URL:     embeds[0],
		Headers: p.headers,
		Op:      contracts.OpResolveStream,
	})
	if err != nil {
		return stream, err
	}

	// The play page is parsed with the Python regex, not goquery: the
	// matched anchor layout is attribute-order-sensitive (href, class).
	for _, match := range animePaheDropRe.FindAllSubmatch(resp.Body, -1) {
		sources, err := resolveEmbeds([]string{string(match[1])})
		if err != nil {
			// Python ignores per-link extraction failures (an empty
			// extractor result updates nothing); a pending extractor
			// must not shadow links that do resolve.
			if len(stream.Links) == 0 {
				return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
			}
			continue
		}
		for quality, src := range sources {
			stream.Links[quality] = src
		}
	}
	return stream, nil
}
