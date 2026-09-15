package tui

// The Shikimori-first search flow: the free-text query goes to the
// Shikimori autocomplete first, the user picks a canonical title from
// their list, THEN the providers are fanned out with that title's name
// plus its alternative names from the metadata manager. This replaces
// the old "type → blind fan-out" flow per the user ruling: search works
// strictly through the Shikimori list.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// shikiPickID is the Shikimori picker screen identity.
const shikiPickID = "shiki_pick"

// shikiCandidatesMsg settles the autocomplete lookup.
type shikiCandidatesMsg struct {
	titles []shikiCandidate
	err    error
}

// shikiCandidate is one Shikimori autocomplete row.
type shikiCandidate struct {
	Title string
	ID    int64
}

// shikiPickScreen lists the Shikimori autocomplete results for the
// user's query; Enter resolves the pick and starts the provider
// fan-out with the canonical title and its alternatives.
type shikiPickScreen struct {
	deps     *Deps
	query    string
	list     *PinList
	loading  bool
	err      error
	titles   []shikiCandidate
	variants []string // resolved alt names for the picked title
}

// NewShikiPickScreen builds the Shikimori autocomplete picker over the
// raw query. The autocomplete runs in Init; until it settles the
// screen shows a spinner line.
func NewShikiPickScreen(deps *Deps, query string) *shikiPickScreen {
	return &shikiPickScreen{
		deps:    deps,
		query:   query,
		loading: true,
	}
}

// ID implements Screen.
func (s *shikiPickScreen) ID() string { return shikiPickID }

// shikiPickTimeout bounds the autocomplete + metadata lookup.
const shikiPickTimeout = 30e9 // 30s

// Init runs the Shikimori autocomplete.
func (s *shikiPickScreen) Init() tea.Cmd {
	deps := s.deps
	query := s.query
	if deps != nil && deps.Log != nil {
		deps.Log.Info("search: shiki pick", "query", query)
	}
	return safeCmd(shikiPickID, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), shikiPickTimeout)
		defer cancel()
		if deps == nil || deps.Shiki == nil || !deps.Shiki.Enabled() {
			return shikiCandidatesMsg{err: fmt.Errorf("Shikimori недоступен")}
		}
		ids, err := deps.Shiki.SearchIDs(ctx, query)
		if err != nil {
			return shikiCandidatesMsg{err: err}
		}
		var cands []shikiCandidate
		for title, id := range ids {
			cands = append(cands, shikiCandidate{Title: title, ID: id})
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].Title < cands[j].Title })
		return shikiCandidatesMsg{titles: cands}
	})
}

// Update implements Screen: the spinner spins until the autocomplete
// settles; then the list is interactive; Enter starts the fan-out.
func (s *shikiPickScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch m := msg.(type) {
	case shikiCandidatesMsg:
		s.loading = false
		s.err = m.err
		s.titles = m.titles
		if len(m.titles) == 0 {
			if m.err != nil {
				s.err = m.err
			} else {
				s.err = fmt.Errorf("ничего не найдено в Shikimori по запросу «%s»", s.query)
			}
			return s, nil
		}
		items := make([]Choice, 0, len(m.titles))
		for _, c := range m.titles {
			items = append(items, Choice{ID: fmt.Sprintf("shiki_%d", c.ID), Label: c.Title, Value: c})
		}
		menu := NewMenu("Выберите тайтл из Shikimori", "Ничего не найдено", items...)
		s.list = NewPinList(menu, defaultListHeight)
		return s, nil
	case tea.KeyPressMsg:
		if s.list == nil {
			if IsCancelKey(m) {
				return s, pop()
			}
			return s, nil
		}
		if IsCancelKey(m) {
			return s, pop()
		}
		if s.list.HandleKey(m) {
			return s, nil
		}
		resolved := ResolveKey(s.list.Menu(), s.list.Cursor(), m)
		switch pick := resolved.(type) {
		case nil:
			return s, nil
		case *backToken:
			return s, pop()
		default:
			cand, ok := pick.(shikiCandidate)
			if !ok {
				return s, nil
			}
			// Resolve variants (title + alternatives from metadata),
			// then start the provider fan-out.
			return s, startShikiFanOut(s.deps, cand)
		}
	case shikiFanOutMsg:
		// The variants resolved by startShikiFanOut settle HERE: this
		// screen is still on top when the message arrives, so it owns
		// the swap to the fan-out table (PR28: the message used to be
		// dropped and the flow dead-ended on the pick list).
		variants := m.variants
		if len(variants) == 0 {
			variants = []string{m.title}
		}
		return s, replace(NewShikiFanOut(s.deps, m.title, m.shikimoriID, variants))
	default:
		return s, nil
	}
}

