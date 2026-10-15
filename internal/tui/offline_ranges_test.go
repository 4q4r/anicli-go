package tui

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	anisync "github.com/an0nx/anicli-go/internal/sync"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
	"github.com/an0nx/anicli-go/internal/storage"
)

// rangesOfflineFixture builds a real library folder under t.TempDir():
// an offline index over some files (the downloader's write path) plus
// optionally hand-placed stray media, via download.UpsertEntry.
func rangesOfflineFixture(t *testing.T, dir string, indexedFiles map[string]string) {
	t.Helper()
	for file, label := range indexedFiles {
		path := filepath.Join(dir, file)
		if err := writeFixtureFile(path); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := download.UpsertEntry(dir, download.EntryInput{
			EpisodeNum: label,
			VideoKey:   "Kodik",
			AudioKey:   "Dub",
			Quality:    1080,
			FilePath:   path,
		}); err != nil {
			t.Fatalf("upsert %s: %v", file, err)
		}
	}
}

func writeFixtureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("x"), 0o600)
}

// realOfflineFor returns the real service pointed at the temp root.
func realOfflineFor(root string) *realOffline {
	return &realOffline{settings: config.Settings{Download: config.Download{Dir: root}}}
}

// TestRealOfflineTitlesPlainFolderUnchanged is the backward-compat
// anchor (Review Focus #1): a folder with an index and NO ranges file
// renders exactly today's one plain title.
func TestRealOfflineTitlesPlainFolderUnchanged(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1", "EP_2_Kodik_1080p.mkv": "2"})

	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatalf("Titles: %v", err)
	}
	if len(titles) != 1 {
		t.Fatalf("titles = %d, want exactly the plain one", len(titles))
	}
	got := titles[0]
	if got.Name != "Тайтл" || got.Range != nil {
		t.Fatalf("plain title = %+v, want Name=Тайтл Range=nil", got)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
	if got.RelDir != "Тайтл" {
		t.Errorf("RelDir = %q, want Тайтл", got.RelDir)
	}
	if got.Snapshot.TotalDownloaded() != 2 {
		t.Errorf("snapshot entries = %d, want 2", got.Snapshot.TotalDownloaded())
	}
}

// TestRealOfflineTitlesRangesSplit: one folder with a ranges file
// renders one virtual title per range plus the unsorted limbo title,
// in file order, with rewritten position labels and the REAL dir.
func TestRealOfflineTitlesRangesSplit(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "Реинкарнация")
	files := []string{
		"EP_1_Kodik_1080p.mkv", "EP_2_Kodik_1080p.mkv", // s1 (indexed)
		"OVA_1.mkv", // s2... actually the OVA range
		"stray.mkv", // unmatched
	}
	for _, f := range files {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1", "EP_2_Kodik_1080p.mkv": "2"})
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "Реинкарнация — S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "ova", Title: "Реинкарнация — OVA", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatalf("SaveRanges: %v", err)
	}

	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatalf("Titles: %v", err)
	}
	if len(titles) != 3 {
		t.Fatalf("titles = %d (%+v), want 3 (s1, ova, unsorted)", len(titles), titles)
	}
	wantNames := []string{"Реинкарнация — S1", "Реинкарнация — OVA", "Реинкарнация · несортированное"}
	for i, want := range wantNames {
		if titles[i].Name != want {
			t.Errorf("titles[%d].Name = %q, want %q", i, titles[i].Name, want)
		}
	}
	s1 := titles[0]
	if s1.Range == nil || s1.Range.ID != "s1" {
		t.Fatalf("s1.Range = %+v, want the s1 binding", s1.Range)
	}
	if s1.Snapshot.TitleDir != dir {
		t.Errorf("Snapshot.TitleDir = %q, want the REAL dir (playback path)", s1.Snapshot.TitleDir)
	}
	labels := []string{}
	for _, e := range s1.Snapshot.Entries {
		labels = append(labels, e.EpisodeNum)
	}
	if strings.Join(labels, ",") != "1,2" {
		t.Errorf("s1 position labels = %v, want [1 2]", labels)
	}
	if titles[2].Range != nil {
		t.Errorf("unsorted title carries a Range — the limbo must stay progress-less")
	}
	for _, e := range titles[2].Snapshot.Entries {
		if e.EpisodeNum != "stray.mkv" {
			t.Errorf("unsorted label = %q, want the base name", e.EpisodeNum)
		}
	}
}

