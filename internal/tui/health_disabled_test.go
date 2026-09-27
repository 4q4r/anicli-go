package tui

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/providers"
)

// TestHealthScreenShowsDisabledProviders (PR24): providers disabled at
// startup for missing configuration render as ОТКЛЮЧЁН rows with
// their reason and are never scheduled for a check.
func TestHealthScreenShowsDisabledProviders(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1] // one enabled provider
	fs.disabled = []providers.DisabledProvider{
		{ID: "kodik", Reason: "не задан токен (providers.kodik.token)"},
	}

	h := NewHealthScreen(&Deps{Search: fs})
	view := h.View().Content

	if !strings.Contains(view, "kodik") {
		t.Fatalf("disabled provider must be listed, got:\n%s", view)
	}
	if !strings.Contains(view, "DISABLED") {
		t.Fatalf("disabled provider must be marked DISABLED, got:\n%s", view)
	}
	if !strings.Contains(view, "не задан токен") {
		t.Fatalf("the disabled row must carry the reason, got:\n%s", view)
	}
}

// TestSearchFanOutExcludesDisabled: the fan-out table lists only the
// enabled providers — disabled ones never get a search command.
func TestSearchFanOutExcludesDisabled(t *testing.T) {
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.disabled = []providers.DisabledProvider{
		{ID: "kodik", Reason: "не задан токен (providers.kodik.token)"},
	}

	app := searchFlowApp(fs)
	model := drive(app, pushMsg{screen: NewSearchProgress(app.deps, "наруто")})
	model = drainCmds(model)

	progress := topOf(model)
	v := progress.View().Content
	if strings.Contains(v, "kodik") {
		t.Fatalf("disabled provider must not appear in the fan-out table, got:\n%s", v)
	}
}
