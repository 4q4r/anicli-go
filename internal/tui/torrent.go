package tui

// Torrent screens (PR35): «🧲 Торренты» — the release list built from
// the configured links, then the file/episode list of one release.
// Playback reuses the exact provider plumbing (PlaybackService.Play)
// with the engine's loopback stream URL, so mpv integration, quality
// handling and the logger-to-file convention come for free. Metadata
// fetches happen in the engine's background; the screens only render
// snapshots and never block on the network.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/torrent"
)

// Screen ids.
const (
	torrentReleasesID = "torrent_releases"
	torrentFilesID    = "torrent_files"
)

// torrentSettingsHint is the disabled/idle empty state: the engine
// starts lazily, so an empty [torrent] links list is a hint, not an
// error.
const torrentSettingsHint = "Торренты отключены или список пуст.\n" +
	"Добавьте magnet-ссылки в раздел [torrent] файла settings.toml:\n" +
	"  links = [\"magnet:?xt=urn:btih:...\"]"

// torrentRefreshInterval paces the live status refresh while some
// release is still fetching its metadata.
const torrentRefreshInterval = time.Second

// torrentReleasesMsg settles one refresh of the release snapshot.
type torrentReleasesMsg struct {
	releases []torrent.Release
	err      error
}

// torrentTickMsg schedules the next refresh while something is still
// fetching.
type torrentTickMsg struct{}

// torrentPlayedMsg settles one playback launch.
type torrentPlayedMsg struct {
	rel  torrent.Release
	file *torrent.FileEntry
	err  error
}

// torrentReleasesScreen lists the added releases: quality badge, name,
// size, live status — Back pinned at the BOTTOM (I1).
type torrentReleasesScreen struct {
	deps     *Deps
	list     *PinList
	releases []torrent.Release
	errText  string
	launched string
}

// NewTorrentReleases builds the releases screen.
//
//nolint:revive // internal screen type
func NewTorrentReleases(deps *Deps) *torrentReleasesScreen {
	s := &torrentReleasesScreen{deps: deps}
	s.rebuildList()
	return s
}

// ID implements Screen.
func (s *torrentReleasesScreen) ID() string { return torrentReleasesID }

// Init implements Screen: kick the first refresh (the service ingests
// the configured links once, then serves cheap snapshots).
func (s *torrentReleasesScreen) Init() tea.Cmd {
	if !s.available() {
		return nil
	}
	return safeCmd(torrentReleasesID, s.refreshCmd())
}

// available reports whether a working torrent service is wired.
func (s *torrentReleasesScreen) available() bool {
	return s.deps != nil && s.deps.Torrent != nil && s.deps.Torrent.Enabled()
}

func (s *torrentReleasesScreen) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		releases, err := s.deps.Torrent.Refresh(context.Background())
		return torrentReleasesMsg{releases: releases, err: err}
	}
}

// rebuildList re-renders the choices from the current snapshot; Back
// stays appended LAST (I1).
func (s *torrentReleasesScreen) rebuildList() {
	choices := make([]Choice, 0, len(s.releases))
	for _, rel := range s.releases {
		choices = append(choices, Choice{
			ID:    rel.InfoHash.HexString(),
			Label: s.releaseLabel(rel),
			Value: rel,
		})
	}
	list := NewPinList(NewMenu("🧲 Торренты", torrentSettingsHint, choices...), defaultListHeight)
	if s.list != nil {
		list.Jump(s.list.Cursor())
	}
	s.list = list
}

// releaseLabel renders one list row: quality badge, name, size, status.
func (s *torrentReleasesScreen) releaseLabel(rel torrent.Release) string {
	size := "—"
	if total := releaseSize(rel); total > 0 {
		size = formatBytes(total)
	}
	return fmt.Sprintf("[%s] %s · %s · %s",
		rel.Quality.Badge(), rel.DisplayName, size, statusLabel(rel))
}

// statusLabel renders the live status in the list vocabulary.
func statusLabel(rel torrent.Release) string {
	switch rel.Status {
	case torrent.StatusReady:
		return "✓ готов"
	case torrent.StatusError:
		return "⚠ ошибка"
	default:
		return "⏳ метаданные"
	}
}

