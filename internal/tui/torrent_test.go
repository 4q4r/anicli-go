package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/an0nx/anicli-go/internal/torrent"
)

// fakeTorrent implements TorrentService.
type fakeTorrent struct {
	enabled   bool
	releases  []torrent.Release
	files     map[string][]torrent.FileEntry
	url       string
	refreshes int
	err       error
}

func (f *fakeTorrent) Enabled() bool { return f.enabled }

func (f *fakeTorrent) Refresh(_ context.Context) ([]torrent.Release, error) {
	f.refreshes++
	return f.releases, f.err
}

func (f *fakeTorrent) Files(_ context.Context, ih torrent.InfoHash) ([]torrent.FileEntry, error) {
	return f.files[ih.HexString()], nil
}

func (f *fakeTorrent) StreamURL(_ torrent.InfoHash, _ int) string {
	if f.url == "" {
		return "http://127.0.0.1:40123/stream/aa/0"
	}
	return f.url
}

var _ TorrentService = (*fakeTorrent)(nil)

// settle feeds async command results back into the screen until the
// chain drains (bubbletea runtime parity — PR24 GOTCHA). A chain that
// never settles is a bug in the test or the screen: capped loudly.
func settle(t *testing.T, s Screen, cmd tea.Cmd) {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i >= 64 {
			t.Fatal("async chain did not settle within 64 rounds (live tick chains must be fed manually)")
		}
		msg := cmd()
		if msg == nil {
			return
		}
		var next tea.Cmd
		s, next = s.Update(msg)
		cmd = next
	}
}

func testRelease(name, hex string, status torrent.Status, files ...torrent.FileEntry) torrent.Release {
	rel := torrent.Release{
		InfoHash:    metainfo.NewHashFromHex(hex),
		DisplayName: name,
		Status:      status,
		Quality:     torrent.ParseQuality(name),
		Files:       files,
	}
	return rel
}

var (
	hexA = "0123456789abcdef0123456789abcdef01234567"
	hexB = "fedcba9876543210fedcba9876543210fedcba98"
)

func testMultiFileRelease() torrent.Release {
	return testRelease("[SubsPlease] Title (01-02) (1080p)", hexA, torrent.StatusReady,
		torrent.FileEntry{Path: "Title - 01.mkv", Size: 100 << 20, Index: 0, Episodes: []int{1}},
		torrent.FileEntry{Path: "Title - 02.mkv", Size: 100 << 20, Index: 1, Episodes: []int{2}},
	)
}

func newTorrentDeps(svc TorrentService, pb PlaybackService) *Deps {
	return &Deps{Torrent: svc, Playback: pb}
}

func TestRootMenuHasTorrentsEntry(t *testing.T) {
	root := NewRootScreen(newTestDeps())
	view := root.View().Content
	if !strings.Contains(view, "🧲 Торренты") {
		t.Fatalf("root view must contain «🧲 Торренты», got:\n%s", view)
	}
	// PR35: the menu grows 5→6, Выход stays the pinned LAST row.
	items := root.list.Menu().Items
	if len(items) != 6 {
		t.Fatalf("root menu must hold 6 items, got %d: %+v", len(items), items)
	}
	if items[len(items)-1].ID != "exit" {
		t.Fatalf("Выход must stay the last item, got %+v", items[len(items)-1])
	}
}

func TestRootTorrentsPushesReleasesScreen(t *testing.T) {
	root := NewRootScreen(newTestDeps())
	idx := indexOfChoice(root, "torrents")
	if idx < 0 {
		t.Fatal("root must carry a torrents choice")
	}
	root.list.Jump(idx)
	_, cmd := root.Update(enter())
	if cmd == nil {
		t.Fatal("torrents must schedule navigation")
	}
	msg := cmd()
	pm, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("torrents must push a screen, got %#v", msg)
	}
	if pm.screen.ID() != torrentReleasesID {
		t.Fatalf("want screen %q, got %q", torrentReleasesID, pm.screen.ID())
	}
}

