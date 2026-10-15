package tui

import (
	"context"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// Editor screen ids (the offline binding editor stack: the editor,
// one range detail per open range, the file order picker and the
// Shikimori search).
const (
	offlineBindID       = "offline-bind"
	offlineBindRangeID  = "offline-bind-range"
	offlineBindFilesID  = "offline-bind-files"
	offlineBindSearchID = "offline-bind-search"
)

// bindSavedMsg settles one ranges-file write.
type bindSavedMsg struct{ err error }

// shikiSearchMsg settles one Shikimori candidate lookup.
type shikiSearchMsg struct {
	ids map[string]int64
	err error
}

// bindEditor is the ✂️ binding editor over one offline folder: the
// staged ranges render as rows, each opens the range detail; the save
// path guards duplicate Shikimori bindings (hard reject), overlapping
// ranges (real, scan-evaluated — explicit confirm) and exact lists
// naming missing files (the same confirm) before the atomic write.
type bindEditor struct {
	deps   *Deps
	dir    string
	relDir string
	// files is the folder's media scan (the exact-mode pool).
	files  []string
	staged []download.RangeBinding
	list   *PinList
	status string
	// loadErr is the ranges-file load failure (a foreign version, a
	// failed quarantine rename): the editor opens read-blocked — the
	// error is the status, nothing is staged over the unreadable file
	// and saving is disabled (review m4).
	loadErr error
	// dirty marks staged mutations: the rows rebuild at the next
	// render (the sub-screens mutate through the shared pointer).
	dirty bool
	// confirm is the save-confirmation substate menu (nil = off); it
	// covers the overlap and missing-file warnings alike.
	confirm *PinList
}

// NewBindEditor builds the editor for one offline title's folder: an
// existing ranges file stages verbatim (edit mode), otherwise the
// disposable SuggestRanges grouping seeds the editor. A load failure
// (m4) blocks the editor instead: suggestions must never overwrite a
// file LoadRanges promised untouched.
//
//nolint:revive // internal screen type (NewOfflineSession pattern)
func NewBindEditor(deps *Deps, title OfflineTitle) *bindEditor {
	e := &bindEditor{deps: deps, dir: title.Dir, relDir: title.RelDir}
	if e.dir != "" {
		e.files = download.ScanMediaFiles(e.dir)
	}
	rf, err := download.LoadRanges(e.dir)
	switch {
	case err != nil:
		e.loadErr = err
		e.status = i18n.T("common.error_with", i18n.Vals{"err": err.Error()})
	case rf != nil && len(rf.Ranges) > 0:
		e.staged = append([]download.RangeBinding(nil), rf.Ranges...)
	default:
		e.staged = download.SuggestRanges(e.dir)
	}
	e.buildList()
	return e
}

// ID implements Screen.
func (e *bindEditor) ID() string { return offlineBindID }

// Init implements Screen.
func (e *bindEditor) Init() tea.Cmd { return nil }

// buildList renders the staged rows plus the add and save rows; a
// load-blocked editor renders no add/save rows (m4 — nothing can be
// written over an unreadable file).
func (e *bindEditor) buildList() {
	choices := make([]Choice, 0, len(e.staged)+2)
	for i, r := range e.staged {
		kind := r.Match.Kind
		count := len(r.Match.Files)
		if kind == "glob" {
			count = len(download.ResolveRangeFiles(e.dir, r.Match))
		}
		choices = append(choices, Choice{
			ID: r.ID,
			Label: i18n.T("offline.ranges.range_row", i18n.Vals{
				"title": r.Title, "kind": kind, "count": strconv.Itoa(count)}),
			Value: i,
		})
	}
	if e.loadErr == nil {
		choices = append(choices,
			Choice{ID: "add", Label: i18n.T("offline.ranges.add")},
			Choice{ID: "save", Label: i18n.T("offline.ranges.save")},
		)
	}
	e.list = NewPinList(NewMenu(i18n.T("offline.ranges.editor_title"), "", choices...), defaultListHeight)
}

// View implements Screen: the armed confirm takes over the body (M1 —
// a hidden confirm intercepted every key while the stale list
// rendered); otherwise the rows render.
func (e *bindEditor) View() tea.View {
	if e.dirty {
		e.buildList()
		e.dirty = false
	}
	if e.confirm != nil {
		body := theme.Title.Render(e.status) + "\n\n" + e.confirm.Render()
		return tea.NewView(body)
	}
	body := theme.Title.Render(i18n.T("offline.ranges.editor_title")) + "\n\n" + e.list.Render()
	if e.status != "" {
		body += "\n" + theme.StatusLine.Render(e.status)
	}
	return tea.NewView(body)
}

// Update implements Screen: settles the save verdict and routes keys
// through the confirm substate or the list.
func (e *bindEditor) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case bindSavedMsg:
		if m.err != nil {
			e.status = i18n.T("common.error_with", i18n.Vals{"err": m.err.Error()})
			return e, nil
		}
		e.status = i18n.T("offline.ranges.saved")
		// Deterministic unwind + a FRESH titles list — the ranges file
		// changed on disk (Review Focus #3).
		return e, tea.Sequence(popToRoot(), push(NewOfflineTitles(e.deps)))
	case tea.KeyPressMsg:
		return e.handleKey(m)
	}
	return e, nil
}