// releaseSize sums the known file sizes.
func releaseSize(rel torrent.Release) int64 {
	var total int64
	for _, f := range rel.Files {
		total += f.Size
	}
	return total
}

// formatBytes renders a byte count in binary units.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Update implements Screen: navigation drives the list; a settled
// refresh re-renders and, while any release is still fetching,
// schedules the next tick so statuses stay live.
func (s *torrentReleasesScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case torrentReleasesMsg:
		if msg.err != nil {
			s.errText = msg.err.Error()
		} else {
			s.errText = ""
		}
		s.releases = msg.releases
		s.rebuildList()
		return s, s.tickIfFetching()
	case torrentTickMsg:
		if !s.available() {
			return s, nil
		}
		return s, safeCmd(torrentReleasesID, s.refreshCmd())
	case tea.KeyPressMsg:
		if s.list.HandleKey(msg) {
			return s, nil
		}
		resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), msg)
		if resolved == nil {
			return s, nil
		}
		if resolved == Back {
			return s, pop()
		}
		rel, ok := resolved.(torrent.Release)
		if !ok {
			return s, nil
		}
		return s, s.pick(rel)
	case torrentPlayedMsg:
		// Playback settles here: failures surface in the status line,
		// successes echo the release — never a silent drop.
		if msg.err != nil {
			s.errText = msg.err.Error()
			return s, nil
		}
		s.errText = ""
		s.launched = "запущено: " + msg.rel.DisplayName
		return s, nil
	default:
		return s, nil
	}
}

// tickIfFetching keeps the refresh chain alive only while it matters.
func (s *torrentReleasesScreen) tickIfFetching() tea.Cmd {
	for _, rel := range s.releases {
		if rel.Status == torrent.StatusFetching {
			return tea.Tick(torrentRefreshInterval, func(time.Time) tea.Msg { return torrentTickMsg{} })
		}
	}
	return nil
}

// pick handles Enter on one release: multi-file releases open the file
// list, single-file ones go straight to playback ("auto-skip").
func (s *torrentReleasesScreen) pick(rel torrent.Release) tea.Cmd {
	switch {
	case rel.Status != torrent.StatusReady:
		return nil
	case len(rel.Files) == 1:
		return s.playCmd(rel, &rel.Files[0])
	case len(rel.Files) > 1:
		return push(NewTorrentFiles(s.deps, rel))
	default:
		return nil
	}
}

// playCmd launches mpv through the SAME PlaybackService the providers
// use, pointing it at the engine's loopback stream URL.
func (s *torrentReleasesScreen) playCmd(rel torrent.Release, file *torrent.FileEntry) tea.Cmd {
	return func() tea.Msg {
		err := playTorrentFile(context.Background(), s.deps, rel, file)
		return torrentPlayedMsg{rel: rel, file: file, err: err}
	}
}

// playTorrentFile resolves the stream URL and hands it to the player.
func playTorrentFile(ctx context.Context, deps *Deps, rel torrent.Release, file *torrent.FileEntry) error {
	if deps == nil || deps.Torrent == nil || deps.Playback == nil {
		return errors.New("торренты: сервис воспроизведения недоступен")
	}
	url := deps.Torrent.StreamURL(rel.InfoHash, file.Index)
	if url == "" {
		return errors.New("торренты: локальный стрим-сервер не запущен")
	}
	title := torrentTitle(rel, file)
	return deps.Playback.Play(ctx, PlayRequest{URL: url, Title: title})
}

// torrentTitle composes the mpv media title.
func torrentTitle(rel torrent.Release, file *torrent.FileEntry) string {
	if file != nil && len(file.Episodes) == 1 {
		return fmt.Sprintf("%s — Серия %d", rel.DisplayName, file.Episodes[0])
	}
	return rel.DisplayName
}

// View implements Screen: hint states replace the list, refresh errors
// surface in the status line, and the last launched release is echoed.
func (s *torrentReleasesScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Title.Render("🧲 Торренты")...)
	b = append(b, '\n', '\n')
	if !s.available() {
		b = append(b, theme.Dim.Render(torrentSettingsHint)...)
		return tea.NewView(string(b))
	}
	if len(s.releases) == 0 && s.errText == "" {
		b = append(b, theme.Dim.Render(torrentSettingsHint)...)
	}
	b = append(b, s.list.Render()...)
	status := s.statusLine()
	if status != "" {
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render(status)...)
	}
	return tea.NewView(string(b))
}