func TestTorrentReleasesDisabledHint(t *testing.T) {
	// nil service (embedded builds) and Enabled=false both show the
	// settings hint instead of failing or pretending.
	for name, deps := range map[string]*Deps{
		"nil service":  {},
		"disabled":     {Torrent: &fakeTorrent{enabled: false}},
		"error toggle": {Torrent: &fakeTorrent{enabled: true}},
	} {
		t.Run(name, func(t *testing.T) {
			screen := NewTorrentReleases(deps)
			view := screen.View().Content
			if !strings.Contains(view, "settings.toml") {
				t.Fatalf("disabled view must hint at settings.toml, got:\n%s", view)
			}
		})
	}
}

func TestTorrentReleasesEmptyLinksHint(t *testing.T) {
	ft := &fakeTorrent{enabled: true}
	screen := NewTorrentReleases(newTorrentDeps(ft, &fakePlayback{}))
	settle(t, screen, screen.Init())
	if ft.refreshes == 0 {
		t.Fatal("enabled service must be refreshed on entry")
	}
	view := screen.View().Content
	if !strings.Contains(view, "links") {
		t.Fatalf("empty list must hint at [torrent] links, got:\n%s", view)
	}
}

func TestTorrentReleasesColumnsAndBackPinned(t *testing.T) {
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{testMultiFileRelease()},
	}
	screen := NewTorrentReleases(newTorrentDeps(ft, &fakePlayback{}))
	settle(t, screen, screen.Init())
	view := screen.View().Content
	for _, want := range []string{
		"1080p",      // quality badge from the parsed name
		"SubsPlease", // release name
		"200",        // size: two 100 MiB files → MiB-based human size
		"готов",      // ready status in RU
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("releases view must contain %q, got:\n%s", want, view)
		}
	}
	items := screen.list.Menu().Items
	if len(items) == 0 || items[len(items)-1].ID != BackID {
		t.Fatalf("Back must be the pinned LAST row (I1), got %+v", items)
	}
}

func TestTorrentReleasesFetchingStatus(t *testing.T) {
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{testRelease("Magnet Only", hexB, torrent.StatusFetching)},
	}
	screen := NewTorrentReleases(newTorrentDeps(ft, &fakePlayback{}))
	// Feed the refresh manually: a fetching release keeps a live tick
	// chain running by design, so settle would never drain.
	cmd := screen.Init()
	if cmd == nil {
		t.Fatal("enabled service must schedule a refresh")
	}
	next, _ := screen.Update(cmd())
	screen = next.(*torrentReleasesScreen)
	if !strings.Contains(screen.View().Content, "метаданные") {
		t.Fatalf("fetching release must render its status, got:\n%s", screen.View().Content)
	}
}

func TestTorrentReleasesRefreshErrorShows(t *testing.T) {
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{testMultiFileRelease()},
		err:      errors.New("bad magnet link"),
	}
	screen := NewTorrentReleases(newTorrentDeps(ft, &fakePlayback{}))
	settle(t, screen, screen.Init())
	if !strings.Contains(screen.View().Content, "bad magnet link") {
		t.Fatalf("refresh error must surface in the status line, got:\n%s", screen.View().Content)
	}
}

func TestTorrentEnterMultiFilePushesFilesScreen(t *testing.T) {
	rel := testMultiFileRelease()
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{rel},
		files:    map[string][]torrent.FileEntry{hexA: rel.Files},
	}
	screen := NewTorrentReleases(newTorrentDeps(ft, &fakePlayback{}))
	settle(t, screen, screen.Init())
	screen.list.Jump(0)
	_, cmd := screen.Update(enter())
	if cmd == nil {
		t.Fatal("enter on a ready release must navigate")
	}
	msg := cmd()
	pm, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("multi-file release must push the files screen, got %#v", msg)
	}
	if pm.screen.ID() != torrentFilesID {
		t.Fatalf("want %q, got %q", torrentFilesID, pm.screen.ID())
	}
	// The files screen renders one row per file with episode labels.
	files := pm.screen.(*torrentFilesScreen)
	settle(t, files, files.Init())
	view := files.View().Content
	for _, want := range []string{"Серия 1", "Серия 2", "Title - 01.mkv"} {
		if !strings.Contains(view, want) {
			t.Fatalf("files view must contain %q, got:\n%s", want, view)
		}
	}
}

