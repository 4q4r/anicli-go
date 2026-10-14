package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
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
	wantURL := download.SyntheticSourceURL(env.relDir, "s2")
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
