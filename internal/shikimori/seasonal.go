package shikimori

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Seasonal surface of the Shikimori client (PR114): the current
// season's airing anime for the TUI seasonal calendar. The /api/animes
// list endpoint scopes by season (`season=<season>_<year>`, verified
// against the Rails controller's apipie docs: "summer_2017", "2016")
// and each row carries next_episode_at — for a weekly-airing title the
// next episode's weekday IS the broadcast day, which is the schedule
// info the calendar groups by.

// seasonalPageLimit is the /api/animes per-request row cap (the
// controller clamps limit to 50).
const seasonalPageLimit = 50

// maxSeasonalPages bounds the pager: the rate budget (1 rps / 5 rpm)
// makes long walks slow, and popularity order keeps the relevant
// titles inside the first pages.
const maxSeasonalPages = 3

// ValidSeason reports whether season is one of the four API season
// names.
func ValidSeason(season string) bool {
	switch season {
	case "winter", "spring", "summer", "fall":
		return true
	}
	return false
}

// SeasonalAnimes fetches one season's anime list, walking pages of 50
// (up to maxSeasonalPages) in popularity order. ongoingOnly adds the
// status=ongoing filter — right for the CURRENT season (the airing
// calendar), wrong for browsing past seasons (mostly released) and
// future ones (anounced), so callers pass it only for "now". Public
// read: works in every non-disabled mode.
func (c *Client) SeasonalAnimes(ctx context.Context, year int, season string, ongoingOnly bool) ([]Anime, error) {
	if err := c.requireMode(false); err != nil {
		return nil, err
	}
	if !ValidSeason(season) {
		return nil, fmt.Errorf("shikimori seasonal: unknown season %q (want winter|spring|summer|fall)", season)
	}
	if year < 1960 || year > 2100 {
		return nil, fmt.Errorf("shikimori seasonal: year %d out of range", year)
	}

	var out []Anime
	for page := 1; page <= maxSeasonalPages; page++ {
		query := url.Values{}
		query.Set("season", season+"_"+strconv.Itoa(year))
		query.Set("order", "popularity")
		query.Set("limit", strconv.Itoa(seasonalPageLimit))
		query.Set("page", strconv.Itoa(page))
		if ongoingOnly {
			query.Set("status", "ongoing")
		}

		resp, err := c.get(ctx, "/api/animes", query)
		if err != nil {
			return nil, fmt.Errorf("shikimori seasonal %s %d page %d: %w", season, year, page, err)
		}
		var rows []Anime
		if err := json.Unmarshal(resp.Body, &rows); err != nil {
			return nil, fmt.Errorf("shikimori seasonal %s %d page %d: decode: %w", season, year, page, err)
		}
		out = append(out, rows...)
		if len(rows) < seasonalPageLimit {
			return out, nil
		}
	}
	return out, nil
}

// NextEpisodeWeekday derives the broadcast weekday off the row's
// next_episode_at timestamp (RFC3339). For a weekly-airing title the
// next episode airs on the title's broadcast day; false when the row
// carries no (parsable) timestamp — finished-in-season and not-yet
// airing titles land in the calendar's unscheduled group.
func (a *Anime) NextEpisodeWeekday() (time.Weekday, bool) {
	raw := strings.TrimSpace(a.NextEpisodeAt)
	if raw == "" {
		return time.Sunday, false
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Sunday, false
	}
	return ts.Weekday(), true
}
