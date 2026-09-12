// Package skip resolves anime skip times (OP/ED/recap chapters) from the
// AniSkip v2 REST API, the Anime Skip GraphQL API and local ffmpeg
// heuristics (IntroSkipper), merges them into one chapter set and renders
// FFMETADATA1 chapter files for mpv and ffmpeg.
//
// Ported from anicli-py anicli/core/aniskip.py and aniskip/skip_manager.py
// with the FEATURE_INVENTORY D rulings applied: the ML AudioPredictor is
// gone, so smart-merge only combines the API providers, and the manager
// orchestrates ordered fallback with per-provider toggles
// (config.Skip.ProvidersOrder over aniskip/anime_skip/intro_skipper).
package skip

import (
	"context"
	"errors"
	"fmt"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// Provider identifiers (python skip_manager _KNOWN).
const (
	ProviderAniSkip      = "aniskip"
	ProviderAnimeSkip    = "anime_skip"
	ProviderIntroSkipper = "intro_skipper"
)

// APISupportedTypes are the skip types the AniSkip remote strictly
// supports (python API_SUPPORTED_TYPES; recaps/mixed are not submitted).
var APISupportedTypes = []string{"op", "ed"}

// ErrUnsupportedSkipType reports a submit rejected because the remote
// only accepts op/ed.
var ErrUnsupportedSkipType = errors.New("skip type not supported by remote (want op or ed)")

// Interval is one normalized skip segment across all providers.
type Interval struct {
	// SkipType is the chapter type: op, ed, recap, preview, mixed-op...
	SkipType string
	// StartTime is the interval start in seconds.
	StartTime float64
	// EndTime is the interval end in seconds.
	EndTime float64
	// EpisodeLength is the full episode duration when the provider
	// reported one; 0 means unknown (FFMETADATA falls back to 1440s).
	EpisodeLength float64
}

// SubmitRequest is one skip-time submission: the six payload fields of
// the anicli-py submit flow (player_session -> AniSkipClient
// .submit_skip_time call shape).
type SubmitRequest struct {
	// ShikimoriID is the MyAnimeList/Shikimori anime id.
	ShikimoriID int64
	// EpisodeNum is the episode number.
	EpisodeNum float64
	// SkipType is strictly op or ed.
	SkipType string
	// StartTime is the skip start in seconds.
	StartTime float64
	// EndTime is the skip end in seconds.
	EndTime float64
	// EpisodeLength is the full episode duration in seconds.
	EpisodeLength float64
}

// HTTP is the subset of *netclient.Client the skip providers use.
// *netclient.Client satisfies it; tests substitute httptest-backed
// clients.
type HTTP interface {
	// Get performs a GET with optional extra headers.
	Get(ctx context.Context, url string, headers map[string]string) (*netclient.Response, error)
	// PostJSON marshals body as JSON and posts it.
	PostJSON(ctx context.Context, url string, body any, headers map[string]string) (*netclient.Response, error)
}

// Provider is one skip-time source the Manager can consult.
type Provider interface {
	// ID returns the stable provider key (aniskip, anime_skip,
	// intro_skipper).
	ID() string
}

// normalizeEpisodeNum renders an episode number for URL construction so
// integers appear as "1", never "1.0" (python _normalize_episode_num).
func normalizeEpisodeNum(episode float64) string {
	if episode == float64(int64(episode)) {
		return fmt.Sprintf("%d", int64(episode))
	}
	return fmt.Sprintf("%v", episode)
}
