package mal

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// TestParseBroadcastDay maps the MAL wire vocabulary (lower-cased day
// names of BroadcastObject) onto weekdays; unknown values report
// false.
func TestParseBroadcastDay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		day  string
		want time.Weekday
		ok   bool
	}{
		{"monday", time.Monday, true},
		{"tuesday", time.Tuesday, true},
		{"wednesday", time.Wednesday, true},
		{"thursday", time.Thursday, true},
		{"friday", time.Friday, true},
		{"saturday", time.Saturday, true},
		{"sunday", time.Sunday, true},
		{"", time.Sunday, false},
		{"Mondays", time.Sunday, false},
		{"funday", time.Sunday, false},
	}
	for _, tc := range cases {
		got, ok := ParseBroadcastDay(tc.day)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("ParseBroadcastDay(%q) = (%v, %v), want (%v, %v)",
				tc.day, got, ok, tc.want, tc.ok)
		}
	}
}

// seasonalFixture is one wire-shaped node of /v2/anime/season.
func seasonalFixture(id int64, day string) map[string]any {
	node := map[string]any{
		"id":           id,
		"title":        "Wire Title",
		"mean":         8.42,
		"num_episodes": 12,
		"status":       "currently_airing",
		"broadcast":    map[string]any{"day_of_the_week": day, "start_time": "22:30"},
	}
	if day == "" {
		node["broadcast"] = map[string]any{}
	}
	return map[string]any{"node": node}
}

// TestSeasonalAnimeFetch pins the wire contract (PR114): bearer GET
// /v2/anime/season/{year}/{season} with the fields set including
// broadcast, parsed into the flat seasonal node shape.
func TestSeasonalAnimeFetch(t *testing.T) {
	t.Parallel()

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/anime/season/2026/fall" {
			t.Errorf("path = %q, want /v2/anime/season/2026/fall", r.URL.Path)
		}
		q := r.URL.Query()
		fields := "," + q.Get("fields") + ","
		for _, want := range []string{"broadcast", "mean", "num_episodes", "status"} {
			if !strings.Contains(fields, ","+want+",") {
				t.Errorf("fields %q must contain %q", q.Get("fields"), want)
			}
		}
		if got := q.Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer at-old" {
			t.Errorf("authorization = %q, want bearer", auth)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": []any{
				seasonalFixture(1, "monday"),
				seasonalFixture(2, "thursday"),
				seasonalFixture(3, ""),
			},
			"paging": map[string]any{},
		})
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	nodes, err := c.SeasonalAnime(context.Background(), 2026, "fall", 100)
	if err != nil {
		t.Fatalf("SeasonalAnime: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(nodes))
	}
	if !nodes[0].HasDay || nodes[0].Day != time.Monday {
		t.Errorf("node0 day = (%v,%v), want monday", nodes[0].Day, nodes[0].HasDay)
	}
	if !nodes[1].HasDay || nodes[1].Day != time.Thursday {
		t.Errorf("node1 day = (%v,%v), want thursday", nodes[1].Day, nodes[1].HasDay)
	}
	if nodes[2].HasDay {
		t.Errorf("node2 must carry no broadcast day")
	}
	if nodes[0].Mean != 8.42 || nodes[0].NumEpisodes != 12 || nodes[0].Status != "currently_airing" {
		t.Errorf("node0 scalar fields drifted: %+v", nodes[0])
	}
	if nodes[0].StartTime != "22:30" {
		t.Errorf("node0 start_time = %q, want 22:30", nodes[0].StartTime)
	}
	if nodes[0].ID != 1 || nodes[0].Title != "Wire Title" {
		t.Errorf("node0 identity drifted: %+v", nodes[0])
	}
}

// TestSeasonalAnimeValidation: unknown seasons and absurd years fail
// before any request.
func TestSeasonalAnimeValidation(t *testing.T) {
	t.Parallel()

	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may leave for an invalid season")
	})
	c := newTestClient(t, bearerCfg(), api, nil)

	if _, err := c.SeasonalAnime(context.Background(), 2026, "autumn", 100); err == nil {
		t.Fatal("season \"autumn\" must be rejected")
	}
	if _, err := c.SeasonalAnime(context.Background(), 9999, "fall", 100); err == nil {
		t.Fatal("year 9999 must be rejected")
	}
}

// TestSeasonalAnimeUnauthenticated: without credentials the seasonal
// read surfaces ErrAuthRequired (MAL has no anonymous access).
func TestSeasonalAnimeUnauthenticated(t *testing.T) {
	t.Parallel()

	c := newTestClient(t, config.MAL{Enabled: true}, nil, nil)
	if _, err := c.SeasonalAnime(context.Background(), 2026, "fall", 100); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
}
