package tui

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// rangesBindFixture builds a media folder with two name groups and a
// stray file (the suggestion heuristic's canonical input).
func rangesBindFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Сериал")
	for _, f := range []string{"EP_1_a.mkv", "EP_2_a.mkv", "OVA_1.mkv", "stray.mkv"} {
		if err := writeFixtureFile(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// newBindEditorFor builds the editor over a fixture dir.
func newBindEditorFor(t *testing.T, deps *Deps, dir string) *bindEditor {
	t.Helper()
	return NewBindEditor(deps, OfflineTitle{Dir: dir, Name: filepath.Base(dir), RelDir: filepath.Base(dir)})
}

// bindRows returns the editor menu's current rows (Back excluded).
func bindRows(e *bindEditor) []Choice {
	items := e.list.Menu().Items
	if len(items) > 0 && items[len(items)-1].ID == BackID {
		items = items[:len(items)-1]
	}
	return items
}

// resolvePick jumps onto the row with the given id, presses Enter and
// returns the scheduled command (nil when the pick only mutated state).
func resolvePick(t *testing.T, screen Screen, list *PinList, id string) tea.Cmd {
	t.Helper()
	idx := -1
	for i, c := range list.Menu().Items {
		if c.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("row %q missing among %+v", id, list.Menu().Items)
	}
	list.Jump(idx)
	_, cmd := screen.Update(enter())
	return cmd
}

// pushedScreen unwraps a pushMsg carrying the pushed screen.
func pushedScreen(t *testing.T, cmd tea.Cmd) Screen {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a push command, got nil")
	}
	msg := cmd()
	pushed, ok := msg.(pushMsg)
	if !ok {
		t.Fatalf("expected pushMsg, got %#v", msg)
	}
	return pushed.screen
}

// TestBindEditorInitialSuggestions: a folder without a ranges file
// stages the disposable SuggestRanges output and renders the rows.
func TestBindEditorInitialSuggestions(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{}, dir)

	if len(e.staged) != 3 {
		t.Fatalf("staged = %d (%+v), want 3 groups (EP_, OVA_, and the digit-free stray as its own prefix group)", len(e.staged), e.staged)
	}
	if e.staged[0].ID != "r1" || e.staged[0].Title != "EP" {
		t.Errorf("first = %s/%s, want r1/EP", e.staged[0].ID, e.staged[0].Title)
	}
	if e.staged[1].ID != "r2" || e.staged[1].Title != "OVA" {
		t.Errorf("second = %s/%s, want r2/OVA", e.staged[1].ID, e.staged[1].Title)
	}
	rows := bindRows(e)
	// 3 range rows + add + save.
	if len(rows) != 5 {
		t.Fatalf("rows = %d (%+v), want 5", len(rows), rows)
	}
	v := e.View().Content
	if !strings.Contains(v, "EP") || !strings.Contains(v, "OVA") {
		t.Errorf("range rows must render:\n%s", v)
	}
}