// handleKey routes one key press: the confirm substate consumes keys
// first, then the list.
func (e *bindEditor) handleKey(key tea.KeyPressMsg) (Screen, tea.Cmd) {
	if e.confirm != nil {
		if e.confirm.HandleKey(key) {
			return e, nil
		}
		resolved := ResolveKey(e.confirm.Menu(), e.confirm.Cursor(), key)
		if resolved == nil {
			return e, nil
		}
		if resolved == Back {
			e.confirm = nil
			e.status = "" // n2: the ⚠ verdict must not linger
			return e, nil
		}
		if id, _ := resolved.(string); id == "confirm" {
			// The armed confirm stays set: save() sees it and proceeds
			// past the warnings (the owner just accepted them).
			return e, e.save()
		}
		e.confirm = nil
		e.status = "" // n2
		return e, nil
	}
	if IsCancelKey(key) {
		return e, pop()
	}
	if e.list.HandleKey(key) {
		return e, nil
	}
	resolved := ResolveKey(e.list.Menu(), e.list.Cursor(), key)
	if resolved == nil {
		return e, nil
	}
	if resolved == Back {
		return e, pop()
	}
	switch id, _ := resolved.(string); id {
	case "add":
		e.staged = append(e.staged, e.newStagedBinding())
		e.dirty = true
		return e, push(newRangeDetail(e, len(e.staged)-1))
	case "save":
		return e, e.save()
	default:
		if idx, ok := resolved.(int); ok && idx >= 0 && idx < len(e.staged) {
			return e, push(newRangeDetail(e, idx))
		}
		return e, nil
	}
}

// newStagedBinding appends an empty binding with a collision-free
// auto id ("r"+n in the editor's numbering).
func (e *bindEditor) newStagedBinding() download.RangeBinding {
	taken := map[string]struct{}{}
	for _, r := range e.staged {
		taken[r.ID] = struct{}{}
	}
	n := len(e.staged) + 1
	id := "r" + strconv.Itoa(n)
	for {
		if _, used := taken[id]; !used {
			break
		}
		n++
		id = "r" + strconv.Itoa(n)
	}
	return download.RangeBinding{
		ID:    id,
		Title: id,
		Match: download.RangeMatch{Kind: "glob"},
	}
}

