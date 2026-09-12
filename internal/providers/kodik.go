package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// KodikAPIBase is the JSON API root. Domain intel (verified 2026-09-12):
// the Python base kodakapi.com and its kodikapi.* failover list are
// dead; the live API lives at kodik-api.com and answers 401 without a
// token. The Python multi-host failover loop is therefore replaced by
// this single verified base.
const KodikAPIBase = "https://kodik-api.com"

// kodikPlayerBase prefixes the constructed player embed URLs
// (kodik.py:144, 161).
const kodikPlayerBase = "https://kodik.info"

// kodikMediaIDRe and kodikMediaHashRe scrape the single-translation
// fallback out of inline scripts (kodik.py:234-240).
var (
	kodikMediaIDRe   = regexp.MustCompile(`\.media_id\s*=\s*["']([^"']+)["']`)
	kodikMediaHashRe = regexp.MustCompile(`\.media_hash\s*=\s*["']([^"']+)["']`)
)

// Kodik is the port of anicli-py anicli/providers/kodik.py: a tokenled
// /search API plus scraped kodik.info player pages that expose every
// translation ("dub") as its own embed set.
type Kodik struct {
	Base

	// token is the API token from settings (providers.kodik.token or
	// ANICLI_KODIK_TOKEN).
	token string
}

// newKodik builds the provider against the API base with its token.
//
// Divergence from Python (task ruling): the original scraped its token
// from third-party JS with a hardcoded last-resort constant
// (kodik.py:251-286); the port takes it from configuration and fails
// loud on use when it is empty. The Python original also sent no extra
// headers; none are added.
func newKodik(apiBase, token string, http *netclient.Client) *Kodik {
	return &Kodik{Base: Base{
		id:         "kodik",
		name:       "Kodik",
		baseURL:    apiBase,
		sourceType: contracts.SourceTypeBoth,
		http:       http,
	},
		token: token}
}

// Search POSTs the tokenled search form (port of kodik.py:42-95).
// Error policy:
//   - an empty token fails loud before any request (task ruling; the
//     API would answer 401 anyway);
//   - HTTP 401 maps onto a loud config-flavored error naming the token
//     settings (the Python failover loop swallowed it into []);
//   - decode and other transport failures return an empty result set,
//     matching the Python per-host except-block.
func (p *Kodik) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	if p.token == "" {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, 0,
			fmt.Errorf("%w: kodik token is empty: set providers.kodik.token in settings.toml or ANICLI_KODIK_TOKEN",
				contracts.ErrInvalidInput))
	}

	form := url.Values{}
	form.Set("token", p.token)
	form.Set("title", query)
	form.Set("limit", "20")
	form.Set("with_material_data", "true")
	form.Set("types", "anime,anime-serial")

	resp, err := p.http.Do(ctx, netclient.Request{
		Method:  "POST",
		URL:     p.baseURL + "/search",
		Headers: formContentType,
		Body:    strings.NewReader(form.Encode()),
		Op:      contracts.OpSearch,
	})
	if err != nil {
		var perr *contracts.ProviderError
		if errors.As(err, &perr) && perr.StatusCode == http.StatusUnauthorized {
			return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, perr.StatusCode,
				fmt.Errorf("%w: kodik api rejected the token: check providers.kodik.token or ANICLI_KODIK_TOKEN",
					contracts.ErrInvalidInput))
		}
		return []contracts.SearchResult{}, nil
	}

	data, ok := safeJSONLoads(resp.Body)
	if !ok {
		return []contracts.SearchResult{}, nil
	}

	rawResults, _ := data["results"].([]any)
	results := make([]contracts.SearchResult, 0, len(rawResults))
	for _, rawItem := range rawResults {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		title := asString(item["title"])
		titleOrig := asString(item["title_orig"])
		display := firstNonEmpty(title, titleOrig, "Unknown")

		link := normalizeProtocolURL(asString(item["link"]))
		if link == "" {
			continue
		}

		poster := ""
		if material, ok := item["material_data"].(map[string]any); ok {
			poster = firstNonEmpty(asString(material["poster_url"]),
				asString(material["anime_poster_url"]))
		}

		meta := map[string]any{
			"year": item["year"], // present-but-null mirrors item.get("year")
			"type": asString(item["type"]),
		}
		results = append(results, contracts.SearchResult{
			Title:    display,
			URL:      link,
			SourceID: p.ID(),
			Poster:   poster,
			Meta:     meta,
		})
	}
	return results, nil
}