// TestBindEditorExistingFilePrefill: a folder WITH a ranges file
// stages its contents verbatim (edit mode).
func TestBindEditorExistingFilePrefill(t *testing.T) {
	dir := rangesBindFixture(t)
	want := download.RangesFile{Version: 1, Ranges: []download.RangeBinding{
		{ID: "s1", Title: "Сериал — S1", ShikimoriID: 39535, Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}}
	if err := download.SaveRanges(dir, &want); err != nil {
		t.Fatal(err)
	}
	e := newBindEditorFor(t, &Deps{}, dir)
	if len(e.staged) != 1 {
		t.Fatalf("staged = %+v, want the file's single range", e.staged)
	}
	if !reflect.DeepEqual(e.staged[0], want.Ranges[0]) {
		t.Errorf("staged = %+v, want %+v", e.staged[0], want.Ranges[0])
	}
}

// openRangeDetail drives the real routing: the range row pick pushes
// the detail screen.
func openRangeDetail(t *testing.T, e *bindEditor, idx int) *rangeDetail {
	t.Helper()
	detail, ok := pushedScreen(t, resolvePick(t, e, e.list, e.staged[idx].ID)).(*rangeDetail)
	if !ok {
		t.Fatal("the range row must push the range detail")
	}
	return detail
}

// TestBindEditorSaveHappyPath: saving writes the file on disk and
// settles as bindSavedMsg{nil}; the unwind sequence is pinned by
// TestBindEditorSaveUnwindSequence.
func TestBindEditorSaveHappyPath(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "Сериал — S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()

	cmd := resolvePick(t, e, e.list, "save")
	if cmd == nil {
		t.Fatal("save must schedule a command")
	}
	msg := cmd()
	if saved, ok := msg.(bindSavedMsg); !ok || saved.err != nil {
		t.Fatalf("save msg = %#v, want bindSavedMsg{nil}", msg)
	}
	onDisk, err := download.LoadRanges(dir)
	if err != nil || onDisk == nil || len(onDisk.Ranges) != 1 {
		t.Fatalf("ranges file = %+v, err %v; want the staged range", onDisk, err)
	}
}

// TestBindEditorSaveUnwindSequence: the successful save schedules
// popToRoot + a fresh NewOfflineTitles push (the stale-picker guard,
// Review Focus #3).
func TestBindEditorSaveUnwindSequence(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	cmd := resolvePick(t, e, e.list, "save")
	saved := cmd()
	next, unwind := e.Update(saved)
	if next != Screen(e) {
		t.Fatalf("save handler must stay on the editor, got %#v", next)
	}
	if unwind == nil {
		t.Fatal("successful save must schedule the unwind")
	}
	cmds := unwrapCmdSlice(t, unwind())
	if len(cmds) != 2 {
		t.Fatalf("unwind = %d commands, want [popToRoot, push titles]", len(cmds))
	}
	first, second := cmds[0](), cmds[1]()
	if _, ok := first.(popToRootMsg); !ok {
		t.Errorf("first unwind msg = %#v, want popToRootMsg", first)
	}
	pushed, ok := second.(pushMsg)
	if !ok {
		t.Fatalf("second unwind msg = %#v, want pushMsg", second)
	}
	if pushed.screen.ID() != offlineTitlesID {
		t.Errorf("pushed screen = %s, want the fresh offline titles", pushed.screen.ID())
	}
}

// unwrapCmdSlice unwraps a tea.Sequence carrier message
// (bubbletea's unexported sequenceMsg ~[]Cmd) via reflection.
func unwrapCmdSlice(t *testing.T, msg tea.Msg) []tea.Cmd {
	t.Helper()
	if cmds, ok := msg.([]tea.Cmd); ok {
		return cmds
	}
	rv := reflect.ValueOf(msg)
	if rv.Kind() != reflect.Slice {
		t.Fatalf("sequence msg %#v is not a slice", msg)
	}
	out := make([]tea.Cmd, 0, rv.Len())
	for i := range rv.Len() {
		cmd, ok := rv.Index(i).Interface().(tea.Cmd)
		if !ok {
			t.Fatalf("sequence element %d is not a Cmd", i)
		}
		out = append(out, cmd)
	}
	return out
}

// TestBindEditorDuplicateShikiRejected: two staged ranges bound to
// one Shikimori entry hard-reject the save (no write, typed status).
func TestBindEditorDuplicateShikiRejected(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatal(err)
	}
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "a", Title: "A", ShikimoriID: 55888, Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "b", Title: "B", ShikimoriID: 55888, Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}
	e.buildList()
	cmd := resolvePick(t, e, e.list, "save")
	if cmd != nil {
		t.Fatal("a rejected save must not schedule anything")
	}
	if e.confirm != nil {
		t.Fatal("dup shikimori must hard-reject before the overlap confirm")
	}
	if !strings.Contains(e.status, "уже привязана") {
		t.Errorf("status = %q, want the dup-shiki verdict", e.status)
	}
	if _, err := os.Stat(filepath.Join(dir, download.RangesFilename)); !os.IsNotExist(err) {
		t.Error("the rejected document must not be written")
	}
}