// TestRealOfflineTitlesZeroFileRangeHidden: a range matching nothing
// is not rendered.
func TestRealOfflineTitlesZeroFileRangeHidden(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	for _, f := range []string{"EP_1_a.mkv", "EP_2_a.mkv"} {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "ghost", Title: "Ghost", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatal(err)
	}
	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0].Range == nil || titles[0].Range.ID != "s1" {
		t.Fatalf("titles = %+v, want only the s1 virtual title", titles)
	}
}

// TestRealOfflineTitlesCorruptRangesDegrades: a corrupt ranges file
// quarantines and the folder degrades to today's plain title.
func TestRealOfflineTitlesCorruptRangesDegrades(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1"})
	if err := download.SaveRanges(dir, &download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, download.RangesFilename), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}

	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0].Name != "Тайтл" || titles[0].Range != nil {
		t.Fatalf("titles = %+v, want the single plain title", titles)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	quarantined := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), download.RangesFilename+".corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Error("no .corrupt-* quarantine file after the degraded load")
	}
}

// TestRealOfflineTitlesEmptyIndexWithRanges: a hand-placed folder
// (no index at all) with a ranges file still renders its virtual
// titles as file pseudo-entries.
func TestRealOfflineTitlesEmptyIndexWithRanges(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Ручная")
	for _, f := range []string{"EP_1_a.mkv", "EP_2_a.mkv"} {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "Ручная — S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatal(err)
	}
	titles, err := realOfflineFor(root).Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0].Name != "Ручная — S1" {
		t.Fatalf("titles = %+v, want the virtual title over pseudo-entries", titles)
	}
	if titles[0].Snapshot.TotalDownloaded() != 2 {
		t.Fatalf("entries = %d, want 2", titles[0].Snapshot.TotalDownloaded())
	}
	for i, e := range titles[0].Snapshot.Entries {
		if e.EpisodeNum != strconv.Itoa(i+1) || e.VideoKey != "file" {
			t.Errorf("entries[%d] = %+v, want the file pseudo-entry with label %d", i, e, i+1)
		}
	}
}

