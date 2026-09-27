package tui

import (
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// TestLocaleKnobRootScreen is the PR110 integration proof: the same
// root screen renders ENGLISH strings after Init("en") and RUSSIAN
// strings after Init("ru") — the [general] locale knob flows all the
// way into rendered TUI output. Runs sequentially (no t.Parallel):
// it mutates the process-wide bundle and restores en on cleanup.
func TestLocaleKnobRootScreen(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })

	if err := i18n.Init("en"); err != nil {
		t.Fatalf("Init(en): %v", err)
	}
	en := NewRootScreen(&Deps{}).View().Content
	for _, want := range []string{"AniCLI — anime in the terminal", "📜 Lists", "🚪 Exit"} {
		if !strings.Contains(en, want) {
			t.Errorf("locale=en root view missing %q\nview:\n%s", want, en)
		}
	}
	if strings.Contains(en, "Списки") {
		t.Errorf("locale=en root view still contains Russian text:\n%s", en)
	}

	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	ru := NewRootScreen(&Deps{}).View().Content
	for _, want := range []string{"AniCLI — аниме в терминале", "📜 Списки", "🚪 Выход"} {
		if !strings.Contains(ru, want) {
			t.Errorf("locale=ru root view missing %q\nview:\n%s", want, ru)
		}
	}
	if strings.Contains(ru, "anime in the terminal") {
		t.Errorf("locale=ru root view still contains English text:\n%s", ru)
	}
}