// TestBindEditorOverlapConfirm: overlapping ranges raise the confirm
// substate — ✅ writes, ✖ returns without writing.
func TestBindEditorOverlapConfirm(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "g", Title: "G", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "x", Title: "X", Match: download.RangeMatch{Kind: "exact", Files: []string{"EP_1_a.mkv"}}},
	}
	e.buildList()
	cmd := resolvePick(t, e, e.list, "save")
	if cmd != nil || e.confirm == nil {
		t.Fatalf("overlapping ranges must raise the confirm substate (cmd %v)", cmd != nil)
	}

	// ✖ Отмена: back to the editor, no file, and the «⚠ пересекаются»
	// status clears with the confirm (n2).
	e.confirm.Jump(1)
	e.Update(enter()) //nolint:errcheck // clears the confirm in place
	if e.confirm != nil {
		t.Fatal("cancel must leave the confirm")
	}
	if e.status != "" {
		t.Errorf("status = %q, want cleared after the cancel", e.status)
	}
	if _, err := os.Stat(filepath.Join(dir, download.RangesFilename)); !os.IsNotExist(err) {
		t.Fatal("cancel must not write the file")
	}

	// ✅ Сохранить: the file lands.
	cmd = resolvePick(t, e, e.list, "save")
	if cmd != nil || e.confirm == nil {
		t.Fatal("confirm must re-arm on the next save pick")
	}
	e.confirm.Jump(0)
	_, cmd = e.Update(enter())
	if cmd == nil {
		t.Fatal("confirmed save must schedule the write")
	}
	msg := cmd()
	if saved, ok := msg.(bindSavedMsg); !ok || saved.err != nil {
		t.Fatalf("save msg = %#v, want bindSavedMsg{nil}", msg)
	}
	if _, err := download.LoadRanges(dir); err != nil {
		t.Fatalf("confirmed save must write the file: %v", err)
	}
}

// TestRangeDetailShikiSearchSetsBinding: the search prompt resolves
// candidates through Shiki.SearchIDs; picking one binds the staged
// range, the skip row leaves it unbound.
func TestRangeDetailShikiSearchSetsBinding(t *testing.T) {
	dir := rangesBindFixture(t)
	shiki := &fakeShiki{enabled: true, ids: map[string]int64{
		"Тайтл S1": 39535,
		"Тайтл S2": 55888,
	}}
	deps := &Deps{Shiki: shiki, Offline: &fakeOffline{}}
	e := newBindEditorFor(t, deps, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "r2", Title: "S2", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}
	e.buildList()

	detail := openRangeDetail(t, e, 0)
	search, ok := pushedScreen(t, resolvePick(t, detail, detail.list, "bind")).(*shikiSearch)
	if !ok {
		t.Fatal("bind must push the shiki search")
	}
	if len(shiki.queries) != 0 {
		t.Fatal("the search must fire on submit, not on open")
	}
	search.prompt.typeText("Тайтл")
	_, cmd := search.Update(enter())
	if cmd == nil {
		t.Fatal("submit must schedule the lookup")
	}
	settled := cmd() // runs the bounded lookup
	if len(shiki.queries) != 1 || shiki.queries[0] != "Тайтл" {
		t.Fatalf("queries = %v, want [Тайтл]", shiki.queries)
	}
	search.Update(settled) //nolint:errcheck // settles the results menu
	rows := search.results.Menu().Items
	// 2 candidates + skip + Back.
	if len(rows) != 4 {
		t.Fatalf("result rows = %d, want 4", len(rows))
	}
	search.results.Jump(0)
	search.Update(enter()) //nolint:errcheck // the pick pops back
	if e.staged[0].ShikimoriID != 39535 || e.staged[0].ShikimoriTitle != "Тайтл S1" {
		t.Errorf("binding = %d/%q, want 39535/Тайтл S1", e.staged[0].ShikimoriID, e.staged[0].ShikimoriTitle)
	}

	// The skip row leaves the range unbound.
	detail2 := openRangeDetail(t, e, 1)
	search2, ok := pushedScreen(t, resolvePick(t, detail2, detail2.list, "bind")).(*shikiSearch)
	if !ok {
		t.Fatal("bind must push the shiki search")
	}
	search2.prompt.typeText("Тайтл")
	_, cmd2 := search2.Update(enter())
	search2.Update(cmd2())  //nolint:errcheck // settles the results
	search2.results.Jump(2) // the skip row
	search2.Update(enter()) //nolint:errcheck // the skip pick pops
	if e.staged[1].ShikimoriID != 0 {
		t.Errorf("skip bound the range: %d", e.staged[1].ShikimoriID)
	}
}

