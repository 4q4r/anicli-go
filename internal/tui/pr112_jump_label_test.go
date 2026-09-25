package tui

// PR112: the «🔢 Перейти к серии» jump list duplicated the episode
// label — every row read «Серия 1 — Серия 1», because RU providers
// fill contracts.Episode.Title with the player-API episode title,
// which is already «Серия N». A title that merely restates the row's
// own «Серия N» prefix must render once («Серия 3»); a real distinct
// title keeps today's «Серия N — Title» form; an empty title keeps
// the bare row. The dedupe applies to the jump list only — the
// episode screen header keeps its own format.

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// pr112Session loads a session whose episodes carry every label
// shape: a «Серия N» echo (RU providers), a real distinct title, an
// empty title, and a whitespace-padded echo (normalized match,
// multi-digit).
func pr112Session(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{Episode: &fakeEpisode{episodes: map[string][]contracts.Episode{
		"animego": {
			{Num: "3", Title: "Серия 3", RawID: "a3", RawEmbeds: map[string][]string{"Дубль 1": {"u3"}}},
			{Num: "5", Title: "Опенинг", RawID: "a5", RawEmbeds: map[string][]string{"Дубль 1": {"u5"}}},
			{Num: "7", Title: "", RawID: "a7", RawEmbeds: map[string][]string{"Дубль 1": {"u7"}}},
			{Num: "12", Title: "  Серия   12  ", RawID: "a12", RawEmbeds: map[string][]string{"Дубль 1": {"u12"}}},
		},
	}}}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	return s
}

// pr112Label returns the jump-list label of one episode.
func pr112Label(t *testing.T, s *sessionScreen, num string) string {
	t.Helper()
	for _, ch := range s.episodeChoices() {
		if ch.ID == num {
			return ch.Label
		}
	}
	t.Fatalf("jump list has no episode %q", num)
	return ""
}

func TestPR112JumpLabelSeriesTitleDedupe(t *testing.T) {
	s := pr112Session(t)
	if got := pr112Label(t, s, "3"); got != "Серия 3" {
		t.Fatalf("an echoed «Серия N» title must render once, got %q", got)
	}
	if got := pr112Label(t, s, "5"); got != "Серия 5 — Опенинг" {
		t.Fatalf("a distinct title must keep the dash form, got %q", got)
	}
	if got := pr112Label(t, s, "7"); got != "Серия 7" {
		t.Fatalf("an empty title must render the bare row, got %q", got)
	}
	if got := pr112Label(t, s, "12"); got != "Серия 12" {
		t.Fatalf("a whitespace-padded echo must normalize to a single label, got %q", got)
	}
}

func TestPR112JumpListViewHasNoDuplicate(t *testing.T) {
	s := pr112Session(t)
	s.buildEpisodeList()
	v := s.episodeList.Render()
	if strings.Contains(v, "Серия 3 — Серия 3") {
		t.Fatalf("jump list must not duplicate the episode label:\n%s", v)
	}
	if !strings.Contains(v, "Серия 5 — Опенинг") {
		t.Fatalf("jump list must keep distinct titles:\n%s", v)
	}
}
