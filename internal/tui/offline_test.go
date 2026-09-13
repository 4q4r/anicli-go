package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/download"
)

// fakeOffline implements OfflineService.
type fakeOffline struct {
	titles []OfflineTitle
}

func (f *fakeOffline) Titles() ([]OfflineTitle, error) { return f.titles, nil }

var _ OfflineService = (*fakeOffline)(nil)

func offlineFixture() []OfflineTitle {
	return []OfflineTitle{
		{
			Dir:  "/dl/Ванпанчмен",
			Name: "Ванпанчмен",
			Snapshot: download.Snapshot{
				TitleDir: "/dl/Ванпанчмен",
				Entries: []download.Entry{
					{EpisodeNum: "1", VideoKey: "[a] dub", AudioKey: "[a] dub", Quality: 1080, RelativePath: "ep1.mkv"},
					{EpisodeNum: "1", VideoKey: "[a] dub", AudioKey: "[b] sub", Quality: 720, RelativePath: "ep1b.mkv"},
					{EpisodeNum: "2", VideoKey: "[a] dub", AudioKey: "[a] dub", Quality: 1080, RelativePath: "ep2.mkv"},
				},
			},
		},
	}
}

// TestOfflineTitlesList: the offline menu lists titles with episode
// and variant counts.
func TestOfflineTitlesList(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}}
	list := NewOfflineTitles(deps)
	v := list.View().Content
	if !strings.Contains(v, "Ванпанчмен") || !strings.Contains(v, "2 сер.") || !strings.Contains(v, "3 лок.") {
		t.Fatalf("offline list must show counts:\n%s", v)
	}
}

// TestOfflineEmptyLibrary: no downloads is a legal empty state (I3).
func TestOfflineEmptyLibrary(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{}}
	list := NewOfflineTitles(deps)
	v := list.View().Content
	if !strings.Contains(v, "Нет скачанных тайтлов") {
		t.Fatalf("empty offline library must render its empty state:\n%s", v)
	}
}

// TestOfflineSession: the offline session lists episodes, plays local
// files with the [OFFLINE] marker and never resolves skips.
func TestOfflineSession(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}, Playback: pb}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])

	t.Run("episodes listed with variant counts", func(t *testing.T) {
		v := s.View().Content
		if !strings.Contains(v, "Эп. 1") || !strings.Contains(v, "2") {
			t.Fatalf("episode list with variant counts missing:\n%s", v)
		}
	})

	t.Run("pick episode then watch plays local file offline-marked", func(t *testing.T) {
		s.episodeList.Jump(1) // episode 1
		next, _ := s.Update(enter())
		ss := next.(*offlineSession)
		if ss.current != "1" {
			t.Fatalf("pick must set current episode, got %q", ss.current)
		}
		// ▶ Смотреть on the action menu.
		idx := -1
		for i, c := range ss.list.Menu().Items {
			if c.ID == "watch" {
				idx = i
			}
		}
		if idx < 0 {
			t.Fatalf("offline action menu must offer watch:\n%s", ss.View().Content)
		}
		ss.list.Jump(idx)
		_, cmd := ss.Update(enter())
		if cmd == nil {
			t.Fatalf("watch must schedule playback")
		}
		// The play command resolves the local path via the snapshot.
		msg := cmd()
		if _, ok := msg.(offlinePlayMsg); !ok {
			t.Fatalf("offline watch must emit offlinePlayMsg, got %#v", msg)
		}
	})

	t.Run("variant resolve prefers matching keys, falls back to best quality", func(t *testing.T) {
		entries := titles[0].Snapshot.Entries
		if got := ResolveVariant(entries, "[a] dub", "[a] dub", 720); got != nil && got.Quality != 1080 {
			t.Fatalf("no exact 720 → best quality wins, got %d", got.Quality)
		}
		if got := ResolveVariant(entries, "[a] dub", "[b] sub", 720); got == nil || got.RelativePath != "ep1b.mkv" {
			t.Fatalf("exact variant must win, got %+v", got)
		}
	})

	t.Run("no skip lookup happens offline", func(t *testing.T) {
		// The offline path never calls ResolveSkips; assert by a
		// playback fake that would panic if asked for skips.
		deps2 := &Deps{
			Offline:  &fakeOffline{titles: offlineFixture()},
			Playback: &skipPanicPlayback{},
		}
		t2, _ := deps2.Offline.Titles()
		s2 := NewOfflineSession(deps2, t2[0])
		s2.episodeList.Jump(1)
		next, _ := s2.Update(enter())
		ss := next.(*offlineSession)
		ss.list.Jump(1)
		_, cmd := ss.Update(enter())
		if cmd == nil {
			t.Fatalf("watch must schedule")
		}
		if _, ok := cmd().(offlinePlayMsg); !ok {
			t.Fatalf("offline watch must emit offlinePlayMsg")
		}
	})
}

// skipPanicPlayback fails loudly if the offline flow ever resolves
// skips (spec: NO skip lookup — chapters are embedded).
type skipPanicPlayback struct{ fakePlayback }

func (p *skipPanicPlayback) ResolveSkips(context.Context, int64, float64) (string, func(), error) {
	panic("offline playback must never resolve skips")
}

// TestOfflineBackLeavesSession: esc from the offline session pops.
func TestOfflineBackLeavesSession(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])
	_, cmd := s.Update(esc())
	if _, ok := cmd().(popMsg); !ok {
		t.Fatalf("esc must pop the offline session, got %#v", cmd())
	}
}