// TestFileOrderPickerOrderIsSelectionOrder: the selection order IS
// the episode order; re-toggling removes.
func TestFileOrderPickerOrderIsSelectionOrder(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "exact"}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	picker, ok := pushedScreen(t, resolvePick(t, detail, detail.list, "files")).(*fileOrderPicker)
	if !ok {
		t.Fatal("files must push the file order picker")
	}
	files := picker.editor.files
	if len(files) < 3 {
		t.Fatalf("scan = %v", files)
	}
	toggle := func(name string) {
		t.Helper()
		for i, c := range picker.list.Menu().Items {
			if c.ID == "file:"+name {
				picker.list.Jump(i)
				picker.Update(enter()) //nolint:errcheck // toggles in place
				return
			}
		}
		t.Fatalf("row for %s missing", name)
	}
	toggle("EP_2_a.mkv")
	toggle("EP_1_a.mkv")
	got := e.staged[0].Match.Files
	if !reflect.DeepEqual(got, []string{"EP_2_a.mkv", "EP_1_a.mkv"}) {
		t.Fatalf("files = %v, want the selection order", got)
	}
	toggle("EP_2_a.mkv")
	if got := e.staged[0].Match.Files; !reflect.DeepEqual(got, []string{"EP_1_a.mkv"}) {
		t.Fatalf("after untoggle = %v, want [EP_1_a.mkv]", got)
	}
}

// TestOffsetPromptValidation: a non-integer surfaces the typed error
// and changes nothing; an integer applies.
func TestOffsetPromptValidation(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	resolvePick(t, detail, detail.list, "offset")
	if detail.prompt == nil {
		t.Fatal("offset must open a prompt")
	}
	prompt := detail.prompt
	prompt.typeText("abc")
	detail.Update(enter()) //nolint:errcheck // invalid submit
	if !strings.Contains(detail.status, "целое число") && !strings.Contains(detail.status, "whole number") {
		t.Errorf("status = %q, want the invalid-number verdict", detail.status)
	}
	if e.staged[0].EpisodeOffset != 0 {
		t.Errorf("offset = %d, want unchanged", e.staged[0].EpisodeOffset)
	}

	resolvePick(t, detail, detail.list, "offset")
	prompt = detail.prompt
	prompt.typeText("12")
	detail.Update(enter()) //nolint:errcheck // valid submit
	if e.staged[0].EpisodeOffset != 12 {
		t.Errorf("offset = %d, want 12", e.staged[0].EpisodeOffset)
	}
}