func TestTorrentSingleFileAutoPlays(t *testing.T) {
	rel := testRelease("Single Movie 1080p", hexB, torrent.StatusReady,
		torrent.FileEntry{Path: "Single Movie.mkv", Size: 700 << 20, Index: 0},
	)
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{rel},
		files:    map[string][]torrent.FileEntry{hexB: rel.Files},
	}
	pb := &fakePlayback{}
	screen := NewTorrentReleases(newTorrentDeps(ft, pb))
	settle(t, screen, screen.Init())
	screen.list.Jump(0)
	_, cmd := screen.Update(enter())
	if cmd == nil {
		t.Fatal("single-file release must go straight to playback")
	}
	settle(t, screen, cmd)
	if len(pb.played) != 1 {
		t.Fatalf("playback must start once, got %d", len(pb.played))
	}
	if pb.played[0].URL != ft.StreamURL(rel.InfoHash, 0) {
		t.Errorf("playback URL = %q, want the local stream URL %q", pb.played[0].URL, ft.StreamURL(rel.InfoHash, 0))
	}
	if !strings.Contains(pb.played[0].Title, "Single Movie 1080p") {
		t.Errorf("playback title %q must carry the release name", pb.played[0].Title)
	}
}

func TestTorrentFilesEnterPlays(t *testing.T) {
	rel := testMultiFileRelease()
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{rel},
		files:    map[string][]torrent.FileEntry{hexA: rel.Files},
	}
	pb := &fakePlayback{}
	screen := NewTorrentReleases(newTorrentDeps(ft, pb))
	settle(t, screen, screen.Init())
	screen.list.Jump(0)
	_, cmd := screen.Update(enter())
	pm := cmd().(pushMsg)
	files := pm.screen.(*torrentFilesScreen)
	settle(t, files, files.Init())
	files.list.Jump(1) // second episode
	_, playCmd := files.Update(enter())
	if playCmd == nil {
		t.Fatal("enter on a file must start playback")
	}
	settle(t, files, playCmd)
	if len(pb.played) != 1 {
		t.Fatalf("playback must start once, got %d", len(pb.played))
	}
	if pb.played[0].URL != ft.StreamURL(rel.InfoHash, 1) {
		t.Errorf("playback must target file index 1, got %q", pb.played[0].URL)
	}
	if !strings.Contains(pb.played[0].Title, "Серия 2") {
		t.Errorf("playback title %q must carry the episode label", pb.played[0].Title)
	}
}

func TestTorrentFilesBackPops(t *testing.T) {
	rel := testMultiFileRelease()
	ft := &fakeTorrent{
		enabled:  true,
		releases: []torrent.Release{rel},
		files:    map[string][]torrent.FileEntry{hexA: rel.Files},
	}
	screen := NewTorrentReleases(newTorrentDeps(ft, &fakePlayback{}))
	settle(t, screen, screen.Init())
	screen.list.Jump(0)
	_, cmd := screen.Update(enter())
	files := cmd().(pushMsg).screen.(*torrentFilesScreen)
	settle(t, files, files.Init())
	_, escCmd := files.Update(esc())
	if escCmd == nil {
		t.Fatal("esc must schedule navigation")
	}
	if _, ok := escCmd().(popMsg); !ok {
		t.Fatal("esc on the files screen must pop")
	}
}