// save runs the guarded write: a load-blocked editor refuses (m4);
// duplicate non-zero Shikimori bindings hard-reject (two ranges on one
// entry break the sync dispatch); overlaps — static and real, i.e.
// glob×glob evaluated against the editor's own scan (m2) — and exact
// lists naming missing files (m3) raise the explicit confirm; the
// confirmed (or clean) path schedules the atomic write.
func (e *bindEditor) save() tea.Cmd {
	if e.loadErr != nil {
		e.status = i18n.T("common.error_with", i18n.Vals{"err": e.loadErr.Error()})
		return nil
	}
	bound := map[int64]string{}
	for _, r := range e.staged {
		if r.ShikimoriID == 0 {
			continue
		}
		if first, dup := bound[r.ShikimoriID]; dup {
			e.status = i18n.T("offline.ranges.dup_shiki", i18n.Vals{
				"id": strconv.FormatInt(r.ShikimoriID, 10), "title": first})
			e.confirm = nil
			return nil
		}
		bound[r.ShikimoriID] = r.Title
	}
	// The warnings share one explicit confirm: while it is armed the
	// owner has already accepted whatever raised it.
	if e.confirm == nil {
		doc := &download.RangesFile{Version: 1, Ranges: e.staged}
		var warnings []string
		appendOverlap := func(overlaps map[string][]string) {
			if len(overlaps) == 0 {
				return
			}
			files := make([]string, 0, len(overlaps))
			for file := range overlaps {
				files = append(files, file)
			}
			sort.Strings(files)
			warnings = append(warnings, i18n.T("offline.ranges.overlap_confirm",
				i18n.Vals{"files": strings.Join(files, ", ")}))
		}
		// Static overlap: exact lists colliding with each other or
		// with a glob pattern.
		appendOverlap(download.RangeOverlaps(doc))
		// Real overlap (m2): the static check cannot see glob×glob —
		// the editor's own scan can.
		appendOverlap(download.RangeOverlapsAmong(e.files, doc))
		// Missing exact files (m3): an exact list naming absent files
		// warns instead of saving silently.
		if missing := e.missingExactFiles(); len(missing) > 0 {
			warnings = append(warnings, i18n.T("offline.ranges.missing_confirm",
				i18n.Vals{"files": strings.Join(missing, ", ")}))
		}
		if len(warnings) > 0 {
			e.status = strings.Join(warnings, "\n")
			e.confirm = NewPinList(NewMenu(i18n.T("offline.ranges.overlap_title"), "", []Choice{
				{ID: "confirm", Label: i18n.T("offline.ranges.confirm")},
				{ID: "cancel", Label: i18n.T("offline.ranges.cancel")},
			}...), defaultListHeight)
			return nil
		}
	}
	e.confirm = nil
	staged := append([]download.RangeBinding(nil), e.staged...)
	dir := e.dir
	return safeCmd(offlineBindID, func() tea.Msg {
		return bindSavedMsg{err: download.SaveRanges(dir, &download.RangesFile{Version: 1, Ranges: staged})}
	})
}