// TestOfflineSessionBindActionRow: the ✂️ row lives on plain titles
// only; routing pushes the editor.
func TestOfflineSessionBindActionRow(t *testing.T) {
	deps := &Deps{Offline: &fakeOffline{titles: offlineFixture()}}
	titles, _ := deps.Offline.Titles()
	plain := NewOfflineSession(deps, titles[0])
	bindIdx := -1
	for i, c := range plain.list.Menu().Items {
		if c.ID == "bind" {
			bindIdx = i
		}
	}
	if bindIdx < 0 {
		t.Fatal("a plain title's action menu must offer the ✂️ bind row")
	}
	if !strings.Contains(plain.list.Menu().Items[bindIdx].Label, "✂️") {
		t.Errorf("bind label = %q, want the ✂️ marker", plain.list.Menu().Items[bindIdx].Label)
	}
	// The action menu sits behind the episode picker: pick an episode
	// first (the watch-test drive).
	plain.episodeList.Jump(0)
	next, _ := plain.Update(enter())
	session := next.(*offlineSession)
	session.list.Jump(bindIdx)
	_, cmd := session.Update(enter())
	if cmd == nil {
		t.Fatal("the bind pick must push the editor")
	}
	if _, ok := pushedScreen(t, cmd).(*bindEditor); !ok {
		t.Error("the bind pick must push the bind editor")
	}

	// A range title's session KEEPS the ✂️ row (review M3): a
	// fully-bound folder renders only range titles, so each of them is
	// an edit path — without the row the owner could never rebind.
	ranged := titles[0]
	ranged.Range = &download.RangeBinding{ID: "s1", Title: "S1"}
	rangedSession := NewOfflineSession(deps, ranged)
	if offlineActionIndex(rangedSession, "bind") < 0 {
		t.Error("a range title's session must offer the bind row (the only edit path of a fully-bound folder)")
	}
}

// TestBindEditorConfirmViewRenders (M1): while the overlap confirm is
// armed the editor must RENDER the confirm — the question text and the
// ✅/✖ rows — instead of the stale rows list silently intercepting
// every key.
func TestBindEditorConfirmViewRenders(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatal(err)
	}
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "g", Title: "G", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "x", Title: "X", Match: download.RangeMatch{Kind: "exact", Files: []string{"EP_1_a.mkv"}}},
	}
	e.buildList()
	if cmd := resolvePick(t, e, e.list, "save"); cmd != nil || e.confirm == nil {
		t.Fatalf("the overlap confirm must arm (cmd %v)", cmd != nil)
	}
	v := e.View().Content
	if !strings.Contains(v, "пересекаются") && !strings.Contains(v, "overlap") {
		t.Errorf("the armed confirm must render its question:\n%s", v)
	}
	if !strings.Contains(v, "Отмена") && !strings.Contains(v, "Cancel") {
		t.Errorf("the armed confirm must render its ✅/✖ rows:\n%s", v)
	}
}

// TestBindEditorRealGlobOverlapConfirmed (m2): glob×glob overlaps are
// invisible to the static check but REAL — evaluated against the
// editor's own scan they route through the same confirm.
func TestBindEditorRealGlobOverlapConfirmed(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "g1", Title: "G1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "g2", Title: "G2", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_1*"}}},
	}
	e.buildList()
	if len(download.RangeOverlaps(&download.RangesFile{Version: 1, Ranges: e.staged})) != 0 {
		t.Fatal("precondition: the static check must see nothing here")
	}
	cmd := resolvePick(t, e, e.list, "save")
	if cmd != nil || e.confirm == nil {
		t.Fatalf("a real glob×glob overlap must raise the confirm (cmd %v)", cmd != nil)
	}
	if !strings.Contains(e.status, "EP_1_a.mkv") {
		t.Errorf("status = %q, want the really-matched file listed", e.status)
	}
	// Confirming still lands the write (overlap = legal after confirm).
	e.confirm.Jump(0)
	_, cmd = e.Update(enter())
	if cmd == nil {
		t.Fatal("confirmed save must schedule the write")
	}
	if saved, ok := cmd().(bindSavedMsg); !ok || saved.err != nil {
		t.Fatalf("save msg = %#v, want bindSavedMsg{nil}", cmd())
	}
}

// TestBindEditorMissingExactFilesConfirmed (m3): an exact list naming
// files absent from the folder warns through the same confirm instead
// of saving silently.
func TestBindEditorMissingExactFilesConfirmed(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "x", Title: "X", Match: download.RangeMatch{Kind: "exact", Files: []string{"EP_1_a.mkv", "ghost_99.mkv"}}},
	}
	e.buildList()
	cmd := resolvePick(t, e, e.list, "save")
	if cmd != nil || e.confirm == nil {
		t.Fatalf("missing exact files must raise the confirm (cmd %v)", cmd != nil)
	}
	if !strings.Contains(e.status, "ghost_99.mkv") {
		t.Errorf("status = %q, want the missing filename listed", e.status)
	}
	// Confirming proceeds; the present file stays in the list.
	e.confirm.Jump(0)
	_, cmd = e.Update(enter())
	if msg, ok := cmd().(bindSavedMsg); !ok || msg.err != nil {
		t.Fatalf("confirmed save msg = %#v, want bindSavedMsg{nil}", cmd())
	}
}

