package tui

// PR74 owner ask: «надо поменять чтобы эмодзи разные были — смысл в
// том, чтобы не читая текст машинально находить нужную кнопку». These
// tests walk every screen's menu and pin the emoji discipline:
//  1. within a screen, no two rows share a leading emoji;
//  2. across screens, an emoji maps to exactly ONE action id — the
//     same action may repeat (▶ Смотреть, ⏭/⏮, 🔢, 🚪 Выход are
//     consistent by design), but one emoji meaning two different
//     actions breaks the muscle-memory navigation.
//
// Labels without a leading emoji (quality rows, «Статус», raw dub
// keys…) map to "" and are exempt. History rows carry bracketed
// badges, not emojis, and are not part of the emoji system.

import (
	"strings"
	"testing"
	"unicode"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// leadingEmoji extracts a label's leading emoji token (the runes
// before the first space); a label starting with a letter or digit
// carries no emoji and yields "".
func leadingEmoji(label string) string {
	runes := []rune(label)
	if len(runes) == 0 {
		return ""
	}
	if unicode.IsLetter(runes[0]) || unicode.IsDigit(runes[0]) {
		return ""
	}
	if i := strings.IndexRune(label, ' '); i >= 0 {
		return label[:i]
	}
	return label
}

// assertNoDuplicateEmoji fails when one screen shows the same emoji
// on two rows.
func assertNoDuplicateEmoji(t *testing.T, screen string, items []Choice) {
	t.Helper()
	seen := map[string]string{}
	for _, c := range items {
		e := leadingEmoji(c.Label)
		if e == "" {
			continue
		}
		if prev, dup := seen[e]; dup {
			t.Errorf("[%s] emoji %s is duplicated: %q and %q (id %s)",
				screen, e, prev, c.Label, c.ID)
			continue
		}
		seen[e] = c.Label
	}
}

func emojiActionPairs(t *testing.T, screen string, items []Choice, into map[string]string) {
	t.Helper()
	for _, c := range items {
		e := leadingEmoji(c.Label)
		if e == "" {
			continue
		}
		if prev, ok := into[e]; ok && prev != c.ID {
			t.Errorf("[%s] emoji %s means %q here but %q elsewhere — muscle-memory collision",
				screen, e, c.ID, prev)
			continue
		}
		into[e] = c.ID
	}
}

func resumedSessionForEmojiTests(t *testing.T) *sessionScreen {
	t.Helper()
	deps := &Deps{Episode: &fakeEpisode{episodes: testEpisodeSet()}}
	group := []contracts.SearchResult{
		{Title: "Тайтл", URL: "u1", SourceID: "animego"},
		{Title: "Тайтл", URL: "u2", SourceID: "anilib"},
	}
	rec := storage.AnimeProgress{ID: 9, Title: "Тайтл", CurrentEpisode: "1"}
	s := newResumedSession(deps, group[0], group, rec)
	s.loadEpisodesSync() // finalizeMerge → buildActionMenu with the resume rows
	return s
}

// TestSessionMenuEmojisUnique: the action menu shows no duplicated
// emoji — fresh AND resumed (the resumed menu adds «Перепривязать»,
// which shared 🔄 with «Обновить источники» until PR74).
func TestSessionMenuEmojisUnique(t *testing.T) {
	fresh := newSessionForTests(t)
	assertNoDuplicateEmoji(t, "session/fresh", fresh.list.Menu().Items)

	resumed := resumedSessionForEmojiTests(t)
	assertNoDuplicateEmoji(t, "session/resumed", resumed.list.Menu().Items)

	// The re-link action carries its own re-link emoji (🔗), not the
	// refresh 🔄.
	labels := map[string]string{}
	for _, c := range resumed.list.Menu().Items {
		labels[c.ID] = c.Label
	}
	if got := labels["rebind"]; got != "🔗 Перепривязать" {
		t.Fatalf("rebind label = %q, want 🔗 Перепривязать", got)
	}
	if got := labels["refresh"]; got != "🔄 Обновить источники" {
		t.Fatalf("refresh label = %q, want 🔄 Обновить источники", got)
	}
}

// TestOfflineMenuEmojisUnique: the offline action menu shares the
// playback verbs with the session (same actions — allowed) and adds
// the variant switch without reusing anything.
func TestOfflineMenuEmojisUnique(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}}
	titles, _ := deps.Offline.Titles()
	s := NewOfflineSession(deps, titles[0])
	assertNoDuplicateEmoji(t, "offline/actions", s.list.Menu().Items)
}

