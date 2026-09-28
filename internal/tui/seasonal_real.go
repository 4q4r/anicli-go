package tui

// Production SeasonalService adapter (PR114): Shikimori first (public
// read, works in every non-disabled mode), MyAnimeList as the
// fallback when the user completed the MAL OAuth setup. The wire
// clients hang behind narrow function fields so the dispatch and the
// row mappings stay testable without transports.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/mal"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// realSeasonal adapts the two wire clients onto SeasonalService. A
// nil fetch func means "this leg is unavailable" (disabled /
// unauthenticated).
type realSeasonal struct {
	shikiFetch func(ctx context.Context, year int, season string, ongoingOnly bool) ([]shikimori.Anime, error)
	malFetch   func(ctx context.Context, year int, season string, limit int) ([]mal.SeasonalNode, error)
	log        *slog.Logger
}

// newRealSeasonal wires the adapter over the live clients: the
// Shikimori leg rides every non-disabled mode (the seasonal read is
// public), the MAL leg only when its OAuth setup completed.
func newRealSeasonal(shiki *shikimori.Client, malc *mal.Client, log *slog.Logger) realSeasonal {
	s := realSeasonal{log: log}
	if shiki != nil && shiki.Mode() != "disabled" {
		s.shikiFetch = shiki.SeasonalAnimes
	}
	if malc != nil && malc.Authenticated() {
		s.malFetch = malc.SeasonalAnime
	}
	return s
}

// Season implements SeasonalService: Shikimori when enabled, MAL as
// the authenticated fallback, an honest combined verdict when both
// fail and an explicit no-source error when neither is available.
func (s realSeasonal) Season(ctx context.Context, year int, season string, ongoingOnly bool) ([]SeasonalRow, error) {
	var shikiErr error
	if s.shikiFetch != nil {
		animes, err := s.shikiFetch(ctx, year, season, ongoingOnly)
		if err == nil {
			rows := make([]SeasonalRow, 0, len(animes))
			for i := range animes {
				rows = append(rows, shikimoriSeasonalRow(&animes[i]))
			}
			return rows, nil
		}
		shikiErr = err
		if s.log != nil {
			s.log.Warn("seasonal: shikimori fetch failed, trying the MAL fallback", "error", err)
		}
	}

	if s.malFetch != nil {
		nodes, err := s.malFetch(ctx, year, season, 100)
		if err == nil {
			rows := make([]SeasonalRow, 0, len(nodes))
			for _, n := range nodes {
				if ongoingOnly && n.Status != "currently_airing" {
					continue
				}
				rows = append(rows, malSeasonalRow(n))
			}
			return rows, nil
		}
		if shikiErr != nil {
			return nil, fmt.Errorf("seasonal: %w", errors.Join(shikiErr, err))
		}
		return nil, fmt.Errorf("mal: %w", err)
	}

	if shikiErr != nil {
		return nil, shikiErr
	}
	return nil, errors.New("seasonal: no source available (shikimori disabled, MAL not authorized)")
}

// shikimoriSeasonalRow maps one Shikimori anime row: russian title
// preferred (the provider roster is RU-first) with the en fallback,
// the zero score dropped, the broadcast weekday derived from
// next_episode_at.
func shikimoriSeasonalRow(a *shikimori.Anime) SeasonalRow {
	title := strings.TrimSpace(a.Russian)
	if title == "" {
		title = strings.TrimSpace(a.Name)
	}
	score := strings.TrimSpace(a.Score.String())
	if score == "0" {
		score = ""
	}
	wd, hasDay := a.NextEpisodeWeekday()
	return SeasonalRow{
		ID:            strconv.FormatInt(a.ID, 10),
		Title:         title,
		EpisodesAired: a.EpisodesAired,
		Episodes:      a.Episodes,
		Score:         score,
		Weekday:       wd,
		HasWeekday:    hasDay,
	}
}

// malSeasonalRow maps one MAL seasonal node: the wire carries no
// aired counter, the mean formats at two decimals, the broadcast
// weekday arrives pre-parsed.
func malSeasonalRow(n mal.SeasonalNode) SeasonalRow {
	score := ""
	if n.Mean > 0 {
		score = strconv.FormatFloat(n.Mean, 'f', 2, 64)
	}
	return SeasonalRow{
		ID:         "mal:" + strconv.FormatInt(n.ID, 10),
		Title:      n.Title,
		Episodes:   n.NumEpisodes,
		Score:      score,
		Weekday:    n.Day,
		HasWeekday: n.HasDay,
	}
}