// TestBindEditorUnsupportedVersionBlocked (m4): a foreign-version
// ranges file opens the editor in a blocked state — the load error is
// the status, no suggestions are seeded over the unreadable file and
// saving is disabled.
func TestBindEditorUnsupportedVersionBlocked(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatal(err)
	}
	dir := rangesBindFixture(t)
	if err := os.WriteFile(filepath.Join(dir, download.RangesFilename),
		[]byte(`{"version": 99, "ranges": [{"id": "foreign", "title": "F", "match": {"kind": "glob", "patterns": ["EP_*"]}}]}`),
		0o600); err != nil {
		t.Fatal(err)
	}
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	if e.loadErr == nil {
		t.Fatal("the editor must surface the load error, not stage suggestions")
	}
	if len(e.staged) != 0 {
		t.Errorf("staged = %+v, want nothing seeded over the unreadable file", e.staged)
	}
	if !strings.Contains(e.status, "версия") && !strings.Contains(e.status, "version") {
		t.Errorf("status = %q, want the unsupported-version verdict", e.status)
	}
	for _, row := range bindRows(e) {
		if row.ID == "save" {
			t.Error("save must be disabled while the file is unreadable")
		}
	}
	// Defense in depth: save() refuses even if invoked directly.
	if cmd := e.save(); cmd != nil {
		t.Error("save() must refuse while the load error stands")
	}
	if _, err := os.Stat(filepath.Join(dir, download.RangesFilename)); err != nil {
		t.Errorf("the foreign file must stay untouched: %v", err)
	}
}

// TestRenamePromptRejectsEmpty (m6): an empty rename cannot reach the
// staged title — the shared ResolveText maps an empty submit to Back
// (cancel), so the title stays unchanged and no invalid state is
// staged. Pinned here: the prompt-level rejection is the structural
// guard; a whitespace-only submit behaves as a cancel, not a staged
// empty title.
func TestRenamePromptRejectsEmpty(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	resolvePick(t, detail, detail.list, "rename")
	detail.prompt.typeText("   ") // whitespace-only = empty
	detail.Update(enter())        //nolint:errcheck // empty submit resolves to Back
	if e.staged[0].Title != "S1" {
		t.Errorf("title = %q, want unchanged S1 (an empty rename must not stage)", e.staged[0].Title)
	}
	if detail.prompt != nil {
		t.Error("an empty submit resolves to Back: the prompt must close")
	}
}

// TestOffsetPromptRejectsNegative (M5, editor leg): a negative episode
// offset is meaningless for ranges — the prompt rejects it with an
// i18n status instead of staging it.
func TestOffsetPromptRejectsNegative(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatal(err)
	}
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	resolvePick(t, detail, detail.list, "offset")
	detail.prompt.typeText("-3")
	detail.Update(enter()) //nolint:errcheck // rejected submit
	if e.staged[0].EpisodeOffset != 0 {
		t.Errorf("offset = %d, want unchanged (negative offsets are rejected)", e.staged[0].EpisodeOffset)
	}
	if strings.TrimSpace(detail.status) == "" {
		t.Error("the rejected offset must surface an i18n status")
	}
}

// TestModeToggleClearsQuality (n3): toggling exact→glob must clear
// Match.Quality — a leftover quality glob with no patterns is a
// guaranteed hard-reject at save.
func TestModeToggleClearsQuality(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{
			Kind: "exact", Files: []string{"EP_1_a.mkv"}, Quality: "1080p"}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	resolvePick(t, detail, detail.list, "mode") // exact → glob
	if e.staged[0].Match.Kind != "glob" {
		t.Fatalf("kind = %q, want glob", e.staged[0].Match.Kind)
	}
	if e.staged[0].Match.Quality != "" {
		t.Errorf("quality = %q, want cleared on the exact→glob toggle", e.staged[0].Match.Quality)
	}
}