// missingExactFiles lists the exact-mode files absent from the
// editor's scan (byte-sorted, deduplicated).
func (e *bindEditor) missingExactFiles() []string {
	scan := make(map[string]struct{}, len(e.files))
	for _, f := range e.files {
		scan[f] = struct{}{}
	}
	seen := map[string]struct{}{}
	var missing []string
	for _, r := range e.staged {
		if r.Match.Kind != "exact" {
			continue
		}
		for _, f := range r.Match.Files {
			if _, ok := scan[f]; ok {
				continue
			}
			if _, dup := seen[f]; dup {
				continue
			}
			seen[f] = struct{}{}
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	return missing
}

// rangeDetail edits one staged range: title, Shikimori binding,
// episode offset, match mode and the glob/exact file selection.
type rangeDetail struct {
	editor *bindEditor
	idx    int
	list   *PinList
	status string
	// dirty rebuilds the rows at the next render (the sub-screens —
	// the Shikimori search, the file picker — mutate the staged
	// binding through the shared editor pointer).
	dirty bool
	// prompt is the active text substate (rename / offset / patterns);
	// while set it consumes every key.
	prompt     *TextPrompt
	promptKind string
	// confirm is the delete-confirmation substate menu (nil = off).
	confirm *PinList
}

// newRangeDetail builds the detail over the staged range at idx.
func newRangeDetail(editor *bindEditor, idx int) *rangeDetail {
	d := &rangeDetail{editor: editor, idx: idx}
	d.buildList()
	return d
}

// ID implements Screen.
func (d *rangeDetail) ID() string { return offlineBindRangeID }

// Init implements Screen.
func (d *rangeDetail) Init() tea.Cmd { return nil }

// binding is the staged range under edit.
func (d *rangeDetail) binding() *download.RangeBinding { return &d.editor.staged[d.idx] }

// buildList renders the detail rows for the current match mode.
func (d *rangeDetail) buildList() {
	r := d.binding()
	choices := []Choice{
		{ID: "rename", Label: i18n.T("offline.ranges.rename")},
	}
	if r.ShikimoriID != 0 {
		choices = append(choices, Choice{ID: "bind", Label: i18n.T("offline.ranges.bound_shiki", i18n.Vals{
			"title": r.ShikimoriTitle, "id": strconv.FormatInt(r.ShikimoriID, 10)})})
		choices = append(choices, Choice{ID: "unbind", Label: i18n.T("offline.ranges.unbind")})
	} else {
		choices = append(choices, Choice{ID: "bind", Label: i18n.T("offline.ranges.bind_shiki")})
	}
	choices = append(choices,
		Choice{ID: "offset", Label: i18n.T("offline.ranges.offset", i18n.Vals{"n": strconv.Itoa(r.EpisodeOffset)})},
		Choice{ID: "mode", Label: i18n.T("offline.ranges.mode", i18n.Vals{"mode": r.Match.Kind})},
	)
	switch r.Match.Kind {
	case "exact":
		choices = append(choices, Choice{ID: "files", Label: i18n.T("offline.ranges.files")})
	default:
		choices = append(choices, Choice{ID: "patterns", Label: i18n.T("offline.ranges.patterns")})
	}
	choices = append(choices, Choice{ID: "delete", Label: i18n.T("offline.ranges.delete")})
	d.list = NewPinList(NewMenu(r.Title, "", choices...), defaultListHeight)
}

// View implements Screen: the active substate (prompt or confirm)
// takes over the body; otherwise the rows render.
func (d *rangeDetail) View() tea.View {
	if d.dirty {
		d.buildList()
		d.dirty = false
	}
	var body string
	switch {
	case d.prompt != nil:
		return d.prompt.View()
	case d.confirm != nil:
		body = theme.Title.Render(d.status) + "\n\n" + d.confirm.Render()
	default:
		body = theme.Title.Render(d.binding().Title) + "\n\n" + d.list.Render()
	}
	if d.status != "" {
		body += "\n" + theme.StatusLine.Render(d.status)
	}
	return tea.NewView(body)
}

// Update implements Screen: the prompt consumes keys first, then the
// delete confirm, then the rows.
func (d *rangeDetail) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if d.prompt != nil {
		// The prompt's Update result must NOT become the return: App
		// replaces the stack top with it, which would retire this
		// detail (and orphan every later async message on a bare
		// prompt — CodeRabbit round-1). The owner screen stays.
		_, cmd := d.prompt.Update(msg)
		return d, cmd
	}
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return d, nil
	}
	if d.confirm != nil {
		if d.confirm.HandleKey(key) {
			return d, nil
		}
		resolved := ResolveKey(d.confirm.Menu(), d.confirm.Cursor(), key)
		if resolved == nil {
			return d, nil
		}
		if resolved == Back {
			d.confirm = nil
			return d, nil
		}
		if id, _ := resolved.(string); id == "confirm" {
			d.editor.staged = append(d.editor.staged[:d.idx], d.editor.staged[d.idx+1:]...)
			d.editor.dirty = true
			return d, pop()
		}
		d.confirm = nil
		return d, nil
	}
	if IsCancelKey(key) {
		return d, pop()
	}
	if d.list.HandleKey(key) {
		return d, nil
	}
	resolved := ResolveKey(d.list.Menu(), d.list.Cursor(), key)
	if resolved == nil {
		return d, nil
	}
	if resolved == Back {
		return d, pop()
	}
	switch id, _ := resolved.(string); id {
	case "rename":
		d.openPrompt("rename", i18n.T("offline.ranges.name_prompt"), d.binding().Title)
	case "bind":
		return d, push(newShikiSearch(d.editor, d, d.idx))
	case "unbind":
		d.binding().ShikimoriID = 0
		d.binding().ShikimoriTitle = ""
		d.editor.dirty = true
		d.dirty = true
	case "offset":
		d.openPrompt("offset", i18n.T("offline.ranges.offset_prompt"), "")
	case "mode":
		r := d.binding()
		if r.Match.Kind == "exact" {
			r.Match.Kind = "glob"
			r.Match.Patterns = nil
			r.Match.Files = nil
			// A leftover quality glob with no patterns is a
			// guaranteed hard-reject at save — clear it on the toggle
			// (review n3).
			r.Match.Quality = ""
		} else {
			// glob → exact: preseed the owner list with the current
			// resolution (selection order = episode order from here).
			r.Match.Files = download.ResolveRangeFiles(d.editor.dir, r.Match)
			r.Match.Patterns = nil
			r.Match.Kind = "exact"
		}
		d.editor.dirty = true
		d.dirty = true
	case "patterns":
		d.openPrompt("patterns", i18n.T("offline.ranges.patterns_prompt"), strings.Join(d.binding().Match.Patterns, ", "))
	case "files":
		return d, push(newFileOrderPicker(d.editor, d, d.idx))
	case "delete":
		d.status = i18n.T("offline.ranges.delete_confirm", i18n.Vals{"title": d.binding().Title})
		d.confirm = NewPinList(NewMenu(d.status, "", []Choice{
			{ID: "confirm", Label: i18n.T("offline.ranges.confirm")},
			{ID: "cancel", Label: i18n.T("offline.ranges.cancel")},
		}...), defaultListHeight)
	}
	return d, nil
}

