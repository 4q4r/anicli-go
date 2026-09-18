package extractors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// bloggerBatchExecuteAPIBase is the Blogger video-player data RPC the
// modern WIZ/boq player drives [LIVE-VERIFIED 2026-09-18]. The embed
// page itself (https://www.blogger.com/video.g?token=…&origin=…)
// carries NO inline playback config — the old
// googleusercontent/videoplayer-config blob moved into this RPC. The
// minimal parameter set was verified live: only rpcids and rt are
// required, no f.sid, no cookies, no browser JS.
const bloggerBatchExecuteAPIBase = "https://www.blogger.com/_/BloggerVideoPlayerUi/data/batchexecute"

// bloggerRPCID is the WIZ rpc id of the playback-data call (captured
// from the live player's own request, 2026-09-18).
const bloggerRPCID = "WcwnYd"

// bloggerExtractor resolves blogger.com/video.g embed URLs — the only
// live mirror family anitaku.io (gogoanime) still serves (PR53 owner
// ruling, parity-breaking addition). The token from the embed URL is
// replayed against the player's batchexecute RPC; the answer carries a
// youtube-style streamingData JSON whose formats[] are PROGRESSIVE mp4
// URLs (signed, IP-locked to the fetching client, ~6h expiry, no DRM).
// Quality keys ride each format's own qualityLabel ("720p" → "720"),
// never a hardcoded itag table.
//
// The playback URL must be fetched from the same network path this
// extractor used (the signature is bound to the client IP); the player
// plumbing streams from the same machine, so this holds by design.
type bloggerExtractor struct {
	http *netclient.Client
	// batchExecuteURL overrides the RPC endpoint (tests); "" keeps the
	// production default.
	batchExecuteURL string
}

// Name identifies the extractor.
func (e *bloggerExtractor) Name() string { return "blogger" }

// Matches gates the blogger video embed shape the anitaku mirrors emit.
func (e *bloggerExtractor) Matches(u string) bool { return strings.Contains(u, "blogger.com/video.g") }

// bloggerFormat is one streamingData.formats entry (youtube itag
// semantics; the live captures carry the progressive mp4 pair itag 22
// = 720p, itag 18 = 360p).
type bloggerFormat struct {
	// Itag documents the wire shape (and anchors itag semantics in the
	// comment above); quality keys ride QualityLabel, never this field.
	Itag         int    `json:"itag"`
	URL          string `json:"url"`
	MimeType     string `json:"mimeType"`
	QualityLabel string `json:"qualityLabel"`
}

// Extract replays the player RPC and collects the progressive mp4
// formats. Shape mismatches surface as typed errors (the repo's
// no-silent-failure replacement for Python's swallowed exceptions).
func (e *bloggerExtractor) Extract(ctx context.Context, embedURL string) (map[string]contracts.VideoSource, error) {
	shape := func(reason string) error {
		return fmt.Errorf("extractor:blogger: %w: %s", contracts.ErrExtractFailed, reason)
	}

	parsed, err := url.Parse(embedURL)
	if err != nil {
		return nil, shape(fmt.Sprintf("unparseable embed url: %v", err))
	}
	token := parsed.Query().Get("token")
	if token == "" {
		return nil, shape("no token on the embed url")
	}

	endpoint := e.batchExecuteURL
	if endpoint == "" {
		endpoint = bloggerBatchExecuteAPIBase
	}

	// f.req=[[["WcwnYd","[\"<token>\"]",null,"generic"]]] — the second
	// element is a JSON-encoded string carrying the JSON args array
	// (verbatim shape of the live player's request).
	args, err := json.Marshal([]string{token})
	if err != nil {
		return nil, shape(fmt.Sprintf("encode token args: %v", err))
	}
	freq := "[[[" + strconv.Quote(bloggerRPCID) + "," + strconv.Quote(string(args)) + `,null,"generic"]]]`

	resp, err := e.http.Do(ctx, netclient.Request{
		Method: "POST",
		URL:    endpoint + "?rpcids=" + bloggerRPCID + "&rt=c",
		Headers: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
		},
		Body: strings.NewReader(url.Values{"f.req": {freq}}.Encode()),
		Op:   contracts.OpResolveStream,
	})
	if err != nil {
		return nil, fmt.Errorf("extractor:blogger: %w", err)
	}

	payload, err := bloggerPayload(resp.Body)
	if err != nil {
		return nil, shape(err.Error())
	}

	sources := map[string]contracts.VideoSource{}
	for _, f := range payload.StreamingData.Formats {
		if f.URL == "" || !strings.Contains(f.MimeType, "mp4") || f.QualityLabel == "" {
			continue // adaptive/audio-only/label-less formats are not the progressive mp4s
		}
		quality := strings.TrimSuffix(f.QualityLabel, "p")
		sources[quality] = contracts.VideoSource{
			URL:     f.URL,
			Quality: quality,
			Type:    "mp4",
		}
	}
	if len(sources) == 0 {
		return nil, shape("no progressive mp4 formats in the player payload")
	}
	return sources, nil
}

// bloggerPayload digs the streamingData JSON out of a batchexecute
// response: an )]}'-prefixed body of length-prefixed JSON rows, the
// wrb.fr row holding the payload string; one element of that payload
// string array is a JSON object with the streaming data. Preamble
// tolerance matters: the fixture (and the live answer) carries length
// lines and comments, and future row orders must not break the parse.
type bloggerStreamData struct {
	StreamingData struct {
		Formats []bloggerFormat `json:"formats"`
	} `json:"streamingData"`
}

func bloggerPayload(body []byte) (*bloggerStreamData, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		var rows [][]json.RawMessage
		if err := json.Unmarshal([]byte(line), &rows); err != nil {
			continue // length lines and non-JSON preamble
		}
		for _, row := range rows {
			if len(row) < 3 {
				continue
			}
			var kind string
			if err := json.Unmarshal(row[0], &kind); err != nil || kind != "wrb.fr" {
				continue
			}
			var payloadStr string
			if err := json.Unmarshal(row[2], &payloadStr); err != nil {
				continue
			}
			if out := bloggerPayloadFromString(payloadStr); out != nil {
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("no %s payload row in the rpc answer", bloggerRPCID)
}

// bloggerPayloadFromString finds the streamingData object among the
// payload string's array elements — position-independent, so WIZ
// payload reshuffles cannot strand the extractor.
func bloggerPayloadFromString(payload string) *bloggerStreamData {
	var elements []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &elements); err != nil {
		return nil
	}
	for _, element := range elements {
		var s string
		if err := json.Unmarshal(element, &s); err != nil {
			continue
		}
		if !strings.Contains(s, `"streamingData"`) {
			continue
		}
		out := &bloggerStreamData{}
		if err := json.Unmarshal([]byte(s), out); err == nil {
			return out
		}
	}
	return nil
}