// TestShikiSearchNilDepsGuard (n1): submitting a search without a
// wired Shikimori service must not panic — it surfaces a status
// message like the other search sites do.
func TestShikiSearchNilDepsGuard(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{}, dir) // deps.Shiki == nil
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	search, ok := pushedScreen(t, resolvePick(t, detail, detail.list, "bind")).(*shikiSearch)
	if !ok {
		t.Fatal("bind must push the shiki search")
	}
	search.prompt.typeText("Тайтл")
	_, cmd := search.Update(enter())
	if cmd != nil {
		t.Fatal("no Shiki service: nothing must be scheduled")
	}
	if strings.TrimSpace(search.status) == "" {
		t.Error("the missing service must surface a status message")
	}
}

// TestShikiSearchErrorRendersStatus (m8): a failed lookup renders the
// verdict — with results still nil the View previously fell back to
// the bare prompt and the user got no feedback.
func TestShikiSearchErrorRendersStatus(t *testing.T) {
	dir := rangesBindFixture(t)
	shiki := &fakeShiki{enabled: true, searchErr: errors.New("shiki down")}
	deps := &Deps{Shiki: shiki, Offline: &fakeOffline{}}
	e := newBindEditorFor(t, deps, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	search, ok := pushedScreen(t, resolvePick(t, detail, detail.list, "bind")).(*shikiSearch)
	if !ok {
		t.Fatal("bind must push the shiki search")
	}
	search.prompt.typeText("Тайтл")
	_, cmd := search.Update(enter())
	search.Update(cmd()) //nolint:errcheck // settles the error
	v := search.View().Content
	if !strings.Contains(v, "shiki down") {
		t.Errorf("the search error must render:\n%s", v)
	}
}

// TestRenamePromptAppliesTitle: ✏️ renames the staged range (prefill
// carries the current title).
func TestRenamePromptAppliesTitle(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 0)
	resolvePick(t, detail, detail.list, "rename")
	if detail.prompt == nil {
		t.Fatal("rename must open a prompt")
	}
	prompt := detail.prompt
	if prompt.Value() != "S1" {
		t.Errorf("prefill = %q, want the current title", prompt.Value())
	}
	prompt.typeText("Сезон 1")
	detail.Update(enter()) //nolint:errcheck // applies
	if e.staged[0].Title != "Сезон 1" {
		t.Errorf("title = %q, want Сезон 1", e.staged[0].Title)
	}
}

// TestDeleteRangeConfirmRemovesStaged: the 🗑 confirm removes the
// staged range and pops back to the editor.
func TestDeleteRangeConfirmRemovesStaged(t *testing.T) {
	dir := rangesBindFixture(t)
	e := newBindEditorFor(t, &Deps{Offline: &fakeOffline{}}, dir)
	e.staged = []download.RangeBinding{
		{ID: "r1", Title: "S1", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"EP_*"}}},
		{ID: "r2", Title: "S2", Match: download.RangeMatch{Kind: "glob", Patterns: []string{"OVA_*"}}},
	}
	e.buildList()
	detail := openRangeDetail(t, e, 1)
	resolvePick(t, detail, detail.list, "delete")
	if detail.confirm == nil {
		t.Fatal("delete must confirm first")
	}
	detail.confirm.Jump(0) // ✅
	_, cmd := detail.Update(enter())
	if cmd == nil {
		t.Fatal("confirmed delete must pop")
	}
	msg := cmd()
	if _, ok := msg.(popMsg); !ok {
		t.Fatalf("delete pop msg = %#v, want popMsg", msg)
	}
	if len(e.staged) != 1 || e.staged[0].ID != "r1" {
		t.Fatalf("staged = %+v, want r2 removed", e.staged)
	}
}