// TestLocalEpisodeCountsAggregatesByDir (R4 fix): the online
// session's «★» marks must see the WHOLE folder — counts sum across
// every title sharing the Dir, matched by directory not by name.
func TestLocalEpisodeCountsAggregatesByDir(t *testing.T) {
	dir := "/dl/Тайтл"
	s1 := OfflineTitle{
		Dir: dir, Name: "Тайтл — S1", RelDir: "Тайтл",
		Range: &download.RangeBinding{ID: "s1", Title: "Тайтл — S1"},
		Snapshot: download.Snapshot{TitleDir: dir, Entries: []download.Entry{
			{EpisodeNum: "1", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_1.mkv"},
		}},
	}
	s2 := OfflineTitle{
		Dir: dir, Name: "Тайтл — S2", RelDir: "Тайтл",
		Range: &download.RangeBinding{ID: "s2", Title: "Тайтл — S2"},
		Snapshot: download.Snapshot{TitleDir: dir, Entries: []download.Entry{
			{EpisodeNum: "1", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_1b.mkv"},
			{EpisodeNum: "2", VideoKey: "Kodik", AudioKey: "Dub", Quality: 1080, RelativePath: "EP_2b.mkv"},
		}},
	}
	counts := LocalEpisodeCounts([]OfflineTitle{s1, s2}, "Тайтл")
	if counts["1"] != 2 {
		t.Errorf("counts[1] = %d, want 2 (both ranges share the folder)", counts["1"])
	}
	if counts["2"] != 1 {
		t.Errorf("counts[2] = %d, want 1", counts["2"])
	}
}

// rangesProgressDeps builds a range-title session over a real temp
// library: two indexed ranges (s1 bound to shikimori 39535, s2 to
// 55888 with offset) plus the real Offline service, history fake and
// playback fake.
type rangesProgressEnv struct {
	deps   *Deps
	hist   *fakeHistory
	shiki  *fakeShiki
	pb     *fakePlayback
	dir    string
	relDir string
	root   string
}

func rangesProgressSetup(t *testing.T, offset int) *rangesProgressEnv {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Реинкарнация")
	rangesOfflineFixture(t, dir, map[string]string{
		"EP_1_Kodik_1080p.mkv": "1",
		"EP_2_Kodik_1080p.mkv": "2",
		"EP_3_Kodik_1080p.mkv": "3",
		"EP_4_Kodik_1080p.mkv": "4",
		"EP_5_Kodik_1080p.mkv": "5",
	})
	rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "Реинкарнация — S1", ShikimoriID: 39535, ShikimoriTitle: "Mushoku Tensei", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_[12]*"}}},
		{ID: "s2", Title: "Реинкарнация — S2 ч.2", ShikimoriID: 55888, Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_[345]*"}}, EpisodeOffset: offset},
	}}
	if err := download.SaveRanges(dir, &rf); err != nil {
		t.Fatal(err)
	}
	env := &rangesProgressEnv{
		hist:   &fakeHistory{},
		shiki:  &fakeShiki{enabled: true, ids: map[string]int64{"Реинкарнация": 39535}},
		pb:     &fakePlayback{},
		dir:    dir,
		relDir: "Реинкарнация",
		root:   root,
	}
	env.deps = &Deps{
		Offline:  realOfflineFor(root),
		History:  env.hist,
		Shiki:    env.shiki,
		Playback: env.pb,
	}
	return env
}

// rangeSession picks the s2 virtual title and drives the session onto
// the episode picker.
func rangeSession(t *testing.T, env *rangesProgressEnv) *offlineSession {
	t.Helper()
	titles, err := env.deps.Offline.Titles()
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range titles {
		if title.Range != nil && title.Range.ID == "s2" {
			return NewOfflineSession(env.deps, title)
		}
	}
	t.Fatalf("s2 virtual title missing among %+v", titles)
	return nil
}

// settleRangeProgress drives the full watch chain: pick episode "1",
// watch → offlinePlayMsg → playLocal → offlinePlayedMsg{nil} → the
// save cmd, settling every intermediate command like the runtime
// would.
func settleRangeProgress(t *testing.T, s *offlineSession) offlineProgressMsg {
	t.Helper()
	s.current = "1"
	_, watchCmd := s.watch()
	playMsg := watchCmd()
	next, playCmd := s.Update(playMsg)
	if playCmd == nil {
		t.Fatal("playLocal must schedule the playback")
	}
	played := playCmd()
	playedMsg, ok := played.(offlinePlayedMsg)
	if !ok {
		t.Fatalf("expected offlinePlayedMsg, got %#v", played)
	}
	_, saveCmd := next.Update(playedMsg)
	if saveCmd == nil {
		t.Fatal("a range session must schedule saveRangeProgress after playback")
	}
	settled := saveCmd()
	progress, ok := settled.(offlineProgressMsg)
	if !ok {
		t.Fatalf("expected offlineProgressMsg, got %#v", settled)
	}
	return progress
}

// TestOfflineRangeProgressSaveAndSync is the mandated integration
// test: playing from the s2 virtual title lands the history row on
// the s2 synthetic source key with s2's shikimori binding.
func TestOfflineRangeProgressSaveAndSync(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	s := rangeSession(t, env)
	progress := settleRangeProgress(t, s)

	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v, want nil", progress.saveErr)
	}
	if len(env.hist.records) != 1 {
		t.Fatalf("history records = %d, want 1", len(env.hist.records))
	}
	rec := env.hist.records[0]
	if rec.SourceID != "offline" {
		t.Errorf("SourceID = %q, want offline", rec.SourceID)
	}
	wantURL := download.SyntheticSourceURL(env.root, env.relDir, "s2")
	if rec.SourceURL != wantURL {
		t.Errorf("SourceURL = %q, want %q", rec.SourceURL, wantURL)
	}
	if rec.CurrentEpisode != "1" {
		t.Errorf("CurrentEpisode = %q, want the position label 1", rec.CurrentEpisode)
	}
	if rec.ShikimoriID == nil || *rec.ShikimoriID != 55888 {
		t.Errorf("ShikimoriID = %v, want 55888 (range s2 binding)", rec.ShikimoriID)
	}
	if rec.Title != "Реинкарнация — S2 ч.2" {
		t.Errorf("Title = %q, want the range title", rec.Title)
	}
}

