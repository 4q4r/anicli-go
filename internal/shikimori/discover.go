package shikimori

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// Discovery surface of the Shikimori client (PR10 + F41 ride-along),
// ported from anicli-py anicli/core/shikimori.py:88-174 (SearchIDs,
// GetAnimesInfo) and the api_server.py HTML parsers
// (_parse_shikimori_autocomplete_html, _fetch_hero_candidates_from_ongoing).

// AutocompleteItem is one parsed autocomplete record
// (api_server.py _parse_shikimori_autocomplete_html item shape).
type AutocompleteItem struct {
	ShikimoriID int64   `json:"shikimori_id"`
	TitleRu     *string `json:"title_ru"`
	TitleEn     *string `json:"title_en"`
	PosterURL   *string `json:"poster_url"`
	Type        *string `json:"type"`
	Year        *int    `json:"year"`
	URL         string  `json:"url"`
}

// OngoingCandidate is one parsed row of the ongoing-anime gallery page
// (api_server.py _fetch_hero_candidates_from_ongoing). The API layer
// attaches badges and pagination.
type OngoingCandidate struct {
	ShikimoriID int64
	TitleRu     *string
	TitleEn     *string
	PosterURL   string
	Year        *int
	SourceURL   string
}

// digitsRE extracts the first run of decimal digits (python
// _coerce_int's re.search(r"\d+")). Signed values are not matched.
var digitsRE = regexp.MustCompile(`\d+`)

// tagRE strips HTML tags from a node's inner HTML (python
// re.sub(r"<[^>]+>", " ", prefix)).
var tagRE = regexp.MustCompile(`<[^>]+>`)

// separatorSpan is the inline separator marker inside autocomplete name
// anchors; the russian title is the text before it.
const separatorSpan = `<span class="b-separator inline"`

// normalizeSpace collapses all whitespace runs into single spaces and
// trims (python _normalize_space).
func normalizeSpace(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// coerceInt converts the first digit run of s into an int (python
// _coerce_int over strings).
func coerceInt(s string) (int, bool) {
	m := digitsRE.FindString(s)
	if m == "" {
		return 0, false
	}
	v, err := strconv.Atoi(m)
	if err != nil {
		return 0, false
	}
	return v, true
}

// resolveShikimoriURL normalizes relative URLs against the site root
// (python _resolve_shikimori_url): "//host/path" gains the scheme,
// "/path" gains the base, bare paths join with "/", absolute URLs pass
// through.
func resolveShikimoriURL(baseURL, raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(cleaned, "//"):
		return "https:" + cleaned
	case strings.HasPrefix(cleaned, "/"):
		return strings.TrimRight(baseURL, "/") + cleaned
	case strings.HasPrefix(cleaned, "http://"), strings.HasPrefix(cleaned, "https://"):
		return cleaned
	default:
		return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(cleaned, "/")
	}
}

// SearchIDs queries the autocomplete endpoint and maps
// {data-text: data-id} for anime rows (port of shikimori.py:88-122).
// Failures surface as an empty map, matching the Python exception
// swallow — callers treat "no ids" as "no match".
func (c *Client) SearchIDs(ctx context.Context, query string) (map[string]int64, error) {
	results := map[string]int64{}
	items, err := c.fetchAutocomplete(ctx, query)
	if err != nil {
		return results, nil
	}
	items.Each(func(_ int, node *goquery.Selection) {
		if attr, ok := node.Attr("data-type"); !ok || attr != "anime" {
			return
		}
		idRaw, hasID := node.Attr("data-id")
		title, hasTitle := node.Attr("data-text")
		if !hasID || !hasTitle {
			return
		}
		id, err := strconv.ParseInt(strings.TrimSpace(idRaw), 10, 64)
		if err != nil {
			return
		}
		results[title] = id
	})
	return results, nil
}

