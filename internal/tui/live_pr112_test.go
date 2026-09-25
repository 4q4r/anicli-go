//go:build live

package tui

// LIVE probe for PR112 (jump list — no duplicate episode label).
// Excluded from the hermetic default suite by the `live` build tag.
// Run manually:
//
//	go test -tags live -run TestLivePR112JumpList -count=1 -v ./internal/tui/
//
// Renders the «🔢 Перейти к серии» screen through the real
// sessionScreen View path for a payload shaped like the owner report:
// a RU provider whose player API echoes «Серия N» as the episode
// title for most episodes, plus one real distinct title and one
// empty title. The frame must show single labels («Серия 1») on the
// echo rows, keep «Серия N — Title» for the distinct title, and never
// print «Серия N — Серия N».

import (
	"fmt"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestLivePR112JumpList(t *testing.T) {
	eps := make([]contracts.Episode, 0, 12)
	for n := 1; n <= 12; n++ {
		num := fmt.Sprintf("%d", n)
		title := "Серия " + num // RU provider echo (the bug payload)
		if n == 5 {
			title = "Опенинг" // a real distinct title keeps the dash form
		}
		if n == 12 {
			title = "" // empty title keeps the bare row
		}
		eps = append(eps, contracts.Episode{
			Num:       num,
			Title:     title,
			RawID:     "raw" + num,
			RawEmbeds: map[string][]string{"Дубль 1": {"u" + num}},
		})
	}
	deps := &Deps{Episode: &fakeEpisode{episodes: map[string][]contracts.Episode{
		"animego": eps,
	}}}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	// The same transition a «🔢 Перейти к серии» pick performs.
	s.setState(sessionStateEpisodeList)
	s.buildEpisodeList()
	v := s.View().Content
	t.Logf("=== PR112 jump-list frame ===\n%s", pr110Indent(v))

	if strings.Contains(v, "Серия 1 — Серия 1") || strings.Contains(v, "— Серия ") {
		t.Fatalf("jump list must not duplicate the «Серия N» label:\n%s", v)
	}
	if !strings.Contains(v, "Серия 5 — Опенинг") {
		t.Fatalf("jump list must keep distinct titles:\n%s", v)
	}
	if strings.Count(v, "Серия 11") != 1 {
		t.Fatalf("echoed label must render exactly once:\n%s", v)
	}
}
