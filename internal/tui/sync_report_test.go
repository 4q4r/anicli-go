package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	anisync "github.com/an0nx/anicli-go/internal/sync"
)

// TestRenderSyncReport pins the per-provider verdict lines (PR112 task
// E): separate lines per provider, both rendered even when only one
// push succeeded, typed skips rendered from the typed notes.
func TestRenderSyncReport(t *testing.T) {
	depsWithShiki := func(enabled bool, mode string) *Deps {
		return &Deps{Shiki: &fakeShikiService{enabled: enabled, mode: mode}, MALCfg: config.MAL{}}
	}

	t.Run("dual sync — two verdict lines", func(t *testing.T) {
		deps := depsWithShiki(true, "bearer")
		line := renderSyncReport(deps, anisync.Report{Participated: true, Verdicts: []anisync.Verdict{
			{Key: "shikimori", Synced: true},
			{Key: "myanimelist", Synced: true},
		}}, 5)
		if !strings.Contains(line, "Shikimori: progress synchronized (ep 5)") ||
			!strings.Contains(line, "MyAnimeList: progress synchronized (ep 5)") {
			t.Fatalf("line = %q, want both provider verdicts", line)
		}
	})

	t.Run("one fails, one succeeds — both lines", func(t *testing.T) {
		deps := depsWithShiki(true, "bearer")
		line := renderSyncReport(deps, anisync.Report{Participated: true, Verdicts: []anisync.Verdict{
			{Key: "shikimori", Synced: true},
			{Key: "myanimelist", Err: errors.New("network down")},
		}}, 5)
		if !strings.Contains(line, "Shikimori: progress synchronized") {
			t.Fatalf("line = %q, want the shiki success", line)
		}
		if !strings.Contains(line, "MyAnimeList: sync error") || !strings.Contains(line, "network down") {
			t.Fatalf("line = %q, want the mal failure with the cause", line)
		}
	})

	t.Run("mal not authorized — typed note", func(t *testing.T) {
		deps := depsWithShiki(true, "bearer")
		line := renderSyncReport(deps, anisync.Report{Participated: true, Verdicts: []anisync.Verdict{
			{Key: "myanimelist", Note: anisync.NoteAuthRequired},
		}}, 5)
		if !strings.Contains(line, "MyAnimeList: not authorized") {
			t.Fatalf("line = %q, want the mal auth note", line)
		}
	})

	t.Run("not on MAL — typed skip beside the shiki success", func(t *testing.T) {
		deps := depsWithShiki(true, "bearer")
		line := renderSyncReport(deps, anisync.Report{Participated: true, Verdicts: []anisync.Verdict{
			{Key: "shikimori", Synced: true},
			{Key: "myanimelist", Note: anisync.NoteNotOnMAL},
		}}, 5)
		if !strings.Contains(line, "not found on MyAnimeList") {
			t.Fatalf("line = %q, want the not-on-MAL note", line)
		}
		if !strings.Contains(line, "Shikimori: progress synchronized") {
			t.Fatalf("line = %q, want the shiki success beside the skip", line)
		}
	})

	t.Run("no rollback — the single guard note with the prior", func(t *testing.T) {
		deps := depsWithShiki(true, "bearer")
		line := renderSyncReport(deps, anisync.Report{Participated: true, NoRollback: true, Prior: 9}, 5)
		if !strings.Contains(line, "already 9") || !strings.Contains(line, "episode 5") && !strings.Contains(line, "эп 5") {
			t.Fatalf("line = %q, want the no-rollback note naming prior 9", line)
		}
	})

	t.Run("nothing participates — the legacy disabled note", func(t *testing.T) {
		deps := &Deps{Shiki: &fakeShikiService{enabled: false, mode: "disabled"}}
		line := renderSyncReport(deps, anisync.Report{}, 5)
		if !strings.Contains(line, "tracker disabled") {
			t.Fatalf("line = %q, want the legacy disabled note", line)
		}
	})

	t.Run("nothing participates but shiki enabled-unauth — the legacy auth note", func(t *testing.T) {
		deps := depsWithShiki(true, "none")
		line := renderSyncReport(deps, anisync.Report{}, 5)
		if !strings.Contains(line, "not authorized") {
			t.Fatalf("line = %q, want the legacy unauthorized note", line)
		}
	})
}

// fakeShikiService is the minimal Shikimori surface renderSyncReport
// consults for the legacy notes; the full service interface is embedded
// (unused methods would panic — renderSyncReport touches only these).
type fakeShikiService struct {
	ShikimoriService
	enabled bool
	mode    string
}

func (f *fakeShikiService) Enabled() bool { return f.enabled }
func (f *fakeShikiService) Mode() string  { return f.mode }

// TestShikiSyncCmdRoutesToDispatcher pins: when the dual-sync seam is
// wired, the episode start uses it even without a wired shiki service;
// without the seam the legacy path needs the shiki service.
func TestShikiSyncCmdRoutesToDispatcher(t *testing.T) {
	sessionWithEpisode := func(deps *Deps) *sessionScreen {
		s := &sessionScreen{deps: deps}
		s.primary.Meta = map[string]any{"shikimori_id": int64(21)}
		s.order = []string{"5"} // the episode about to play
		return s
	}
	t.Run("dispatcher seam wins", func(t *testing.T) {
		s := sessionWithEpisode(&Deps{ProgressSync: func(context.Context, int64, int) anisync.Report {
			return anisync.Report{}
		}})
		if cmd := s.shikiSyncCmd(); cmd == nil {
			t.Fatal("ProgressSync wired: the sync command must be scheduled")
		}
	})
	t.Run("legacy path without the seam needs shiki", func(t *testing.T) {
		s := sessionWithEpisode(&Deps{})
		if cmd := s.shikiSyncCmd(); cmd != nil {
			t.Fatal("no seam and no shiki service: nothing must be scheduled")
		}
	})
}
