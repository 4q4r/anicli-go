package tui

// The python resolve_dubs_smart parity on the remembered-dub resolve
// (fix-round 3 of #158): when the scoped resolve fails with the
// provider's typed «the episode does not carry this dub» verdict, the
// session prints the python warning («⚠ Прошлые настройки
// недоступны») and AUTO-OPENS the PR95 dub menu over the dubs the
// episode DOES carry — the user picks, playback continues. NEVER a
// silent substitution of another dub. Any OTHER failure class keeps
// the PR94 fail-soft fall-through (the merged resolve of the rest).

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// dubNotCarriedErr mirrors the classifyVMError wrap of the scripts'
// not_found dub wall (the animevib message shape, the carrier list
// riding the message).
func dubNotCarriedErr() error {
	return contracts.WrapProvider("animego", contracts.OpResolveStream, 0,
		fmt.Errorf("%w: anicli:not_found:episode 1 carries no dub \"Дубль 1\" (episode dubs: AniLib)",
			contracts.ErrNotFound))
}

// dubMissSession wires the two-source session with a typed not-carried
// failure on the remembered dub's resolve.
func dubMissSession(t *testing.T, streamErr error) (*sessionScreen, tea.Cmd) {
	t.Helper()
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: testEpisodeSet(),
			streams: map[string]contracts.MediaStream{
				"[anilib] AniLib": {DubName: "d2", Links: map[string]contracts.VideoSource{
					"1080": {URL: "a1080"},
				}},
			},
			streamErr: map[string]error{
				"[animego] Дубль 1": streamErr,
			},
		},
		Playback: &fakePlayback{},
	}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	s.setState(sessionStateMenu)
	s.buildActionMenu()
	_, cmd := s.beginStreamResolve("[animego] Дубль 1")
	if cmd == nil {
		t.Fatal("the scoped resolve must schedule a command")
	}
	return s, cmd
}

// TestScopedResolveDubMissWarnsAndOpensDubMenu pins the ask-don't-
// substitute flow: the typed not-carried failure surfaces the python
// warning and the dub menu over the episode's REMAINING dubs (the
// fresh hydration's linked keys), not a swapped dub and not the
// quality picker.
func TestScopedResolveDubMissWarnsAndOpensDubMenu(t *testing.T) {
	s, cmd := dubMissSession(t, dubNotCarriedErr())
	msg := cmd()
	_scr, _ := s.Update(msg)
	ss := _scr.(*sessionScreen)

	if ss.state != sessionStateRedub {
		t.Fatalf("state after the dub-miss settle = %v, want the dub menu", ss.state)
	}
	items := ss.redubList.Menu().Items
	// The episode carried two dubs; the missed one is freshly proven
	// absent — the ask offers the ONE remaining dub + Back.
	if len(items) != 2 {
		t.Fatalf("dub menu rows = %d (%v), want the remaining dub + Back",
			len(items), labelsOf(items))
	}
	values := make([]string, 0, len(items))
	for _, c := range items {
		if c.ID == BackID {
			continue
		}
		values = append(values, c.Value.(string))
	}
	// The episode's carriers: the missed dub's provider sibling and
	// the other source's dub — the fresh hydration's linked keys,
	// deterministic order. The MISSED key must NOT be offered (its
	// resolve just proved it absent).
	joined := strings.Join(values, "|")
	if !strings.Contains(joined, "[anilib] AniLib") {
		t.Fatalf("dub menu values = %v, want the carried dubs", values)
	}
	if strings.Contains(joined, "[animego] Дубль 1") {
		t.Fatalf("dub menu values = %v — the missed dub must not be offered", values)
	}
	if got := ss.status; !strings.Contains(got, "Previous settings unavailable") {
		t.Fatalf("status = %q, want the python warning", got)
	}
}

// TestScopedResolveOtherFailureKeepsFailSoft pins the boundary: a
// resolve failure that is NOT the typed dub-miss (transport, extract,
// any other class) keeps the PR94 fail-soft fall-through — the merged
// resolve of the rest — never the dub menu.
func TestScopedResolveOtherFailureKeepsFailSoft(t *testing.T) {
	s, cmd := dubMissSession(t, contracts.ErrAllCandidatesFailed)
	msg := cmd()
	_scr, _ := s.Update(msg)
	ss := _scr.(*sessionScreen)

	if ss.state == sessionStateRedub {
		t.Fatal("a non-dub-miss failure must not open the dub menu")
	}
}

// TestScopedResolveDubMissZeroDubsKeepsFailSoft pins the zero-dubs
// edge: the typed miss with NO other linked dubs on the episode has
// nothing to ask about — the honest fall-through stands.
func TestScopedResolveDubMissZeroDubsKeepsFailSoft(t *testing.T) {
	deps := &Deps{
		Episode: &fakeEpisode{
			episodes: map[string][]contracts.Episode{
				"animego": {
					{Num: "1", RawID: "a1", RawEmbeds: map[string][]string{
						"[animego] Дубль 1": nil, // tier-1 key, never hydrated
					}},
				},
			},
			streamErr: map[string]error{
				"[animego] Дубль 1": dubNotCarriedErr(),
			},
		},
		Playback: &fakePlayback{},
	}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()
	s.videoDub, s.audioDub = "[animego] Дубль 1", "[animego] Дубль 1"
	s.setState(sessionStateMenu)
	s.buildActionMenu()
	_, cmd := s.beginStreamResolve("[animego] Дубль 1")
	msg := cmd()
	_scr, _ := s.Update(msg)
	ss := _scr.(*sessionScreen)

	if ss.state == sessionStateRedub {
		t.Fatal("a dub miss with zero linked dubs must not open an empty ask")
	}
}