// Autocomplete parses the autocomplete HTML into the rich records the
// API search endpoint returns (shikimori_id, ru/en titles, poster, kind,
// year, canonical url), deduplicated by id and capped at limit.
func (c *Client) Autocomplete(ctx context.Context, query string, limit int) ([]AutocompleteItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	nodes, err := c.fetchAutocomplete(ctx, query)
	if err != nil {
		return nil, err
	}

	baseURL := c.baseURL
	items := make([]AutocompleteItem, 0, limit)
	seen := map[int64]bool{}
	nodes.Each(func(_ int, node *goquery.Selection) {
		if len(items) >= limit {
			return
		}
		if attr, ok := node.Attr("data-type"); !ok || attr != "anime" {
			return
		}
		idRaw, ok := node.Attr("data-id")
		if !ok {
			return
		}
		id, err := strconv.ParseInt(strings.TrimSpace(idRaw), 10, 64)
		if err != nil || seen[id] {
			return
		}
		seen[id] = true

		titleEn := normalizeSpace(attrOr(node, "data-text"))
		anchor := node.Find("div.info > div.name > a")
		if anchor.Length() == 0 {
			anchor = node.Find(".info .name a")
		}
		item := AutocompleteItem{
			ShikimoriID: id,
			URL:         fmt.Sprintf("/anime/%d", id),
		}
		if titleEn != "" {
			item.TitleEn = &titleEn
		}
		if ru := titleRuFromAnchor(anchor); ru != "" {
			item.TitleRu = &ru
		}
		imgNode := node.Find("picture img")
		if imgNode.Length() == 0 {
			imgNode = node.Find("img")
		}
		if poster := preferredImgSrc(imgNode); poster != "" {
			if resolved := resolveShikimoriURL(baseURL, poster); resolved != "" {
				item.PosterURL = &resolved
			}
		}
		if kind := node.Find(`div.b-tag[data-href*="/kind/"]`); kind.Length() > 0 {
			if v := normalizeSpace(kind.Text()); v != "" {
				item.Type = &v
			}
		}
		if year := node.Find(`div.b-tag[data-href*="/season/"]`); year.Length() > 0 {
			if y, ok := coerceInt(year.Text()); ok {
				item.Year = &y
			}
		}
		items = append(items, item)
	})
	return items, nil
}

