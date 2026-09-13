package tui

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// countingScreen is a minimal Screen for stack tests.
type countingScreen struct {
	id      string
	updates int
	panics  bool
	viewed  int
}

func (c *countingScreen) ID() string    { return c.id }
func (c *countingScreen) Init() tea.Cmd { return nil }
func (c *countingScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if c.panics {
		panic("boom: provider exploded mid-search")
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && key.Code == tea.KeyEsc {
		return c, pop()
	}
	c.updates++
	return c, nil
}
func (c *countingScreen) View() tea.View {
	c.viewed++
	return tea.NewView("screen " + c.id)
}

// newTestApp builds an App with the given root screen and no services.
func newTestApp(root Screen) App {
	return NewApp(root, nil, testLogger())
}

// drive runs one message through the model and then executes every
// command the updates returned (the way the bubbletea runtime would),
// so Cmd-driven navigation lands synchronously in tests.
func drive(model tea.Model, msg tea.Msg) tea.Model {
	m, cmd := model.Update(msg)
	for cmd != nil {
		m, cmd = m.Update(cmd())
	}
	return m
}

// TestAppPushPopNavigation: screen-stack push, pop and pop-to-root.
func TestAppPushPopNavigation(t *testing.T) {
	root := &countingScreen{id: "root"}
	app := newTestApp(root)

	t.Run("initial stack is the root alone", func(t *testing.T) {
		if len(app.stack) != 1 || app.stack[0].ID() != "root" {
			t.Fatalf("want [root], got %v", screenIDs(app.stack))
		}
	})

	t.Run("push adds a level", func(t *testing.T) {
		child := &countingScreen{id: "child"}
		model, _ := app.Update(pushMsg{screen: child})
		app2 := model.(App)
		if len(app2.stack) != 2 || app2.stack[1].ID() != "child" {
			t.Fatalf("want [root child], got %v", screenIDs(app2.stack))
		}
	})

	t.Run("pop removes one level", func(t *testing.T) {
		child := &countingScreen{id: "child"}
		model, _ := app.Update(pushMsg{screen: child})
		model, _ = model.Update(popMsg{})
		app2 := model.(App)
		if len(app2.stack) != 1 || app2.stack[0].ID() != "root" {
			t.Fatalf("want [root] after pop, got %v", screenIDs(app2.stack))
		}
	})

	t.Run("pop on root stack stays at root", func(t *testing.T) {
		model, _ := app.Update(popMsg{})
		app2 := model.(App)
		if len(app2.stack) != 1 {
			t.Fatalf("pop at root must not underflow, got %v", screenIDs(app2.stack))
		}
	})

	t.Run("popToRoot truncates deep stacks", func(t *testing.T) {
		model, _ := app.Update(pushMsg{screen: &countingScreen{id: "a"}})
		model, _ = model.Update(pushMsg{screen: &countingScreen{id: "b"}})
		model, _ = model.Update(pushMsg{screen: &countingScreen{id: "c"}})
		model, _ = model.Update(popToRootMsg{})
		app2 := model.(App)
		if len(app2.stack) != 1 || app2.stack[0].ID() != "root" {
			t.Fatalf("want [root] after popToRoot, got %v", screenIDs(app2.stack))
		}
	})

	t.Run("replace swaps only the top", func(t *testing.T) {
		model, _ := app.Update(pushMsg{screen: &countingScreen{id: "a"}})
		model, _ = model.Update(replaceMsg{screen: &countingScreen{id: "b"}})
		app2 := model.(App)
		if len(app2.stack) != 2 || app2.stack[1].ID() != "b" {
			t.Fatalf("want [root b] after replace, got %v", screenIDs(app2.stack))
		}
	})
}

// TestAppBackOnEsc: pressing Esc on a submenu screen pops one level
// (the Back semantics of I2 driven through a real screen).
func TestAppBackOnEsc(t *testing.T) {
	app := newTestApp(&countingScreen{id: "root"})
	model := drive(app, pushMsg{screen: &countingScreen{id: "search"}})
	model = drive(model, esc())
	app2 := model.(App)
	if len(app2.stack) != 1 || app2.stack[0].ID() != "root" {
		t.Fatalf("esc must pop one level, got %v", screenIDs(app2.stack))
	}
}

// TestAppPanicRecovery: a panicking screen never kills the process —
// the app logs, swaps in the error screen, and dismissing it returns
// one level up (§5 exception rule).
func TestAppPanicRecovery(t *testing.T) {
	t.Run("screen update panic swaps in error screen", func(t *testing.T) {
		app := newTestApp(&countingScreen{id: "root"})
		model, _ := app.Update(pushMsg{screen: &countingScreen{id: "broken", panics: true}})
		// Any message triggers the panic path.
		model, _ = model.Update(tea.KeyPressMsg{Code: 'x'})
		app2 := model.(App)
		top := app2.stack[len(app2.stack)-1]
		if top.ID() != errorScreenID {
			t.Fatalf("panic must surface the error screen, got %q", top.ID())
		}
		if !contains(top.View().Content, "Произошла ошибка") {
			t.Fatalf("error screen must show a RU error banner, got %q", top.View().Content)
		}
	})

	t.Run("dismissing the error screen pops one level", func(t *testing.T) {
		app := newTestApp(&countingScreen{id: "root"})
		model := drive(app, pushMsg{screen: &countingScreen{id: "broken", panics: true}})
		model = drive(model, tea.KeyPressMsg{Code: 'x'})
		model = drive(model, enter()) // dismiss
		app2 := model.(App)
		if len(app2.stack) != 2 || app2.stack[0].ID() != "root" || app2.stack[1].ID() != "broken" {
			t.Fatalf("dismiss must return one level up, got %v", screenIDs(app2.stack))
		}
	})
}

// TestAppAsyncPanicRecovery: panics inside tea.Cmd goroutines are
// converted into error messages, never process crashes.
func TestAppAsyncPanicRecovery(t *testing.T) {
	t.Run("safeCmd converts panic into errMsg", func(t *testing.T) {
		cmd := safeCmd("search-fanout", func() tea.Msg {
			panic("provider goroutine exploded")
		})
		msg := cmd()
		errMsg, ok := msg.(errMsg)
		if !ok {
			t.Fatalf("safeCmd must yield errMsg, got %#v", msg)
		}
		if errMsg.screen == "" || errMsg.err == nil {
			t.Fatalf("errMsg must carry origin and error, got %+v", errMsg)
		}
		if !errors.Is(errMsg.err, errPanic) {
			t.Fatalf("errMsg error must wrap the panic sentinel, got %v", errMsg.err)
		}
	})

	t.Run("app routes errMsg to the error screen", func(t *testing.T) {
		app := newTestApp(&countingScreen{id: "root"})
		model, _ := app.Update(errMsg{screen: "search", err: errors.New("network down")})
		app2 := model.(App)
		top := app2.stack[len(app2.stack)-1]
		if top.ID() != errorScreenID {
			t.Fatalf("errMsg must surface the error screen, got %q", top.ID())
		}
		if !contains(top.View().Content, "network down") {
			t.Fatalf("error screen must show the cause, got %q", top.View().Content)
		}
	})
}

// TestAppQuitRouting: quitMsg produces a tea.QuitMsg.
func TestAppQuitRouting(t *testing.T) {
	app := newTestApp(&countingScreen{id: "root"})
	_, cmd := app.Update(quitMsg{})
	if cmd == nil {
		t.Fatalf("quitMsg must schedule tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf("quitMsg command must yield tea.QuitMsg")
	}
}

// TestAppForwardsToTop: ordinary messages reach only the top screen;
// window-size events update the app frame itself.
func TestAppForwardsToTop(t *testing.T) {
	root := &countingScreen{id: "root"}
	child := &countingScreen{id: "child"}
	app := newTestApp(root)
	model, _ := app.Update(pushMsg{screen: child})
	model, _ = model.Update(tea.KeyPressMsg{Code: 'x'})
	app2 := model.(App)
	r := app2.stack[0].(*countingScreen)
	c := app2.stack[1].(*countingScreen)
	if r.updates != 0 {
		t.Fatalf("root must not see forwarded messages, saw %d", r.updates)
	}
	if c.updates == 0 {
		t.Fatalf("top screen must receive the message")
	}

	sized, _ := app2.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	app3 := sized.(App)
	if app3.width != 80 || app3.height != 24 {
		t.Fatalf("app must remember window size, got %dx%d", app3.width, app3.height)
	}
}

func screenIDs(stack []Screen) []string {
	out := make([]string, 0, len(stack))
	for _, s := range stack {
		out = append(out, s.ID())
	}
	return out
}