// TestOfflineRangeProgressOffsetPush pins the offset units (Review
// Focus #2): position "3" + offset 12 pushes episode 15, while the
// stored row keeps the position label.
func TestOfflineRangeProgressOffsetPush(t *testing.T) {
	env := rangesProgressSetup(t, 12)
	s := rangeSession(t, env)
	s.current = "3"
	_, watchCmd := s.watch()
	next, playCmd := s.Update(watchCmd())
	_, saveCmd := next.Update(playCmd())
	if saveCmd == nil {
		t.Fatal("save cmd missing")
	}
	settled := saveCmd()
	progress := settled.(offlineProgressMsg)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	if len(env.shiki.epPushes) != 1 {
		t.Fatalf("shiki pushes = %d, want 1", len(env.shiki.epPushes))
	}
	if got := env.shiki.epPushes[0].episodes; got != 15 {
		t.Errorf("pushed episode = %d, want 15 (position 3 + offset 12)", got)
	}
	if env.hist.records[0].CurrentEpisode != "3" {
		t.Errorf("stored label = %q, want the position 3 (units never mix)", env.hist.records[0].CurrentEpisode)
	}
}

// TestOfflineSessionPlainFolderNoProgress: a plain folder title
// (Range nil) keeps today's write-nothing behavior.
func TestOfflineSessionPlainFolderNoProgress(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	plain := OfflineTitle{Dir: env.dir, Name: "Реинкарнация"}
	s := NewOfflineSession(env.deps, plain)
	if cmd := s.saveRangeProgress(); cmd != nil {
		t.Fatal("plain session must not schedule a save")
	}
	if len(env.hist.records) != 0 {
		t.Fatalf("records = %d, want none", len(env.hist.records))
	}
}

// TestOfflineRangeProgressSaveError: a failed save surfaces the
// typed status and never syncs.
func TestOfflineRangeProgressSaveError(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	env.hist.saveErr = errors.New("db locked")
	s := rangeSession(t, env)
	progress := settleRangeProgress(t, s)
	if progress.saveErr == nil {
		t.Fatal("saveErr = nil, want the failure")
	}
	if progress.note != "" {
		t.Errorf("note = %q, want empty (no sync after a failed save)", progress.note)
	}
	if len(env.shiki.epPushes) != 0 {
		t.Errorf("shiki pushes = %d, want none", len(env.shiki.epPushes))
	}
	next, _ := s.Update(progress)
	view := next.View().Content
	// The default test locale is en; the ru table renders «Прогресс
	// не сохранён: …» (pinned by the i18n parity keys).
	if !strings.Contains(view, "Progress not saved") && !strings.Contains(view, "Прогресс не сохранён") {
		t.Errorf("status must surface the failed save:\n%s", view)
	}
}

// TestOfflineRangeProgressSyncNote: a non-empty sync verdict renders
// appended to the play_done status.
func TestOfflineRangeProgressSyncNote(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	s := rangeSession(t, env)
	progress := settleRangeProgress(t, s)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	if progress.note == "" {
		t.Fatal("note = empty, want the sync verdict line")
	}
	next, _ := s.Update(progress)
	view := next.View().Content
	if !strings.Contains(view, progress.note) {
		t.Errorf("status must render the sync note %q:\n%s", progress.note, view)
	}
}

// TestRealOfflineTitlesWatchedDirs: titles come from every watched
// root; a folder listed under two roots renders once (first root
// wins) with RelDir relative to the root that actually holds it.
func TestRealOfflineTitlesWatchedDirs(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	for _, root := range []string{rootA, rootB} {
		dir := filepath.Join(root, "Сериал")
		for _, f := range []string{"EP_1_a.mkv", "EP_2_a.mkv"} {
			if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
				t.Fatal(err)
			}
		}
		rf := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
			{ID: "s1", Title: "Сериал — S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		}}
		if err := download.SaveRanges(dir, &rf); err != nil {
			t.Fatal(err)
		}
	}

	svc := &realOffline{settings: config.Settings{Download: config.Download{
		Dir:         rootA,
		WatchedDirs: []string{rootB},
	}}}
	titles, err := svc.Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 2 {
		t.Fatalf("titles = %d (%+v), want one per root", len(titles), titles)
	}
	if titles[0].RelDir != "Сериал" || titles[1].RelDir != "Сериал" {
		t.Errorf("RelDirs = %q, %q; want Сериал under each own root", titles[0].RelDir, titles[1].RelDir)
	}
	if titles[0].Dir != filepath.Join(rootA, "Сериал") || titles[1].Dir != filepath.Join(rootB, "Сериал") {
		t.Errorf("Dirs must point into their own roots: %q, %q", titles[0].Dir, titles[1].Dir)
	}
	// M2: the titles must carry their watched root so the synthetic
	// progress key hashes THE ROOT — two same-named folders under two
	// roots never share one anime_progress row.
	if titles[0].Root != rootA || titles[1].Root != rootB {
		t.Errorf("Roots = %q, %q; want their own watched roots", titles[0].Root, titles[1].Root)
	}
	if download.SyntheticSourceURL(titles[0].Root, titles[0].RelDir, "s1") ==
		download.SyntheticSourceURL(titles[1].Root, titles[1].RelDir, "s1") {
		t.Error("identical basenames under two roots must yield distinct synthetic keys")
	}

	// The same physical folder under both roots renders once.
	shared := &realOffline{settings: config.Settings{Download: config.Download{
		Dir:         rootA,
		WatchedDirs: []string{rootA},
	}}}
	titles, err = shared.Titles()
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 {
		t.Fatalf("dedupe: titles = %d, want 1", len(titles))
	}

	// A missing watched root is tolerated (the missing-dir tolerance).
	missing := &realOffline{settings: config.Settings{Download: config.Download{
		Dir:         rootA,
		WatchedDirs: []string{filepath.Join(rootB, "ghost")},
	}}}
	titles, err = missing.Titles()
	if err != nil {
		t.Fatalf("missing root must not fail: %v", err)
	}
	if len(titles) != 1 {
		t.Fatalf("titles = %d, want the rootA one", len(titles))
	}
}

