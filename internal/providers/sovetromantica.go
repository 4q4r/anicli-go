package providers

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// SovetRomanticaBase is the site root (anicli-py anicli/providers/
// sovetromantica.py:21).
const SovetRomanticaBase = "https://sovetromantica.com"

// sovetFileRe captures the m3u8 URL from the inline jwplayer setup
// (port of sovetromantica.py:92).
var sovetFileRe = regexp.MustCompile(`file\s*:\s*"([^"]+\.m3u8[^"]*)"`)

// SovetRomantica is the port of anicli-py anicli/providers/
// sovetromantica.py: HTML scraping with a single "SovetRomantica" dub
// whose episodes resolve to one 1080p m3u8 scraped off the episode page.
type SovetRomantica struct {
	Base
}

// newSovetRomantica builds the provider against baseURL.
func newSovetRomantica(baseURL string, http *netclient.Client) *SovetRomantica {
	return &SovetRomantica{Base: Base{
		id:         "sovetromantica",
		name:       "SovetRomantica",
		baseURL:    baseURL,
		sourceType: contracts.SourceTypeBoth,
		headers:    map[string]string{"Referer": baseURL},
		http:       http,
	}}
}

// Search scrapes /anime?query=<quote(query)> (anicli-py
// sovetromantica.py:29-53). The query keeps Python's urllib quote()
// encoding (spaces as %20, not form-style +).
func (p *SovetRomantica) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	searchURL := p.Base.baseURL + "/anime?query=" + pyQuote(query)

	resp, err := p.http.Get(ctx, searchURL, p.headers)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse search page: %w", err))
	}

	var results []contracts.SearchResult
	doc.Find(".anime--block").Each(func(_ int, item *goquery.Selection) {
		titleNode := item.Find(".anime--block__name").First()
		linkNode := item.Find("a").First()
		if titleNode.Length() == 0 || linkNode.Length() == 0 {
			return
		}

		link, _ := linkNode.Attr("href")
		if link == "" {
			return
		}
		if !strings.HasPrefix(link, "http") {
			link = p.Base.baseURL + link
		}

		results = append(results, contracts.SearchResult{
			Title:    strings.TrimSpace(titleNode.Text()),
			URL:      link,
			SourceID: p.ID(),
		})
	})
	return results, nil
}

// GetEpisodes scrapes the anime page (anicli-py sovetromantica.py:55-82).
// Episodes live in .episodes-slick .episode (falling back to
// .episodes-list .episode); the "Эпизод" label is stripped off the span
// text and the raw id is the absolute episode URL.
func (p *SovetRomantica) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Get(ctx, animeURL, p.headers)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	items := doc.Find(".episodes-slick .episode")
	if items.Length() == 0 {
		items = doc.Find(".episodes-list .episode")
	}

	var episodes []contracts.Episode
	items.Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find("a").First()
		if linkNode.Length() == 0 {
			return
		}
		rawURL, _ := linkNode.Attr("href")
		if rawURL == "" {
			return
		}
		if !strings.HasPrefix(rawURL, "http") {
			rawURL = p.Base.baseURL + rawURL
		}

		numNode := item.Find("span").First()
		if numNode.Length() == 0 {
			numNode = item
		}
		num := strings.TrimSpace(
			strings.ReplaceAll(strings.TrimSpace(numNode.Text()), "Эпизод", ""))

		episodes = append(episodes, contracts.Episode{
			Num:   num,
			RawID: rawURL,
			RawEmbeds: map[string][]string{
				"SovetRomantica": {rawURL},
			},
		})
	})

	sort.SliceStable(episodes, func(i, j int) bool {
		return pythonFloatKey(episodes[i].Num) < pythonFloatKey(episodes[j].Num)
	})
	return episodes, nil
}

// ResolveStream fetches the episode page and scrapes the jwplayer m3u8
// URL (port of sovetromantica.py:84-98): one 1080p link, or an empty
// stream when the page has no player.
//
// Divergence from Python (task ruling): the resolved VideoSource carries
// the site root as its Referer header so mpv can play the CDN link; the
// Python original left stream headers empty.
func (p *SovetRomantica) ResolveStream(ctx context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 || embeds[0] == "" {
		return stream, nil
	}

	resp, err := p.http.Get(ctx, embeds[0], p.headers)
	if err != nil {
		return stream, err
	}

	if match := sovetFileRe.FindSubmatch(resp.Body); match != nil {
		m3u8URL := string(match[1])
		if !strings.HasPrefix(m3u8URL, "http") {
			m3u8URL = p.Base.baseURL + m3u8URL
		}
		stream.Links["1080"] = contracts.VideoSource{
			URL:     m3u8URL,
			Quality: "1080",
			Headers: map[string]string{"Referer": p.Base.baseURL},
		}
	}
	return stream, nil
}

// pyQuote ports urllib.parse.quote with its default safe="/" set:
// every byte outside the URL-unreserved set (and "/") is percent-
// encoded uppercase, one UTF-8 byte at a time — spaces become %20, not
// the form-style "+" of url.Values.Encode.
func pyQuote(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-' || c == '_' || c == '.' || c == '~' || c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xF])
		}
	}
	return b.String()
}