// fetchAutocomplete performs the autocomplete GET and returns the item
// nodes of the embedded HTML content.
func (c *Client) fetchAutocomplete(ctx context.Context, query string) (*goquery.Selection, error) {
	q := url.Values{}
	q.Set("search", query)
	resp, err := c.get(ctx, "/animes/autocomplete/v2", q)
	if err != nil {
		return nil, fmt.Errorf("shikimori autocomplete: %w", err)
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("shikimori autocomplete: decode response: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(payload.Content))
	if err != nil {
		return nil, fmt.Errorf("shikimori autocomplete: parse html: %w", err)
	}
	return doc.Find("div.b-db_entry-variant-list_item"), nil
}

// titleRuFromAnchor extracts the russian title: the inner-HTML text
// before the inline separator span, falling back to the anchor text
// split on "/" (python _extract_title_ru_from_name_anchor).
func titleRuFromAnchor(anchor *goquery.Selection) string {
	if anchor.Length() == 0 {
		return ""
	}
	if raw, err := anchor.Html(); err == nil && strings.TrimSpace(raw) != "" {
		prefix := raw
		if idx := strings.Index(raw, separatorSpan); idx >= 0 {
			prefix = raw[:idx]
		}
		candidate := normalizeSpace(html.UnescapeString(tagRE.ReplaceAllString(prefix, " ")))
		if candidate != "" {
			return candidate
		}
	}
	fallback := normalizeSpace(anchor.Text())
	if fallback == "" {
		return ""
	}
	if idx := strings.Index(fallback, "/"); idx >= 0 {
		fallback = normalizeSpace(fallback[:idx])
	}
	return fallback
}

// preferredImgSrc resolves the best image URL from an img node: the
// densest srcset entry first (python iterates reversed), then
// src/data-src/data-original (python _extract_preferred_img_src).
func preferredImgSrc(img *goquery.Selection) string {
	if img.Length() == 0 {
		return ""
	}
	if srcset, ok := img.Attr("srcset"); ok && strings.TrimSpace(srcset) != "" {
		candidates := strings.Split(srcset, ",")
		for i := len(candidates) - 1; i >= 0; i-- {
			parts := strings.Fields(strings.TrimSpace(candidates[i]))
			if len(parts) > 0 && parts[0] != "" {
				return parts[0]
			}
		}
	}
	for _, attr := range []string{"src", "data-src", "data-original"} {
		if v, ok := img.Attr(attr); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// attrOr reads an attribute with an empty default.
func attrOr(s *goquery.Selection, name string) string {
	v, _ := s.Attr(name)
	return v
}

// GetAnimesInfo fetches the brief anime rows for a list of ids, chunked
// at 50 ids per request to respect the API limits (port of
// shikimori.py:143-174: GET /api/animes?ids=...&limit=50). A failing
// chunk contributes nothing but does not fail the batch.
func (c *Client) GetAnimesInfo(ctx context.Context, ids []int64) ([]Anime, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if err := c.requireMode(false); err != nil {
		return nil, err
	}

	const chunkSize = 50
	var out []Anime
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		parts := make([]string, 0, len(chunk))
		for _, id := range chunk {
			parts = append(parts, strconv.FormatInt(id, 10))
		}
		query := url.Values{}
		query.Set("ids", strings.Join(parts, ","))
		query.Set("limit", "50")

		resp, err := c.get(ctx, "/api/animes", query)
		if err != nil {
			continue // python: silently skip a failing chunk
		}
		var rows []Anime
		if err := json.Unmarshal(resp.Body, &rows); err != nil {
			continue
		}
		out = append(out, rows...)
	}
	return out, nil
}

// FetchOngoingCandidates fetches and parses the public ongoing-anime
// gallery page (/animes/status/ongoing) into hero-gallery candidates
// (port of api_server.py _fetch_hero_candidates_from_ongoing). A fetch
// failure yields an empty list (python exception swallow).
func (c *Client) FetchOngoingCandidates(ctx context.Context) ([]OngoingCandidate, error) {
	if err := c.requireMode(false); err != nil {
		return nil, err
	}
	if err := c.lim.Wait(ctx); err != nil {
		return nil, fmt.Errorf("shikimori: rate limiter: %w", err)
	}
	resp, err := c.net.Do(ctx, netclientRequest(http.MethodGet, c.baseURL+"/animes/status/ongoing", c.pageHeaders()))
	if err != nil {
		return []OngoingCandidate{}, nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(resp.Body)))
	if err != nil {
		return []OngoingCandidate{}, nil
	}

	baseURL := c.baseURL
	var out []OngoingCandidate
	seen := map[int64]bool{}
	doc.Find(".cc-entries article.b-catalog_entry.c-anime").Each(func(_ int, article *goquery.Selection) {
		idRaw, ok := article.Attr("id")
		if !ok {
			return
		}
		id, err := strconv.ParseInt(strings.TrimSpace(idRaw), 10, 64)
		if err != nil || seen[id] {
			return
		}

		titleEn := normalizeSpace(article.Find(".title .name-en").Text())
		titleRu := normalizeSpace(article.Find(".title .name-ru").Text())
		if titleEn == "" && titleRu == "" {
			return
		}
		seen[id] = true

		background := resolveShikimoriURL(baseURL, preferredImgSrc(article.Find("picture img")))

		hrefNode := article.Find(`a.cover[href]`)
		if hrefNode.Length() == 0 {
			hrefNode = article.Find(`a.title[href]`)
		}
		href, _ := hrefNode.Attr("href")
		sourceURL := resolveShikimoriURL(baseURL, href)
		if sourceURL == "" {
			sourceURL = fmt.Sprintf("%s/animes/%d", baseURL, id)
		}

		candidate := OngoingCandidate{
			ShikimoriID: id,
			PosterURL:   background,
			SourceURL:   sourceURL,
		}
		if titleEn != "" {
			candidate.TitleEn = &titleEn
		}
		if titleRu != "" {
			candidate.TitleRu = &titleRu
		}
		if spans := article.Find(".misc span"); spans.Length() > 0 {
			last := normalizeSpace(spans.Last().Text())
			if y, ok := coerceInt(last); ok {
				candidate.Year = &y
			}
		}
		out = append(out, candidate)
	})
	return out, nil
}

// netclientRequest builds the netclient.Request literal for the ongoing
// page fetch.
func netclientRequest(method, u string, headers map[string]string) netclient.Request {
	return netclient.Request{Method: method, URL: u, Headers: headers}
}