// statusLine renders the bottom line: errors win over the launch
// echo, the fetch note wins over nothing in particular.
func (s *torrentReleasesScreen) statusLine() string {
	switch {
	case s.errText != "":
		return "⚠ " + s.errText
	case s.launched != "":
		return "▶ " + s.launched
	case s.anyFetching():
		return "⏳ Получение метаданных… (статус обновляется автоматически)"
	default:
		return rootBackHint
	}
}

func (s *torrentReleasesScreen) anyFetching() bool {
	for _, rel := range s.releases {
		if rel.Status == torrent.StatusFetching {
			return true
		}
	}
	return false
}

// torrentFilesScreen lists the files of one release with episode
// numbers from the release-name parsing when available; Back pinned at
// the BOTTOM (I1).
type torrentFilesScreen struct {
	deps *Deps
	rel  torrent.Release
	list *PinList
	// errText carries the last playback failure; launched the last
	// success echo (same contract as the releases screen).
	errText  string
	launched string
}

// NewTorrentFiles builds the files screen for one ready release.
//
//nolint:revive // internal screen type
func NewTorrentFiles(deps *Deps, rel torrent.Release) *torrentFilesScreen {
	s := &torrentFilesScreen{deps: deps, rel: rel}
	choices := make([]Choice, 0, len(rel.Files))
	for _, f := range rel.Files {
		choices = append(choices, Choice{
			ID:    fmt.Sprintf("%d", f.Index),
			Label: fileLabel(f),
			Value: f,
		})
	}
	s.list = NewPinList(NewMenu("🧲 "+rel.DisplayName, "Выберите файл:", choices...), defaultListHeight)
	return s
}

// ID implements Screen.
func (s *torrentFilesScreen) ID() string { return torrentFilesID }

// Init implements Screen.
func (s *torrentFilesScreen) Init() tea.Cmd { return nil }

// fileLabel renders one file row: episode label when the parser found
// one, else the raw path — always with the size.
func fileLabel(f torrent.FileEntry) string {
	name := f.Path
	if dot := strings.LastIndexByte(name, '/'); dot >= 0 {
		name = name[dot+1:]
	}
	if len(f.Episodes) == 1 {
		return fmt.Sprintf("Серия %d · %s · %s", f.Episodes[0], name, formatBytes(f.Size))
	}
	return fmt.Sprintf("%s · %s", name, formatBytes(f.Size))
}

// Update implements Screen: navigation, Enter plays the file, Esc pops.
func (s *torrentFilesScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case torrentPlayedMsg:
		// Same settled-error contract as the releases screen: a dead
		// mpv or a dead URL is visible, never swallowed.
		if msg.err != nil {
			s.errText = msg.err.Error()
			return s, nil
		}
		s.errText = ""
		if msg.file != nil {
			s.launched = "запущено: " + fileLabel(*msg.file)
		}
		return s, nil
	case tea.KeyPressMsg:
		if s.list.HandleKey(msg) {
			return s, nil
		}
		resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), msg)
		if resolved == nil {
			return s, nil
		}
		if resolved == Back {
			return s, pop()
		}
		file, ok := resolved.(torrent.FileEntry)
		if !ok {
			return s, nil
		}
		return s, func() tea.Msg {
			err := playTorrentFile(context.Background(), s.deps, s.rel, &file)
			return torrentPlayedMsg{rel: s.rel, file: &file, err: err}
		}
	default:
		return s, nil
	}
}

// View implements Screen.
func (s *torrentFilesScreen) View() tea.View {
	var b []byte
	b = append(b, theme.Title.Render("🧲 "+s.rel.DisplayName)...)
	b = append(b, '\n', '\n')
	b = append(b, s.list.Render()...)
	switch {
	case s.errText != "":
		b = append(b, '\n')
		b = append(b, theme.Error.Render("⚠ "+s.errText)...)
	case s.launched != "":
		b = append(b, '\n')
		b = append(b, theme.StatusLine.Render("▶ "+s.launched)...)
	}
	return tea.NewView(string(b))
}
