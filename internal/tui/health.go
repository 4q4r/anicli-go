package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// Health screen id.
const healthID = "health"

// healthCheckTimeout bounds one provider's test search (python
// connect_timeout budget).
const healthCheckTimeout = 10 * 1000 * 1000 * 1000 //nolint:mnd // 10s in ns

// healthSettledMsg settles one provider's check.
type healthSettledMsg struct {
	providerID string
	err        error
}

// healthScreen runs every provider's test search in parallel with a
// live OK/Error table (python health_check + check_provider_safe
// port): one lightweight "test" query per provider with a timeout.
type healthScreen struct {
	deps    *Deps
	spin    spinner.Model
	rows    []ProviderMeta
	status  map[string]string
	pending map[string]bool
}

// NewHealthScreen builds the screen and schedules the checks.
//
//nolint:revive // internal screen type
func NewHealthScreen(deps *Deps) *healthScreen {
	sp := spinner.New(spinner.WithSpinner(spinner.Line))
	var rows []ProviderMeta
	if deps != nil && deps.Search != nil {
		rows = deps.Search.Providers()
	}
	h := &healthScreen{
		deps:    deps,
		spin:    sp,
		rows:    rows,
		status:  make(map[string]string, len(rows)),
		pending: make(map[string]bool, len(rows)),
	}
	for _, r := range rows {
		h.status[r.ID] = "Проверка…"
		h.pending[r.ID] = true
	}
	return h
}

// ID implements Screen.
func (h *healthScreen) ID() string { return healthID }

// Init implements Screen: one panic-safe check command per provider.
func (h *healthScreen) Init() tea.Cmd {
	cmds := []tea.Cmd{h.spin.Tick}
	for _, row := range h.rows {
		cmds = append(cmds, safeCmd(healthID, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
			defer cancel()
			err := h.deps.Health.Check(ctx, row.ID)
			return healthSettledMsg{providerID: row.ID, err: err}
		}))
	}
	return tea.Batch(cmds...)
}

// Update implements Screen.
func (h *healthScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		h.spin, cmd = h.spin.Update(msg)
		return h, cmd
	case healthSettledMsg:
		delete(h.pending, msg.providerID)
		if msg.err != nil {
			h.status[msg.providerID] = "Error"
		} else {
			h.status[msg.providerID] = "OK"
		}
		return h, nil
	case tea.KeyPressMsg:
		if IsCancelKey(msg) || msg.Code == tea.KeyEnter {
			return h, pop()
		}
		return h, nil
	default:
		return h, nil
	}
}

// View implements Screen: the live table.
func (h *healthScreen) View() tea.View {
	var b strings.Builder
	b.WriteString(theme.Title.Render("🛠 Проверка провайдеров"))
	b.WriteString("\n\n")
	for _, row := range h.rows {
		state := h.status[row.ID]
		style := theme.Dim
		switch {
		case h.pending[row.ID]:
			state = h.spin.View() + " " + state
			style = theme.Accent
		case state == "OK":
			style = theme.Success
		case state != "Проверка…":
			style = theme.Error
		}
		fmt.Fprintf(&b, "  %-16s %-6s %s\n", row.Name, "Ping", style.Render(state))
	}
	if len(h.rows) == 0 {
		b.WriteString(theme.Dim.Render("Нет зарегистрированных провайдеров"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(theme.StatusLine.Render("enter/esc — назад"))
	return tea.NewView(b.String())
}