// openPrompt engages a text substate; the submit closure writes back
// into the staged binding.
func (d *rangeDetail) openPrompt(kind, title, initial string) {
	d.promptKind = kind
	d.prompt = NewTextPrompt(TextPromptConfig{
		ID:       offlineBindRangeID + "-" + kind,
		Title:    title,
		Initial:  initial,
		OnSubmit: func(resolved any) tea.Cmd { return d.submitPrompt(resolved) },
	})
}

// submitPrompt applies the resolution of the active text substate.
func (d *rangeDetail) submitPrompt(resolved any) tea.Cmd {
	prompt := d.prompt
	kind := d.promptKind
	d.prompt = nil
	d.promptKind = ""
	if resolved == Back || prompt == nil {
		return nil
	}
	text, _ := resolved.(string)
	switch kind {
	case "rename":
		// ResolveText already maps an empty submit to Back; this
		// guard keeps the staged title non-empty should the text
		// path ever carry blanks.
		if strings.TrimSpace(text) == "" {
			d.status = i18n.T("offline.ranges.name_empty")
			return nil
		}
		d.binding().Title = text
	case "offset":
		n, err := strconv.Atoi(text)
		if err != nil {
			d.status = i18n.T("offline.ranges.invalid_number")
			return nil
		}
		// Negative offsets are meaningless for ranges: the sync
		// counter is position + offset and the guard compares entry
		// units — a negative offset would block every push (review M5).
		if n < 0 {
			d.status = i18n.T("offline.ranges.offset_negative")
			return nil
		}
		d.binding().EpisodeOffset = n
	case "patterns":
		var patterns []string
		for _, p := range strings.Split(text, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				patterns = append(patterns, trimmed)
			}
		}
		if len(patterns) == 0 {
			d.status = i18n.T("offline.ranges.patterns_empty")
			return nil
		}
		d.binding().Match.Kind = "glob"
		d.binding().Match.Patterns = patterns
	}
	d.editor.dirty = true
	d.dirty = true
	return nil
}

