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
	if !strings.Contains(v, "Ванпанчмен") || !strings.Contains(v, "2 ep.") || !strings.Contains(v, "3 local") {
		t.Fatalf("offline list must show counts:\n%s", v)
	}
}

// TestOfflineEmptyLibrary: no downloads is a legal empty state (I3).
func TestOfflineEmptyLibrary(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{}}
	list := NewOfflineTitles(deps)
	v := list.View().Content
	if !strings.Contains(v, "No downloaded titles") {
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
		if !strings.Contains(v, "Ep. 1") || !strings.Contains(v, "2") {
			t.Fatalf("episode list with variant counts missing:\n%s", v)
		}
	})

	t.Run("pick episode then watch plays local file offline-marked", func(t *testing.T) {
		s.episodeList.Jump(0) // episode 1
		next, _ := s.Update(enter())
		ss := next.(*offlineSession)
		if ss.current != "1" {
			t.Fatalf("pick must set current episode, got %q", ss.current)
		}
		// ▶ Watch on the action menu.
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
		s2.episodeList.Jump(0)
		next, _ := s2.Update(enter())
		ss := next.(*offlineSession)
		ss.list.Jump(0)
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

func (p *skipPanicPlayback) ResolveSkips(context.Context, int64, float64) (string, func(), string, error) {
	panic("offline playback must never resolve skips")
}

func offlineActionIndex(s *offlineSession, id string) int {
	for i, c := range s.list.Menu().Items {
		if c.ID == id {
			return i
		}
	}
	return -1
}

// TestOfflineWatchDispatchesPlayback (C3): selecting an episode and
// watching drives the playback service with the local path and the
// [OFFLINE] title; the settle reaches the status line.
func TestOfflineWatchDispatchesPlayback(t *testing.T) {
	pb := &fakePlayback{}
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}, Playback: pb}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])

	s.episodeList.Jump(0) // episode 1
	next, _ := s.Update(enter())
	ss := next.(*offlineSession)
	ss.list.Jump(offlineActionIndex(ss, "watch"))
	_, cmd := ss.Update(enter())
	if cmd == nil {
		t.Fatalf("watch must schedule playback")
	}
	msg := cmd()
	pm, ok := msg.(offlinePlayMsg)
	if !ok {
		t.Fatalf("offline watch must emit offlinePlayMsg, got %T", msg)
	}

	// The screen must CONSUME offlinePlayMsg: dispatch Playback.Play.
	next, cmd = ss.Update(pm)
	if cmd == nil {
		t.Fatalf("offlinePlayMsg must dispatch Playback.Play")
	}
	settled := cmd()
	dm, ok := settled.(offlinePlayedMsg)
	if !ok {
		t.Fatalf("play must settle into offlinePlayedMsg, got %T", settled)
	}
	if dm.err != nil {
		t.Fatalf("fake playback must succeed, got %v", dm.err)
	}
	next, _ = next.Update(dm)
	ss = next.(*offlineSession)
	if !contains(ss.status, "Playback finished") {
		t.Fatalf("settle must reach the status line, got %q", ss.status)
	}
	if len(pb.played) != 1 {
		t.Fatalf("exactly one playback expected, got %d", len(pb.played))
	}
	if pb.played[0].URL != "/dl/Ванпанчмен/ep1.mkv" {
		t.Fatalf("player must get the local path, got %q", pb.played[0].URL)
	}
	if !contains(pb.played[0].Title, "[OFFLINE]") {
		t.Fatalf("player title must carry the [OFFLINE] marker, got %q", pb.played[0].Title)
	}
}

// TestOfflineVariantPickerFlow (C4): «Switch local stream» opens
// a real picker; Enter switches the local variant and returns to the
// menu; Esc cancels the picker without leaving the session.
func TestOfflineVariantPickerFlow(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])
	s.episodeList.Jump(0)
	next, _ := s.Update(enter())
	ss := next.(*offlineSession)

	t.Run("enter switches the variant", func(t *testing.T) {
		ss.list.Jump(offlineActionIndex(ss, "variant"))
		next, _ := ss.Update(enter())
		vs := next.(*offlineSession)
		if !vs.stateVariant {
			t.Fatalf("variant picker must engage")
		}
		// Down moves the PICKER (not the action menu): the cursor
		// starts on the first variant; one down lands on the 720p [b]
		// sub variant; Enter applies it.
		next, _ = vs.Update(down())
		next, _ = next.Update(enter())
		picked := next.(*offlineSession)
		if picked.stateVariant {
			t.Fatalf("picker must close after selection")
		}
		if picked.videoKey != "[a] dub" || picked.audioKey != "[b] sub" || picked.quality != 720 {
			t.Fatalf("variant switch must apply, got %q/%q/%d",
				picked.videoKey, picked.audioKey, picked.quality)
		}
		if !contains(picked.header(), "720p") {
			t.Fatalf("header must reflect the switched variant: %q", picked.header())
		}
		if picked.current != "1" {
			t.Fatalf("session must stay on the menu at the same episode, got %q", picked.current)
		}
	})

	t.Run("esc cancels the picker back to the menu", func(t *testing.T) {
		ss.list.Jump(offlineActionIndex(ss, "variant"))
		next, _ := ss.Update(enter())
		vs := next.(*offlineSession)
		if !vs.stateVariant {
			t.Fatalf("picker must engage")
		}
		next, cmd := vs.Update(esc())
		cancelled := next.(*offlineSession)
		if cancelled.stateVariant {
			t.Fatalf("esc must reset the picker state")
		}
		if cmd != nil {
			t.Fatalf("esc in the picker cancels it without popping, got %#v", cmd())
		}
		if cancelled.current != "1" {
			t.Fatalf("esc must keep the episode, got %q", cancelled.current)
		}
	})
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
