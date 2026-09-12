package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// cdnVideoHubAPIBase is the playlist/video API root
// (extractors.py:281, 302).
const cdnVideoHubAPIBase = "https://plapi.cdnvideohub.com"

// cdnVideoHubExtractor ports CdnVideoHubExtractor (extractors.py:258-325),
// the animego cdn-iframe player.
type cdnVideoHubExtractor struct {
	http    *netclient.Client
	apiBase string
}

var (
	cdnTitleIDRe = regexp.MustCompile(`data-title-id="(\d+)"`)
	cdnPubIDRe   = regexp.MustCompile(`data-publisher-id="(\d+)"`)
	cdnAggrRe    = regexp.MustCompile(`data-aggregator="([^"]+)"`)
)

// Name identifies the extractor.
func (e *cdnVideoHubExtractor) Name() string { return "cdnvideohub" }

// Matches ports the Python URL gate (extractors.py:261).
func (e *cdnVideoHubExtractor) Matches(u string) bool {
	return strings.Contains(u, "cdn-iframe")
}

// Extract resolves an animego cdn-iframe URL through the cdnvideohub
// playlist APIs.
func (e *cdnVideoHubExtractor) Extract(ctx context.Context, rawURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:cdnvideohub: %w: %s", contracts.ErrExtractFailed, reason)
	}

	resp, err := e.http.Get(ctx, rawURL, map[string]string{"Referer": "https://animego.org/"})
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	html := string(resp.Body)

	titleID := cdnTitleIDRe.FindStringSubmatch(html)
	pubID := cdnPubIDRe.FindStringSubmatch(html)
	aggr := cdnAggrRe.FindStringSubmatch(html)
	if titleID == nil || pubID == nil || aggr == nil {
		return nil, shape("iframe page missing title-id/publisher-id/aggregator")
	}

	parts := strings.Split(afterLast(rawURL, "cdn-iframe/"), "/")
	if len(parts) < 4 {
		return nil, shape("cdn-iframe path lacks dubber/season/episode parts")
	}
	dubber, err := url.PathUnescape(parts[1])
	if err != nil {
		return nil, shape(fmt.Sprintf("unquote dubber: %v", err))
	}
	season, err1 := strconv.Atoi(parts[2])
	episode, err2 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil {
		return nil, shape("non-numeric season/episode path parts")
	}

	params := url.Values{}
	params.Set("pub", pubID[1])
	params.Set("aggr", aggr[1])
	params.Set("id", titleID[1])
	plResp, err := e.http.Get(ctx, e.apiBase+"/api/v1/player/sv/playlist?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	var plData struct {
		Items []struct {
			Episode     json.Number `json:"episode"`
			Season      json.Number `json:"season"`
			VoiceStudio string      `json:"voiceStudio"`
			VkID        string      `json:"vkId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(plResp.Body, &plData); err != nil {
		return nil, shape(fmt.Sprintf("playlist json: %v", err))
	}

	vkID := ""
	for _, item := range plData.Items {
		if item.Episode.String() == strconv.Itoa(episode) &&
			item.Season.String() == strconv.Itoa(season) &&
			item.VoiceStudio == dubber {
			vkID = item.VkID
			break
		}
	}
	if vkID == "" {
		// Python returns {} without an exception here
		// (extractors.py:299-300): a legitimate empty outcome.
		return map[string]contracts.VideoSource{}, nil
	}

	vidResp, err := e.http.Get(ctx, e.apiBase+"/api/v1/player/sv/video/"+vkID, nil)
	if err != nil {
		return nil, fmt.Errorf("extractor:cdnvideohub: %w", err)
	}
	var vidData struct {
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(vidResp.Body, &vidData); err != nil {
		return nil, shape(fmt.Sprintf("video json: %v", err))
	}

	// Quality map (extractors.py:309-314).
	qualityMap := map[string]int{
		"mpegTinyUrl": 144, "mpegLowestUrl": 240, "mpegLowUrl": 360,
		"mpegMediumUrl": 480, "mpegHighUrl": 720, "mpegFullHdUrl": 1080,
		"mpegQhdUrl": 1440, "mpeg4kUrl": 2160,
		"hlsUrl": 1080, "dashUrl": 1080,
	}
	results := map[string]contracts.VideoSource{}
	for key, link := range vidData.Sources {
		if link == "" {
			continue
		}
		if q, ok := qualityMap[key]; ok {
			qs := strconv.Itoa(q)
			results[qs] = contracts.VideoSource{URL: link, Quality: qs}
		}
	}
	return results, nil
}

// afterLast ports url.split(marker)[-1] for the cdn-iframe path split.
func afterLast(s, marker string) string {
	if i := strings.LastIndex(s, marker); i >= 0 {
		return s[i+len(marker):]
	}
	return s
}
