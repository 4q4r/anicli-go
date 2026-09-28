package shikimori

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// seasonalRows builds n synthetic anime rows with alternating weekdays
// and ru titles.
func seasonalRows(n, startID int) []Anime {
	rows := make([]Anime, 0, n)
	for i := range n {
		id := int64(startID + i)
		rows = append(rows, Anime{
			ID:            id,
			Name:          fmt.Sprintf("Anime %d", id),
			Russian:       fmt.Sprintf("Аниме %d", id),
			Episodes:      12,
			EpisodesAired: 4,
			Status:        "ongoing",
			Kind:          "tv",
			Score:         "8.5",
			NextEpisodeAt: time.Date(2026, time.October, 5+i%7, 12, 0, 0, 0, time.UTC).
				Format(time.RFC3339),
		})
	}
	return rows
}

// TestSeasonalAnimesQueries pins the wire contract of the seasonal
// fetch (PR114): season param is <season>_<year>, order=popularity,
// limit 50 per page, status=ongoing only when requested, and the pager
// walks until a short page.
func TestSeasonalAnimesQueries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		ongoingOnly bool
		wantStatus  string
	}{
		{name: "current season keeps ongoing filter", ongoingOnly: true, wantStatus: "ongoing"},
		{name: "browsing drops the status filter", ongoingOnly: false, wantStatus: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var pages []string
			c, log := newTestClient(t, config.Shikimori{Enabled: true},
				func(w http.ResponseWriter, r *http.Request) {
					q := r.URL.Query()
					if r.URL.Path != "/api/animes" {
						t.Errorf("path = %q, want /api/animes", r.URL.Path)
					}
					if got := q.Get("season"); got != "fall_2026" {
						t.Errorf("season = %q, want fall_2026", got)
					}
					if got := q.Get("order"); got != "popularity" {
						t.Errorf("order = %q, want popularity", got)
					}
					if got := q.Get("limit"); got != "50" {
						t.Errorf("limit = %q, want 50", got)
					}
					if got := q.Get("status"); got != tc.wantStatus {
						t.Errorf("status = %q, want %q", got, tc.wantStatus)
					}
					page := q.Get("page")
					pages = append(pages, page)
					count := 50
					if page == "2" {
						count = 10
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(seasonalRows(count, 1))
				})

			rows, err := c.SeasonalAnimes(context.Background(), 2026, "fall", tc.ongoingOnly)
			if err != nil {
				t.Fatalf("SeasonalAnimes: %v", err)
			}
			if len(rows) != 60 {
				t.Fatalf("rows = %d, want 60 (50 + 10)", len(rows))
			}
			if got := log.count("/api/animes"); got != 2 {
				t.Fatalf("requests = %d, want 2 pages", got)
			}
			if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
				t.Fatalf("pages walked = %v, want [1 2]", pages)
			}
		})
	}
}

// TestSeasonalAnimesPageCap: a full-length season stops at the page cap
// instead of walking forever.
func TestSeasonalAnimesPageCap(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{Enabled: true},
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(seasonalRows(50, 1))
		})

	rows, err := c.SeasonalAnimes(context.Background(), 2026, "summer", false)
	if err != nil {
		t.Fatalf("SeasonalAnimes: %v", err)
	}
	if len(rows) != maxSeasonalPages*50 {
		t.Fatalf("rows = %d, want %d (page cap)", len(rows), maxSeasonalPages*50)
	}
	if got := log.count("/api/animes"); got != maxSeasonalPages {
		t.Fatalf("requests = %d, want %d", got, maxSeasonalPages)
	}
}

// TestSeasonalAnimesValidation: unknown season names and absurd years
// fail before any request leaves the client.
func TestSeasonalAnimesValidation(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{Enabled: true}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may leave for an invalid season")
	})

	if _, err := c.SeasonalAnimes(context.Background(), 2026, "autumn", false); err == nil {
		t.Fatal("season \"autumn\" must be rejected")
	}
	if _, err := c.SeasonalAnimes(context.Background(), 0, "fall", false); err == nil {
		t.Fatal("year 0 must be rejected")
	}
	if got := log.count("/api/animes"); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

// TestSeasonalAnimesDisabled: the disabled integration rejects the read.
func TestSeasonalAnimesDisabled(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, config.Shikimori{}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("disabled client must not issue requests")
	})
	if _, err := c.SeasonalAnimes(context.Background(), 2026, "fall", true); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
}

// TestSeasonalAnimesEmptyPage: a season with no anime yields an empty
// list after one page.
func TestSeasonalAnimesEmptyPage(t *testing.T) {
	t.Parallel()

	c, log := newTestClient(t, config.Shikimori{Enabled: true},
		func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]Anime{})
		})
	rows, err := c.SeasonalAnimes(context.Background(), 2026, "winter", true)
	if err != nil {
		t.Fatalf("SeasonalAnimes: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
	if got := log.count("/api/animes"); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

// TestAnimeNextEpisodeWeekday: the broadcast-day derivation off the
// next_episode_at RFC3339 timestamp.
func TestAnimeNextEpisodeWeekday(t *testing.T) {
	t.Parallel()

	cases := []struct {
		at       string
		want     time.Weekday
		wantSeen bool
	}{
		// 2026-09-28 is a Monday.
		{"2026-09-28T15:30:00+09:00", time.Monday, true},
		{"2026-10-01T16:00:00Z", time.Thursday, true},
		{"", time.Sunday, false},
		{"garbage", time.Sunday, false},
	}
	for _, tc := range cases {
		a := Anime{NextEpisodeAt: tc.at}
		got, seen := a.NextEpisodeWeekday()
		if seen != tc.wantSeen || (seen && got != tc.want) {
			t.Errorf("NextEpisodeWeekday(%q) = (%v, %v), want (%v, %v)",
				tc.at, got, seen, tc.want, tc.wantSeen)
		}
	}
}