// TestRealOfflineTitlesUnreadableExtraRootSkipped (m9): one unreadable
// extra watched root (an unmounted NAS, EACCES/EIO) must not discard
// the WHOLE library — the root is skipped with a log note while the
// main downloads root still enumerates. The main root's own read error
// still fails loudly.
func TestRealOfflineTitlesUnreadableExtraRootSkipped(t *testing.T) {
	rootA := t.TempDir()
	dir := filepath.Join(rootA, "Сериал")
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_a.mkv": "1"})

	// A FILE where an extra root directory should be: ReadDir fails
	// with ENOTDIR — the unmounted/broken root simulation.
	broken := filepath.Join(rootA, "not-a-dir")
	if err := os.WriteFile(broken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	svc := &realOffline{settings: config.Settings{Download: config.Download{
		Dir:         rootA,
		WatchedDirs: []string{broken},
	}}, log: log}
	titles, err := svc.Titles()
	if err != nil {
		t.Fatalf("an unreadable EXTRA root must not fail the library: %v", err)
	}
	if len(titles) != 1 || titles[0].Name != "Сериал" {
		t.Fatalf("titles = %+v, want the rootA title", titles)
	}
	if !strings.Contains(logBuf.String(), broken) {
		t.Errorf("the skipped root must be logged:\n%s", logBuf.String())
	}

	// The MAIN root's read error still surfaces.
	mainSvc := &realOffline{settings: config.Settings{Download: config.Download{Dir: broken}}}
	if _, err := mainSvc.Titles(); err == nil {
		t.Fatal("a broken MAIN root must fail loudly")
	} else if !strings.Contains(err.Error(), broken) {
		t.Errorf("err = %v, want the failing root path in the message", err)
	}
}

// TestRealOfflineTitlesDataDirErrorPropagates (m1): the DataDir
// failure of the default downloads root must propagate out of Titles
// — a swallowed error rendered the whole library silently empty.
func TestRealOfflineTitlesDataDirErrorPropagates(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANICLI_DATA", blocker) // mkdir over a regular file fails
	svc := &realOffline{settings: config.Settings{}}
	if _, err := svc.Titles(); err == nil {
		t.Fatal("the DataDir error must propagate, not render an empty library")
	}
}

// TestRealOfflineTitlesRangesLoadErrorLogged (m5): a ranges file that
// fails to load (a foreign version, a failed quarantine rename)
// degrades the folder to the plain title AND leaves a log note — no
// total silence.
func TestRealOfflineTitlesRangesLoadErrorLogged(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Тайтл")
	rangesOfflineFixture(t, dir, map[string]string{"EP_1_Kodik_1080p.mkv": "1"})
	if err := os.WriteFile(filepath.Join(dir, download.RangesFilename),
		[]byte(`{"version": 99, "ranges": []}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	svc := &realOffline{settings: config.Settings{Download: config.Download{Dir: root}},
		log: slog.New(slog.NewTextHandler(&logBuf, nil))}
	titles, err := svc.Titles()
	if err != nil {
		t.Fatalf("one bad folder must not fail the library: %v", err)
	}
	if len(titles) != 1 || titles[0].Name != "Тайтл" || titles[0].Range != nil {
		t.Fatalf("titles = %+v, want the single plain title", titles)
	}
	if !strings.Contains(logBuf.String(), "ranges") {
		t.Errorf("the ranges load failure must be logged:\n%s", logBuf.String())
	}
}

// TestOfflineRangeProgressSourceKeyCarriesRoot (M2): the row lands on
// the root-hashed synthetic key of the ROOT the folder lives under —
// identical basenames under two roots never share a progress row.
func TestOfflineRangeProgressSourceKeyCarriesRoot(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	s := rangeSession(t, env)
	progress := settleRangeProgress(t, s)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	want := download.SyntheticSourceURL(env.root, env.relDir, "s2")
	if got := env.hist.records[0].SourceURL; got != want {
		t.Errorf("SourceURL = %q, want the root-hashed %q", got, want)
	}
}

// TestOfflineSessionRangeTitleKeepsBindRow (M3): a fully-bound folder
// renders ONLY range titles — each of their sessions must offer the
// ✂️ editor row, or the owner loses every edit/rebind/delete path the
// moment the last stray file is bound.
func TestOfflineSessionRangeTitleKeepsBindRow(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	s := rangeSession(t, env)
	if offlineActionIndex(s, "bind") < 0 {
		t.Fatal("a range title's action menu must offer the ✂️ bind row")
	}
	// And the pick actually opens the editor over the folder.
	_, cmd := s.handleKeyRouteBind(t)
	if _, ok := pushedScreen(t, cmd).(*bindEditor); !ok {
		t.Error("the bind pick from a range title must push the bind editor")
	}
}

// handleKeyRouteBind drives the episode pick + the bind row the way
// the runtime does (the menu sits behind the episode picker).
func (s *offlineSession) handleKeyRouteBind(t *testing.T) (Screen, tea.Cmd) {
	t.Helper()
	s.episodeList.Jump(0)
	next, _ := s.Update(enter())
	session := next.(*offlineSession)
	session.list.Jump(offlineActionIndex(session, "bind"))
	_, cmd := session.Update(enter())
	return session, cmd
}

// TestRangeSaveKeepsOnlineRow (M4): an offline range save must not
// touch an unrelated ONLINE row that carries the same shikimori id —
// source-only lookup + upsert; the shikimori-id fallback (which MOVES
// and retires the online row) stays online-path behavior.
func TestRangeSaveKeepsOnlineRow(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	onlineID := int64(55888)
	online := storage.AnimeProgress{
		Title:          "Онлайн-строка",
		SourceID:       "shikimori",
		SourceURL:      "https://shikimori.io/animes/55888",
		CurrentEpisode: "5",
		ShikimoriID:    &onlineID,
		UpdatedAt:      nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &online); err != nil {
		t.Fatal(err)
	}
	env.deps.History = &realHistory{store: store}
	env.hist.records = nil

	s := rangeSession(t, env)
	progress := settleRangeProgress(t, s)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}

	// The online row is untouched: same key, same episode label.
	kept, err := store.Progress.GetBySource(ctx, "shikimori", "https://shikimori.io/animes/55888")
	if err != nil {
		t.Fatalf("the online row must survive the offline save: %v", err)
	}
	if kept.CurrentEpisode != "5" {
		t.Errorf("online row episode = %q, want untouched 5", kept.CurrentEpisode)
	}
	// The offline row exists under the synthetic key.
	wantURL := download.SyntheticSourceURL(env.root, env.relDir, "s2")
	off, err := store.Progress.GetBySource(ctx, "offline", wantURL)
	if err != nil {
		t.Fatalf("the offline row must exist under the synthetic key: %v", err)
	}
	if off.CurrentEpisode != "1" || off.ShikimoriID == nil || *off.ShikimoriID != 55888 {
		t.Errorf("offline row = %+v, want episode 1 with the s2 binding", off)
	}
}

// TestOfflineRangeProgressGuardPrecomputedPrior (M5): the no-rollback
// guard must compare ENTRY units — the prior read BEFORE the save (a
// stored position label converted by +offset), not the freshly
// overwritten row. Rewatching position 1 of an offset-12 range with
// the row at position 10 (entry 22) must NOT push 13 over 22.
func TestOfflineRangeProgressGuardPrecomputedPrior(t *testing.T) {
	env := rangesProgressSetup(t, 12)
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	shikiID := int64(55888)
	wantURL := download.SyntheticSourceURL(env.root, env.relDir, "s2")
	seeded := storage.AnimeProgress{
		Title:          "Реинкарнация — S2 ч.2",
		SourceID:       "offline",
		SourceURL:      wantURL,
		CurrentEpisode: "10",
		ShikimoriID:    &shikiID,
		UpdatedAt:      nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &seeded); err != nil {
		t.Fatal(err)
	}
	env.deps.History = &realHistory{store: store}

	s := rangeSession(t, env)
	s.current = "1"
	_, watchCmd := s.watch()
	next, playCmd := s.Update(watchCmd())
	_, saveCmd := next.Update(playCmd())
	if saveCmd == nil {
		t.Fatal("save cmd missing")
	}
	progress := saveCmd().(offlineProgressMsg)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	// The guard blocked the push: no episode ever reached shikimori.
	if len(env.shiki.epPushes) != 0 {
		t.Errorf("shiki pushes = %d (%+v), want 0 — prior 22 must block episode 13",
			len(env.shiki.epPushes), env.shiki.epPushes)
	}
	if !strings.Contains(progress.note, "22") {
		t.Errorf("note = %q, want the no-rollback verdict carrying prior 22", progress.note)
	}
}

// TestOfflineRangePriorIgnoresNewerOnlineRow (F2): the prior must come
// from the range's OWN row (source lookup), never from the newest row
// sharing the shikimori id. A NEWER online row of the same entry
// (global episode 17) must not block the FIRST offline push: no own
// row exists yet, so prior = 0 and the push of entry 13 goes through.
func TestOfflineRangePriorIgnoresNewerOnlineRow(t *testing.T) {
	env := rangesProgressSetup(t, 12)
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	onlineID := int64(55888)
	online := storage.AnimeProgress{
		Title:          "Онлайн-строка",
		SourceID:       "shikimori",
		SourceURL:      "https://shikimori.io/animes/55888",
		CurrentEpisode: "17",
		ShikimoriID:    &onlineID,
		UpdatedAt:      nowUTC().Add(2 * time.Minute), // strictly newer
	}
	if err := store.Progress.Upsert(ctx, &online); err != nil {
		t.Fatal(err)
	}
	env.deps.History = &realHistory{store: store}

	s := rangeSession(t, env)
	s.current = "1"
	_, watchCmd := s.watch()
	next, playCmd := s.Update(watchCmd())
	_, saveCmd := next.Update(playCmd())
	if saveCmd == nil {
		t.Fatal("save cmd missing")
	}
	progress := saveCmd().(offlineProgressMsg)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	// The online row's global episode (17 + offset 12 = prior 29) must
	// NOT block the first offline push of entry 13.
	if len(env.shiki.epPushes) != 1 {
		t.Fatalf("shiki pushes = %d (%+v), want 1 — the newer online row must not block the first range push",
			len(env.shiki.epPushes), env.shiki.epPushes)
	}
	push := env.shiki.epPushes[0]
	if push.shikimoriID != 55888 || push.episodes != 13 {
		t.Errorf("push = %+v, want shikimoriID 55888 episodes 13", push)
	}
}

// TestOfflineRangePriorOwnRowBeatsNewerOnlineRow (F2): when the range's
// OWN row exists, it is the prior — even when a foreign online row of
// the same shikimori id is newer. Own row at position 10 (entry 22)
// must produce the 22-verdict, not the online row's 17+12=29.
func TestOfflineRangePriorOwnRowBeatsNewerOnlineRow(t *testing.T) {
	env := rangesProgressSetup(t, 12)
	ctx := context.Background()
	store, err := storage.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	shikiID := int64(55888)
	wantURL := download.SyntheticSourceURL(env.root, env.relDir, "s2")
	own := storage.AnimeProgress{
		Title:          "Реинкарнация — S2 ч.2",
		SourceID:       "offline",
		SourceURL:      wantURL,
		CurrentEpisode: "10",
		ShikimoriID:    &shikiID,
		UpdatedAt:      nowUTC(),
	}
	if err := store.Progress.Upsert(ctx, &own); err != nil {
		t.Fatal(err)
	}
	onlineID := int64(55888)
	online := storage.AnimeProgress{
		Title:          "Онлайн-строка",
		SourceID:       "shikimori",
		SourceURL:      "https://shikimori.io/animes/55888",
		CurrentEpisode: "17",
		ShikimoriID:    &onlineID,
		UpdatedAt:      nowUTC().Add(2 * time.Minute), // strictly newer
	}
	if err := store.Progress.Upsert(ctx, &online); err != nil {
		t.Fatal(err)
	}
	env.deps.History = &realHistory{store: store}

	s := rangeSession(t, env)
	s.current = "1"
	_, watchCmd := s.watch()
	next, playCmd := s.Update(watchCmd())
	_, saveCmd := next.Update(playCmd())
	if saveCmd == nil {
		t.Fatal("save cmd missing")
	}
	progress := saveCmd().(offlineProgressMsg)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	if len(env.shiki.epPushes) != 0 {
		t.Errorf("shiki pushes = %d (%+v), want 0 — the own row's prior 22 must block episode 13",
			len(env.shiki.epPushes), env.shiki.epPushes)
	}
	if !strings.Contains(progress.note, "22") {
		t.Errorf("note = %q, want the no-rollback verdict carrying the OWN row's prior 22", progress.note)
	}
}

// TestOfflineRangeProgressPassesPriorToDispatcher (M5, dispatcher
// leg): the precomputed prior rides into the ProgressSyncPrior seam —
// same units, same guard — instead of the dispatcher re-reading the
// freshly overwritten row.
func TestOfflineRangeProgressPassesPriorToDispatcher(t *testing.T) {
	env := rangesProgressSetup(t, 12)
	var got struct {
		shikiID  int64
		episode  int
		prior    int
		hasPrior bool
	}
	env.deps.ProgressSyncPrior = func(_ context.Context, shikiID int64, episode, prior int, hasPrior bool) anisync.Report {
		got = struct {
			shikiID  int64
			episode  int
			prior    int
			hasPrior bool
		}{shikiID, episode, prior, hasPrior}
		return anisync.Report{Participated: true, Verdicts: []anisync.Verdict{
			{Key: "shikimori", Synced: true},
		}}
	}
	// A prior row exists (position 3 from the last watch): the
	// dispatcher receives the ENTRY-unit prior 15 (3 + offset 12).
	// The prior lives on the range's OWN row (review round 2) — the
	// synthetic source key, not the shikimori-id fixture.
	env.hist.bySource = map[string]*storage.AnimeProgress{
		bySourceKey("offline", download.SyntheticSourceURL(env.root, env.relDir, "s2")): {ID: 9, CurrentEpisode: "3"},
	}
	s := rangeSession(t, env)
	s.current = "3"
	_, watchCmd := s.watch()
	next, playCmd := s.Update(watchCmd())
	_, saveCmd := next.Update(playCmd())
	progress := saveCmd().(offlineProgressMsg)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	if !got.hasPrior {
		t.Fatal("the offline range path must pass the precomputed prior")
	}
	if got.episode != 15 || got.prior != 15 {
		t.Errorf("dispatcher got episode=%d prior=%d, want 15/15 (prior = position 3 + offset 12)",
			got.episode, got.prior)
	}
	if got.shikiID != 55888 {
		t.Errorf("dispatcher shikiID = %d, want 55888", got.shikiID)
	}
}

// TestOfflineProgressSettlesPlayedEpisode (m7): the settle writes the
// PLAYED episode, not the live session state — starting ep1 and
// switching to ep3 before ep1's playback settles must still record 1.
func TestOfflineProgressSettlesPlayedEpisode(t *testing.T) {
	env := rangesProgressSetup(t, 0)
	s := rangeSession(t, env)
	s.current = "1"
	_, watchCmd := s.watch()
	next, playCmd := s.Update(watchCmd())
	// The playback is still running while the user moves on.
	s.current = "3"
	s.videoKey, s.audioKey = "Other", "Other"
	// ep1's playback settles now.
	_, saveCmd := next.Update(playCmd())
	if saveCmd == nil {
		t.Fatal("settle must schedule the progress save")
	}
	progress := saveCmd().(offlineProgressMsg)
	if progress.saveErr != nil {
		t.Fatalf("saveErr = %v", progress.saveErr)
	}
	if got := env.hist.records[0].CurrentEpisode; got != "1" {
		t.Errorf("stored episode = %q, want the PLAYED 1 (not the live 3)", got)
	}
	if env.hist.records[0].VideoDub == nil || *env.hist.records[0].VideoDub != "Kodik" {
		t.Errorf("stored dub = %v, want the PLAYED variant keys", env.hist.records[0].VideoDub)
	}
}