// fileOrderPicker picks the exact-mode files; the selection order IS
// the episode order.
type fileOrderPicker struct {
	editor *bindEditor
	idx    int
	list   *PinList
	status string
}

// newFileOrderPicker builds the picker over the editor's media scan.
func newFileOrderPicker(editor *bindEditor, _ *rangeDetail, idx int) *fileOrderPicker {
	p := &fileOrderPicker{editor: editor, idx: idx}
	p.buildList()
	return p
}

// ID implements Screen.
func (p *fileOrderPicker) ID() string { return offlineBindFilesID }

// Init implements Screen.
func (p *fileOrderPicker) Init() tea.Cmd { return nil }

// buildList renders the scan: included files carry their 1-based
// episode order, the rest render bare.
func (p *fileOrderPicker) buildList() {
	files := p.binding().Match.Files
	order := make(map[string]int, len(files))
	for i, f := range files {
		order[f] = i + 1
	}
	choices := make([]Choice, 0, len(p.editor.files))
	for _, file := range p.editor.files {
		label := file
		if n := order[file]; n > 0 {
			label = strconv.Itoa(n) + ". " + file
		}
		choices = append(choices, Choice{ID: "file:" + file, Label: label, Value: file})
	}
	p.list = NewPinList(NewMenu(i18n.T("offline.ranges.files_title"), "", choices...), defaultListHeight)
}

// binding is the staged range under edit.
func (p *fileOrderPicker) binding() *download.RangeBinding { return &p.editor.staged[p.idx] }

// View implements Screen.
func (p *fileOrderPicker) View() tea.View {
	body := theme.Title.Render(i18n.T("offline.ranges.files_title")) + "\n\n" + p.list.Render()
	if p.status != "" {
		body += "\n" + theme.StatusLine.Render(p.status)
	}
	return tea.NewView(body)
}

// Update implements Screen: Enter toggles the current file (include
// appends — selection order = episode order; exclude removes), Esc
// pops back to the detail.
func (p *fileOrderPicker) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	if IsCancelKey(key) {
		return p, pop()
	}
	if p.list.HandleKey(key) {
		return p, nil
	}
	resolved := ResolveKey(p.list.Menu(), p.list.Cursor(), key)
	if resolved == nil {
		return p, nil
	}
	if resolved == Back {
		return p, pop()
	}
	file, _ := resolved.(string)
	files := p.binding().Match.Files
	kept := files[:0:0]
	removed := false
	for _, f := range files {
		if f == file {
			removed = true
			continue
		}
		kept = append(kept, f)
	}
	if !removed {
		kept = append(kept, file)
	}
	p.binding().Match.Files = kept
	p.editor.dirty = true
	prev := cursorID(p.list)
	p.buildList()
	restoreCursor(p.list, prev)
	return p, nil
}

// shikiSearch binds one staged range to a Shikimori entry: the query
// prompt fans into SearchIDs, the results menu carries the sorted
// candidates plus the explicit skip row.
type shikiSearch struct {
	editor *bindEditor
	detail *rangeDetail
	idx    int
	prompt *TextPrompt
	// results is the settled candidate menu (nil until the lookup
	// resolves).
	results *PinList
	status  string
}

// newShikiSearch builds the search over one staged range.
func newShikiSearch(editor *bindEditor, detail *rangeDetail, idx int) *shikiSearch {
	s := &shikiSearch{editor: editor, detail: detail, idx: idx}
	s.prompt = NewTextPrompt(TextPromptConfig{
		ID:       offlineBindSearchID,
		Title:    i18n.T("offline.ranges.search_prompt"),
		OnSubmit: func(resolved any) tea.Cmd { return s.submit(resolved) },
	})
	return s
}