// TestAuxMenusEmojisUnique: download-mode, format, info, status and
// the stream pickers show no duplicated emoji.
func TestAuxMenusEmojisUnique(t *testing.T) {
	deps := &Deps{Episode: &fakeEpisode{episodes: testEpisodeSet()}}
	group := []contracts.SearchResult{{Title: "Тайтл", URL: "u1", SourceID: "animego"}}
	s := NewSessionScreen(deps, group[0], group)
	s.loadEpisodesSync()

	s.buildModeList()
	assertNoDuplicateEmoji(t, "session/mode", s.modeList.Menu().Items)
	s.state = sessionStateFormat
	s.formatList = NewPinList(NewMenu("Формат просмотра:", "", []Choice{
		{ID: "stream", Label: "Потоковый", Value: "stream"},
		{ID: "buffer", Label: "Буферный", Value: "buffer"},
	}...), defaultListHeight)
	assertNoDuplicateEmoji(t, "session/format", s.formatList.Menu().Items)

	s.videoDub = "[animego] Дубль 1"
	s.openAudioSelect()
	assertNoDuplicateEmoji(t, "session/audio", s.dubList.Menu().Items)
}

// TestRootAndSetupMenusEmojisUnique: root, db manage and the
// shikimori setup stay duplicate-free.
func TestRootAndSetupMenusEmojisUnique(t *testing.T) {
	root := NewRootScreen(&Deps{})
	assertNoDuplicateEmoji(t, "root", root.list.Menu().Items)

	db := NewDBMenu(&Deps{})
	assertNoDuplicateEmoji(t, "db", db.list.Menu().Items)

	setup := NewShikimoriSetup(&Deps{})
	assertNoDuplicateEmoji(t, "shiki-setup", setup.list.Menu().Items)
}

// TestEmojiMeansOneActionAcrossScreens: the cross-screen muscle-memory
// contract — an emoji may repeat only while it means the SAME action
// (▶/⏭/⏮/🔢 on session and offline, 🚪 on session and root).
func TestEmojiMeansOneActionAcrossScreens(t *testing.T) {
	pairs := map[string]string{}

	fresh := newSessionForTests(t)
	emojiActionPairs(t, "session/fresh", fresh.list.Menu().Items, pairs)

	resumed := resumedSessionForEmojiTests(t)
	emojiActionPairs(t, "session/resumed", resumed.list.Menu().Items, pairs)

	deps := &Deps{
		Episode:  &fakeEpisode{episodes: testEpisodeSet()},
		Offline:  &fakeOffline{titles: offlineFixture()},
		Playback: &fakePlayback{},
	}
	titles, _ := deps.Offline.Titles()
	offline := NewOfflineSession(deps, titles[0])
	emojiActionPairs(t, "offline", offline.list.Menu().Items, pairs)

	emojiActionPairs(t, "root", NewRootScreen(deps).list.Menu().Items, pairs)
	emojiActionPairs(t, "db", NewDBMenu(deps).list.Menu().Items, pairs)
	emojiActionPairs(t, "shiki-setup", NewShikimoriSetup(deps).list.Menu().Items, pairs)

	fresh.buildModeList()
	emojiActionPairs(t, "session/mode", fresh.modeList.Menu().Items, pairs)

	// The owner's pins: the refresh and the re-link are distinct
	// actions under distinct emojis.
	if pairs["🔄"] != "refresh" {
		t.Fatalf("🔄 = %q, want refresh only", pairs["🔄"])
	}
	if pairs["🔗"] != "rebind" {
		t.Fatalf("🔗 = %q, want rebind", pairs["🔗"])
	}
}