// startShikiFanOut resolves the canonical title's alternative names and
// replaces this screen with the provider fan-out progress table.
func startShikiFanOut(deps *Deps, cand shikiCandidate) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), shikiPickTimeout)
		defer cancel()

		variants := []string{cand.Title}
		if deps != nil && deps.Metadata != nil {
			if more, err := deps.Metadata.SearchAlternativeTitles(ctx, cand.Title); err == nil {
				variants = append(variants, more...)
			}
		}
		// De-duplicate + cap.
		seen := map[string]bool{}
		uniq := variants[:0]
		for _, v := range variants {
			if !seen[v] {
				seen[v] = true
				uniq = append(uniq, v)
			}
		}
		if len(uniq) > maxQueryVariants {
			uniq = uniq[:maxQueryVariants]
		}
		return shikiFanOutMsg{title: cand.Title, shikimoriID: cand.ID, variants: uniq}
	}
}

// shikiFanOutMsg carries the resolved variants to the screen.
type shikiFanOutMsg struct {
	title       string
	shikimoriID int64
	variants    []string
}

// View renders the picker.
func (s *shikiPickScreen) View() tea.View {
	var b strings.Builder
	b.WriteString(theme.Title.Render("🔎 Поиск через Shikimori"))
	b.WriteString("\n\n")
	if s.loading {
		b.WriteString(theme.Accent.Render("⠋ Поиск в Shikimori…"))
		b.WriteString("\n\n")
		b.WriteString(theme.StatusLine.Render("загрузка автодополнения…"))
		return tea.NewView(b.String())
	}
	if s.err != nil {
		b.WriteString(theme.Error.Render("⚠ " + s.err.Error()))
		b.WriteString("\n\n")
		b.WriteString(theme.StatusLine.Render("esc — назад к поиску"))
		return tea.NewView(b.String())
	}
	if s.list != nil {
		b.WriteString(s.list.Render())
		b.WriteString("\n")
		b.WriteString(theme.StatusLine.Render("enter — выбрать · esc — назад"))
	}
	return tea.NewView(b.String())
}

// maxQueryVariants caps the query-variant set (Python MAX_QUERY_VARIANTS).
const maxQueryVariants = 8

// shikiFanOutScreen is the provider progress table for a picked
// Shikimori title. It wraps the existing searchProgress with the
// pre-resolved variants and the shikimori_id binding.
type shikiFanOutScreen struct {
	*searchProgress
	title       string
	shikimoriID int64
}

// NewShikiFanOut builds the fan-out progress over the picked title.
func NewShikiFanOut(deps *Deps, title string, shikimoriID int64, variants []string) *shikiFanOutScreen {
	sp := NewSearchProgress(deps, variants[0])
	sp.variants = variants
	return &shikiFanOutScreen{
		searchProgress: sp,
		title:          title,
		shikimoriID:    shikimoriID,
	}
}

// ID overrides to distinguish from the old search progress.
func (s *shikiFanOutScreen) ID() string { return "shiki_fanout" }

// Init overrides: no Shikimori enrichment needed (already resolved).
func (s *shikiFanOutScreen) Init() tea.Cmd {
	if s.deps != nil && s.deps.Log != nil {
		s.deps.Log.Info("search: shiki fan-out",
			"title", s.title, "shikimori_id", s.shikimoriID,
			"variants", len(s.variants), "providers", len(s.rows))
	}
	return s.startFanOut()
}

// Update delegates every message to the progress table. A stale
// shikiFanOutMsg (a duplicate variant resolution landing after the
// screen swap) must NOT restart the fan-out, so it falls through to
// the table's default no-op like any other unknown message. When the
// table keeps itself, this wrapper stays the screen so the stack top
// keeps the shiki_fanout identity.
func (s *shikiFanOutScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	next, cmd := s.searchProgress.Update(msg)
	if next == Screen(s.searchProgress) {
		return s, cmd
	}
	return next, cmd
}