// ID implements Screen.
func (s *shikiSearch) ID() string { return offlineBindSearchID }

// Init implements Screen.
func (s *shikiSearch) Init() tea.Cmd { return nil }

// submit schedules the bounded candidate lookup.
func (s *shikiSearch) submit(resolved any) tea.Cmd {
	if resolved == Back {
		return pop()
	}
	query, _ := resolved.(string)
	deps := s.editor.deps
	if deps == nil || deps.Shiki == nil {
		// The same nil-guard the other search sites carry (n1): no
		// wired service must not panic the submit.
		s.status = i18n.T("offline.ranges.search_unavailable")
		return nil
	}
	return safeCmd(offlineBindSearchID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		ids, err := deps.Shiki.SearchIDs(ctx, query)
		return shikiSearchMsg{ids: ids, err: err}
	})
}

// apply binds the picked candidate and unwinds back to the detail.
func (s *shikiSearch) apply(id int64, title string) tea.Cmd {
	r := &s.editor.staged[s.idx]
	r.ShikimoriID = id
	r.ShikimoriTitle = title
	s.editor.dirty = true
	if s.detail != nil {
		s.detail.dirty = true
	}
	return pop()
}

// View implements Screen: a lookup failure renders its verdict over
// the prompt (m8 — results stay nil and the bare prompt previously
// gave the user no feedback); the results menu takes over when set.
func (s *shikiSearch) View() tea.View {
	if s.results != nil {
		body := theme.Title.Render(i18n.T("offline.ranges.search_results")) + "\n\n" + s.results.Render()
		if s.status != "" {
			body += "\n" + theme.StatusLine.Render(s.status)
		}
		return tea.NewView(body)
	}
	body := s.prompt.View().Content
	if s.status != "" {
		body += "\n" + theme.StatusLine.Render(s.status)
	}
	return tea.NewView(body)
}

// Update implements Screen: the prompt drives until the lookup
// settles, then the results menu takes over.
func (s *shikiSearch) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if m, ok := msg.(shikiSearchMsg); ok {
		if m.err != nil {
			s.status = i18n.T("common.error_with", i18n.Vals{"err": m.err.Error()})
			return s, nil
		}
		candidates := make([]string, 0, len(m.ids))
		for title := range m.ids {
			candidates = append(candidates, title)
		}
		sort.Strings(candidates)
		choices := make([]Choice, 0, len(candidates)+1)
		for _, title := range candidates {
			choices = append(choices, Choice{ID: "cand", Label: title, Value: m.ids[title]})
		}
		choices = append(choices, Choice{ID: "skip", Label: i18n.T("offline.ranges.search_skip")})
		s.results = NewPinList(NewMenu(i18n.T("offline.ranges.search_results"), "", choices...), defaultListHeight)
		return s, nil
	}
	if s.results != nil {
		key, ok := msg.(tea.KeyPressMsg)
		if !ok {
			return s, nil
		}
		if IsCancelKey(key) {
			return s, pop()
		}
		if s.results.HandleKey(key) {
			return s, nil
		}
		resolved := ResolveKey(s.results.Menu(), s.results.Cursor(), key)
		if resolved == nil {
			return s, nil
		}
		if resolved == Back {
			return s, pop()
		}
		switch v := resolved.(type) {
		case int64:
			// The picked candidate's title: the row label round-trips
			// through the current menu.
			title := s.results.Menu().Items[s.results.Cursor()].Label
			return s, s.apply(v, title)
		default:
			if id, _ := resolved.(string); id == "skip" {
				return s, pop()
			}
			return s, nil
		}
	}
	// The prompt's Update result stays internal (App replaces the
	// stack top with the returned Screen — returning the bare prompt
	// would strand the coming shikiSearchMsg on a screen that ignores
	// it). The owner screen stays.
	_, cmd := s.prompt.Update(msg)
	return s, cmd
}
