package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// SameBandBase is the site root (anicli-py anicli/providers/
// sameband.py:20).
const SameBandBase = "https://sameband.studio"

// samebandFileRe captures the playlist URL off the embedded player page
// (sameband.py:60). The capture is [^>]+ — non-'>' chars — exactly like
// the Python original.
var samebandFileRe = regexp.MustCompile(`Playerjs[^>]+file:\s*["']([^>]+)["']`)

// samebandQualityRe matches one "[NNNp]<url>" part of a file field
// (sameband.py:89).
var samebandQualityRe = regexp.MustCompile(`^\[(\d+)p\](.*)`)

// SameBand is the port of anicli-py anicli/providers/sameband.py: a DLE
// search form, an iframe-chained player page, a JSON playlist of
// quality-prefixed m3u8 files.
type SameBand struct {
	Base
}

// newSameBand builds the provider against baseURL. The Python original
// sends no extra headers (sameband.py defines none).
func newSameBand(baseURL string, http *netclient.Client) *SameBand {
	return &SameBand{Base: Base{
		id:         "sameband",
		name:       "SameBand",
		baseURL:    baseURL,
		sourceType: contracts.SourceTypeBoth,
		http:       http,
	}}
}

// samebandPlaylist mirrors the fields consumed by sameband.py:74-80.
type samebandPlaylist []struct {
	Title *string `json:"title"`
	File  string  `json:"file"`
}

// Search GETs the /anime catalog and filters client-side [LIVE-VERIFIED
// 2026-09-13: GET /anime → HTTP 200, 94 article.shortstory entries, no
// pagination]. The DLE POST search form the Python original used
// (sameband.py:23-46) is dead server-side: the endpoint answers 200 with
// an empty fastsearch_results shell because the site's search became
// AJAX-only. Card parsing keeps the same selectors the DLE results page
// used (.col-auto / .image[href] / .poster[title] / img.swiper-lazy) —
// the catalog renders the identical shortstory template. Matching is a
// case-insensitive substring test on the card title. Posters stay the
// site root glued onto the swiper img src — even when the src is already
// absolute, verbatim like the Python original (sameband.py:44).
func (p *SameBand) Search(ctx context.Context, query string) ([]contracts.SearchResult, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    p.baseURL + "/anime",
		Op:     contracts.OpSearch,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpSearch, resp.StatusCode,
			fmt.Errorf("parse catalog page: %w", err))
	}

	needle := strings.ToLower(query)
	var results []contracts.SearchResult
	doc.Find(".col-auto").Each(func(_ int, item *goquery.Selection) {
		linkNode := item.Find(".image[href]").First()
		titleNode := item.Find(".poster[title]").First()
		if linkNode.Length() == 0 || titleNode.Length() == 0 {
			return
		}

		link, _ := linkNode.Attr("href")
		title, _ := titleNode.Attr("title")

		if !strings.Contains(strings.ToLower(title), needle) {
			return
		}

		poster := ""
		if img := item.Find("img.swiper-lazy").First(); img.Length() > 0 {
			if src, ok := img.Attr("src"); ok {
				poster = p.baseURL + src // quirk: prefix even absolute srcs
			}
		}

		results = append(results, contracts.SearchResult{
			Title:    title,
			URL:      link,
			SourceID: p.ID(),
			Poster:   poster,
		})
	})
	return results, nil
}

// GetEpisodes follows the anime page iframe to the player page and its
// JSON playlist (port of sameband.py:48-81). Only the playlist decode is
// silent on failure (the Python bare except); page, player and playlist
// fetches stay loud.
func (p *SameBand) GetEpisodes(ctx context.Context, animeURL string) ([]contracts.Episode, error) {
	resp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    animeURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(resp.Body))
	if err != nil {
		return nil, contracts.WrapProvider(p.ID(), contracts.OpGetEpisodes, resp.StatusCode,
			fmt.Errorf("parse anime page: %w", err))
	}

	iframe := doc.Find(".player > .player-content > iframe[src]").First()
	if iframe.Length() == 0 {
		return []contracts.Episode{}, nil
	}

	playerURL, _ := iframe.Attr("src")
	if !strings.HasPrefix(playerURL, "http") {
		playerURL = p.baseURL + playerURL
	}

	playerResp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    playerURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	match := samebandFileRe.FindSubmatch(playerResp.Body)
	if match == nil {
		return []contracts.Episode{}, nil
	}

	playlistURL := string(match[1])
	if !strings.HasPrefix(playlistURL, "http") {
		playlistURL = p.baseURL + playlistURL
	}

	playlistResp, err := p.http.Do(ctx, netclient.Request{
		Method: "GET",
		URL:    playlistURL,
		Op:     contracts.OpGetEpisodes,
	})
	if err != nil {
		return nil, err
	}

	var playlist samebandPlaylist
	if jsonErr := json.Unmarshal(playlistResp.Body, &playlist); jsonErr != nil {
		return []contracts.Episode{}, nil
	}

	episodes := make([]contracts.Episode, 0, len(playlist))
	for i, item := range playlist {
		num := strconv.Itoa(i + 1)
		title := fmt.Sprintf("Episode %d", i+1)
		if item.Title != nil {
			title = *item.Title
		}
		episodes = append(episodes, contracts.Episode{
			Num:   num,
			Title: title,
			RawID: num,
			RawEmbeds: map[string][]string{
				"SameBand": {item.File},
			},
		})
	}
	return episodes, nil
}

// ResolveStream splits the raw file field on commas and maps each
// "[NNNp]<url>" part to a quality-keyed m3u8 source (port of
// sameband.py:83-96).
//
// Divergence from Python (task ruling, PR5): resolved sources carry the
// site root as their Referer so mpv can play the m3u8; the Python
// original left stream headers empty.
func (p *SameBand) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	stream := contracts.MediaStream{
		DubName: dubID,
		Links:   map[string]contracts.VideoSource{},
	}

	embeds := episode.RawEmbeds[dubID]
	if len(embeds) == 0 {
		return stream, nil
	}

	for _, part := range strings.Split(embeds[0], ",") {
		match := samebandQualityRe.FindStringSubmatch(part)
		if match == nil {
			continue
		}
		quality := match[1]
		src := match[2]
		if !strings.HasPrefix(src, "http") {
			src = p.baseURL + src
		}
		stream.Links[quality] = contracts.VideoSource{
			URL:     src,
			Quality: quality,
			Type:    "m3u8",
			Headers: map[string]string{"Referer": p.baseURL},
		}
	}
	return stream, nil
}