// GetEpisodes scrapes the kodik.info player page the search returned
// (port of kodik.py:97-170). Serial pages build one episode per
// series-box option with an embed per translation; movie pages yield a
// single film entry. Transport failures return an empty list (the
// Python try-block); a promo-error page does too.
func (p *Kodik) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    animeURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return []contracts.Episode{}, nil
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return []contracts.Episode{}, nil
	}

	if doc.Find(".promo-error").Length() > 0 {
		return []contracts.Episode{}, nil
	}

	isSerial := strings.Contains(animeURL, "/serial/") ||
		doc.Find(".serial-series-box").Length() > 0

	translations := parseKodikTranslations(doc, isSerial)

	var episodes []contracts.Episode
	if isSerial {
		seriesBox := doc.Find(".serial-series-box select").First()
		if seriesBox.Length() == 0 {
			return []contracts.Episode{}, nil
		}
		seriesBox.Find("option").Each(func(_ int, opt *goquery.Selection) {
			epNum := strings.TrimSpace(opt.Text())
			embeds := map[string][]string{}
			for name, tr := range translations {
				// URL pattern from kodik.py:143-147 (season hardcoded 1).
				embeds[name] = []string{fmt.Sprintf(
					"%s/serial/%s/%s/720p?min_age=16&first_url=false&season=1&episode=%s",
					kodikPlayerBase, tr.id, tr.hash, epNum)}
			}
			episodes = append(episodes, contracts.Episode{
				Num:       epNum,
				RawID:     epNum,
				RawEmbeds: embeds,
			})
		})
		return episodes, nil
	}

	// Movie / single episode (kodik.py:153-168).
	embeds := map[string][]string{}
	for name, tr := range translations {
		embeds[name] = []string{fmt.Sprintf(
			"%s/video/%s/%s/720p?min_age=16&first_url=false",
			kodikPlayerBase, tr.id, tr.hash)}
	}
	return []contracts.Episode{{
		Num:       "1",
		Title:     "Фильм",
		RawID:     "movie",
		RawEmbeds: embeds,
	}}, nil
}

// ResolveStream resolves the chosen translation's player link (port of
// kodik.py:172-183): the kodik.info URL runs through the extractor
// factory, which folds the /ftor API sources into the stream.
func (p *Kodik) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	sources, err := resolveEmbeds(ctx, p.http, episode.RawEmbeds[dubID])
	if err != nil {
		return stream, contracts.WrapProvider(p.ID(), contracts.OpResolveStream, 0, err)
	}
	stream.Links = sources
	return stream, nil
}

// kodikTranslation is one scraped translation entry (kodik.py:185-249).
type kodikTranslation struct {
	id   string
	hash string
}

// parseKodikTranslations scrapes the translation select box; without
// one it falls back to the single-translation script globals under the
// "Default" name.
func parseKodikTranslations(doc *goquery.Document, isSerial bool) map[string]kodikTranslation {
	selector := ".movie-translations-box select"
	if isSerial {
		selector = ".serial-translations-box select"
	}

	translations := map[string]kodikTranslation{}
	selectNode := doc.Find(selector).First()
	if selectNode.Length() > 0 {
		selectNode.Find("option").Each(func(_ int, opt *goquery.Selection) {
			mediaID, _ := opt.Attr("data-media-id")
			mediaHash, _ := opt.Attr("data-media-hash")
			if mediaID == "" || mediaHash == "" {
				return
			}
			translations[strings.TrimSpace(opt.Text())] = kodikTranslation{
				id:   mediaID,
				hash: mediaHash,
			}
		})
		return translations
	}

	doc.Find("script").EachWithBreak(func(_ int, script *goquery.Selection) bool {
		txt := script.Text()
		if !strings.Contains(txt, ".media_id") || !strings.Contains(txt, ".media_hash") {
			return true
		}
		idMatch := kodikMediaIDRe.FindStringSubmatch(txt)
		hashMatch := kodikMediaHashRe.FindStringSubmatch(txt)
		if idMatch != nil && hashMatch != nil {
			translations["Default"] = kodikTranslation{id: idMatch[1], hash: hashMatch[1]}
		}
		return false // Python breaks after the first matching script
	})
	return translations
}

// asString renders a decoded JSON scalar the way item.get() + str
// context does here: strings pass through, everything else (numbers,
// null, missing) yields "" for the string-typed consumers.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
