package mal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Seasonal surface of the MAL client (PR114): the fallback source of
// the TUI seasonal calendar. GET /v2/anime/season/{year}/{season}
// (verified against the official apiconfig references) returns
// data[].node rows whose BroadcastObject carries
// day_of_the_week (a lower-cased day name, e.g. "thursday") and
// start_time ("19:30") — the exact airing schedule the calendar groups
// by. The endpoint requires a bearer token, so this path only serves
// users who completed the MAL setup.

// SeasonalNode is one flattened /v2/anime/season row.
type SeasonalNode struct {
	ID    int64
	Title string
	// Mean is the weighted score (0 when unscored).
	Mean float64
	// NumEpisodes is the planned episode count (0 = unknown/TBA).
	NumEpisodes int
	// Status is the airing status ("currently_airing",
	// "finished_airing", "not_yet_aired", "cancelled").
	Status string
	// Day/HasDay carry the broadcast weekday; HasDay is false when the
	// row's broadcast object is empty.
	Day    time.Weekday
	HasDay bool
	// StartTime is the broadcast time of day ("22:30"; "" unknown).
	StartTime string
}

// broadcastObject mirrors the wire BroadcastObject.
type broadcastObject struct {
	DayOfWeek string `json:"day_of_the_week"`
	StartTime string `json:"start_time"`
}

// seasonalWire is the reply envelope of /v2/anime/season.
type seasonalWire struct {
	Data []struct {
		Node struct {
			ID          int64           `json:"id"`
			Title       string          `json:"title"`
			Mean        float64         `json:"mean"`
			NumEpisodes int             `json:"num_episodes"`
			Status      string          `json:"status"`
			Broadcast   broadcastObject `json:"broadcast"`
		} `json:"node"`
	} `json:"data"`
}

// ParseBroadcastDay maps the wire day name onto a weekday; false for
// unknown names.
func ParseBroadcastDay(day string) (time.Weekday, bool) {
	switch day {
	case "monday":
		return time.Monday, true
	case "tuesday":
		return time.Tuesday, true
	case "wednesday":
		return time.Wednesday, true
	case "thursday":
		return time.Thursday, true
	case "friday":
		return time.Friday, true
	case "saturday":
		return time.Saturday, true
	case "sunday":
		return time.Sunday, true
	}
	return time.Sunday, false
}

// SeasonalAnime fetches one season's anime from
// /v2/anime/season/{year}/{season} with the fields the calendar needs.
// limit is clamped to [1, 500] (the API's maximum); the call is a
// single page — the fallback path favors one cheap request over full
// pagination.
func (c *Client) SeasonalAnime(ctx context.Context, year int, season string, limit int) ([]SeasonalNode, error) {
	if err := c.requireMode(); err != nil {
		return nil, err
	}
	switch season {
	case "winter", "spring", "summer", "fall":
	default:
		return nil, fmt.Errorf("mal seasonal: unknown season %q (want winter|spring|summer|fall)", season)
	}
	if year < 1960 || year > 2100 {
		return nil, fmt.Errorf("mal seasonal: year %d out of range", year)
	}
	limit = min(max(limit, 1), 500)

	q := url.Values{}
	q.Set("fields", "id,title,mean,num_episodes,status,broadcast")
	q.Set("limit", strconv.Itoa(limit))
	body, err := c.apiCall(ctx, http.MethodGet,
		fmt.Sprintf("%s/v2/anime/season/%d/%s?%s", c.apiBase, year, season, q.Encode()), nil)
	if err != nil {
		return nil, fmt.Errorf("mal seasonal %s %d: %w", season, year, err)
	}

	var wire seasonalWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("mal seasonal %s %d: decode: %w", season, year, err)
	}
	nodes := make([]SeasonalNode, 0, len(wire.Data))
	for _, row := range wire.Data {
		node := SeasonalNode{
			ID:          row.Node.ID,
			Title:       row.Node.Title,
			Mean:        row.Node.Mean,
			NumEpisodes: row.Node.NumEpisodes,
			Status:      row.Node.Status,
			StartTime:   row.Node.Broadcast.StartTime,
		}
		node.Day, node.HasDay = ParseBroadcastDay(row.Node.Broadcast.DayOfWeek)
		nodes = append(nodes, node)
	}
	return nodes, nil
}
