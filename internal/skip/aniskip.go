package skip

import (
	"context"
	"encoding/json"
	"fmt"
)

// DefaultAniSkipBaseURL is the AniSkip v2 API root.
const DefaultAniSkipBaseURL = "https://api.aniskip.com"

// DefaultAniSkipProviderName is the submit attribution reported to
// AniSkip. The python default ("anicli-py-audio-predictor") referenced
// the removed ML predictor; the Go port attributes honestly.
const DefaultAniSkipProviderName = "anicli-go"

// AniSkipOptions customizes the AniSkip v2 client.
type AniSkipOptions struct {
	// BaseURL overrides the API root (tests point this at httptest).
	BaseURL string
	// SubmitterID is the contributor identity; empty disables submits
	// (python guard: `if not self.cfg.submitter_id: return`).
	SubmitterID string
	// ProviderName is the submission attribution; empty uses the
	// default.
	ProviderName string
}

// AniSkipClient queries and contributes skip times on
// api.aniskip.com/v2 (ported from anicli-py anicli/core/aniskip.py,
// API v1 -> v2 per the PR9 ruling).
type AniSkipClient struct {
	http HTTP
	opts AniSkipOptions
}

// NewAniSkipClient builds the client; opts zero values fall back to the
// defaults.
func NewAniSkipClient(http HTTP, opts AniSkipOptions) *AniSkipClient {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultAniSkipBaseURL
	}
	if opts.ProviderName == "" {
		opts.ProviderName = DefaultAniSkipProviderName
	}
	return &AniSkipClient{http: http, opts: opts}
}

// ID implements Provider.
func (c *AniSkipClient) ID() string { return ProviderAniSkip }

// aniskipResponse is the v2 GET payload shape (python _fetch_from_api).
type aniskipResponse struct {
	Found   bool                `json:"found"`
	Results []aniskipResultItem `json:"results"`
}

// aniskipResultItem is one skip-time result. The live v2 API answers
// camelCase (startTime/endTime/skipType/skipId/episodeLength —
// verified against api.aniskip.com 2026-09-18; the snake_case shape
// was v1 and silently decoded to zero intervals).
type aniskipResultItem struct {
	Interval      aniskipInterval `json:"interval"`
	SkipType      string          `json:"skipType"`
	SkipID        string          `json:"skipId"`
	EpisodeLength float64         `json:"episodeLength"`
}

// aniskipInterval carries the start/end seconds.
type aniskipInterval struct {
	StartTime float64 `json:"startTime"`
	EndTime   float64 `json:"endTime"`
}

// aniskipSubmitPayload is the six-field submit body (skipType,
// providerName, startTime, endTime, episodeLength, submitterId — v2
// camelCase, live-verified).
type aniskipSubmitPayload struct {
	SkipType      string  `json:"skipType"`
	ProviderName  string  `json:"providerName"`
	StartTime     float64 `json:"startTime"`
	EndTime       float64 `json:"endTime"`
	EpisodeLength float64 `json:"episodeLength"`
	SubmitterID   string  `json:"submitterId"`
}

// GetSkipTimes fetches the op/ed skip times of one episode. A clean
// found=false answers as an empty slice with a nil error; transport and
// HTTP failures propagate as errors.
//
// PR61 live finding: the v2 API requires the episodeLength query
// parameter — requests without it answer HTTP 400 for every anime.
// The fetch happens before playback, so the length is unknown: the
// neutral 0 is sent (accepted by the API) and each result carries the
// authoritative episode_length.
func (c *AniSkipClient) GetSkipTimes(ctx context.Context, shikimoriID int64, episodeNum float64) ([]Interval, error) {
	url := fmt.Sprintf("%s/v2/skip-times/%d/%s?types=op&types=ed&episodeLength=0",
		c.opts.BaseURL, shikimoriID, normalizeEpisodeNum(episodeNum))

	resp, err := c.http.Get(ctx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("aniskip get skip times (%d, %v): %w", shikimoriID, episodeNum, err)
	}

	var payload aniskipResponse
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, fmt.Errorf("aniskip decode response (%d, %v): %w", shikimoriID, episodeNum, err)
	}
	if !payload.Found {
		return []Interval{}, nil
	}

	intervals := make([]Interval, 0, len(payload.Results))
	for _, item := range payload.Results {
		intervals = append(intervals, Interval{
			SkipType:      item.SkipType,
			StartTime:     item.Interval.StartTime,
			EndTime:       item.Interval.EndTime,
			EpisodeLength: item.EpisodeLength,
		})
	}
	return intervals, nil
}

// SubmitSkipTime contributes one op/ed skip time. Guards mirror the
// python flow: an unconfigured submitter id is a deliberate no-op; a
// type outside op/ed fails with ErrUnsupportedSkipType. Unlike python,
// transport errors are returned instead of swallowed so callers decide
// how loudly to surface them.
func (c *AniSkipClient) SubmitSkipTime(ctx context.Context, req SubmitRequest) error {
	if c.opts.SubmitterID == "" {
		return nil
	}

	supported := false
	for _, t := range APISupportedTypes {
		if req.SkipType == t {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("%w: %q", ErrUnsupportedSkipType, req.SkipType)
	}

	url := fmt.Sprintf("%s/v2/skip-times/%d/%s",
		c.opts.BaseURL, req.ShikimoriID, normalizeEpisodeNum(req.EpisodeNum))

	body := aniskipSubmitPayload{
		SkipType:      req.SkipType,
		ProviderName:  c.opts.ProviderName,
		StartTime:     req.StartTime,
		EndTime:       req.EndTime,
		EpisodeLength: req.EpisodeLength,
		SubmitterID:   c.opts.SubmitterID,
	}

	if _, err := c.http.PostJSON(ctx, url, body, nil); err != nil {
		return fmt.Errorf("aniskip submit skip time (%d, %v, %s): %w",
			req.ShikimoriID, req.EpisodeNum, req.SkipType, err)
	}
	return nil
}
