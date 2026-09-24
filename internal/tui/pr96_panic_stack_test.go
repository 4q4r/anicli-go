package tui

// PR96 panic diagnosability: the recovery wrappers capture debug.Stack
// alongside the panic message into the file logger (the full stack),
// while the user-facing error screen shows only the brief one-liner
// («panic: <msg> — детали в логе»). Evidence surface for the owner's
// stackless «источник: session» report: the next panic must be fully
// attributable from ~/tmp/anicli-tui.log alone.

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newBufApp builds an App whose diagnostics land in a probe buffer —
// the same seam production wires to the TUI log file (newTUILogger).
func newBufApp(t *testing.T, root Screen) (App, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return NewApp(root, nil, slog.New(slog.NewTextHandler(buf, nil))), buf
}

// TestPR96ScreenPanicStackCaptured: a panicking screen handler driven
// through the TUI update loop logs the full stack (the panicking
// frame's function name must appear in the sink) and the error screen
// shows the brief one-liner instead of the raw wrapped error.
func TestPR96ScreenPanicStackCaptured(t *testing.T) {
	app, buf := newBufApp(t, &countingScreen{id: "root"})
	model, _ := app.Update(pushMsg{screen: &countingScreen{id: "broken", panics: true}})
	model, _ = model.Update(tea.KeyPressMsg{Code: 'x'})
	app2 := model.(App)

	top := app2.stack[len(app2.stack)-1]
	if top.ID() != errorScreenID {
		t.Fatalf("panic must surface the error screen, got %q", top.ID())
	}
	view := top.View().Content
	if !strings.Contains(view, "panic: boom") || !strings.Contains(view, "детали в логе") {
		t.Fatalf("error screen must show the brief one-liner, got:\n%s", view)
	}
	if strings.Contains(view, "tui: recovered panic") {
		t.Fatalf("error screen must not show the raw wrapped error:\n%s", view)
	}

	logged := buf.String()
	if !strings.Contains(logged, "tui: screen panic recovered") {
		t.Fatalf("file sink must carry the recovery record:\n%s", logged)
	}
	if !strings.Contains(logged, "countingScreen") {
		t.Fatalf("file sink must carry the panicking frame from debug.Stack:\n%s", logged)
	}
}

// pr96PanicSite names the panicking frame so the stack assertion can
// grep it from the captured stack.
func pr96PanicSite() tea.Msg { panic("async: fan-out exploded") }

// TestPR96CommandPanicStackCaptured: a panic inside a safeCmd
// goroutine carries the brief form AND the captured stack through
// errMsg; processing it logs the stack to the file sink and surfaces
// the brief error screen.
func TestPR96CommandPanicStackCaptured(t *testing.T) {
	cmd := safeCmd("search-fanout", pr96PanicSite)
	msg := cmd()
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("safeCmd must yield errMsg, got %#v", msg)
	}
	if !strings.Contains(em.brief, "panic: async: fan-out exploded") {
		t.Fatalf("errMsg must carry the brief form, got %q", em.brief)
	}
	if !strings.Contains(em.stack, "pr96PanicSite") {
		t.Fatalf("errMsg must carry the captured stack, got:\n%s", em.stack)
	}

	app, buf := newBufApp(t, &countingScreen{id: "root"})
	model, _ := app.Update(em)
	app2 := model.(App)
	top := app2.stack[len(app2.stack)-1]
	if top.ID() != errorScreenID {
		t.Fatalf("panic errMsg must surface the error screen, got %q", top.ID())
	}
	if v := top.View().Content; !strings.Contains(v, "детали в логе") {
		t.Fatalf("error screen must show the brief one-liner:\n%s", v)
	}
	logged := buf.String()
	if !strings.Contains(logged, "tui: command panic recovered") {
		t.Fatalf("file sink must carry the command panic record:\n%s", logged)
	}
	if !strings.Contains(logged, "pr96PanicSite") {
		t.Fatalf("file sink must carry the panicking frame:\n%s", logged)
	}
}

// TestPR96RegularErrorStaysVerbose: plain async failures (no panic)
// keep the full cause on the error screen and the async-failure log
// record — the brief form is the panic path's signature only.
func TestPR96RegularErrorStaysVerbose(t *testing.T) {
	app, buf := newBufApp(t, &countingScreen{id: "root"})
	model, _ := app.Update(errMsg{screen: "search", err: errors.New("network down")})
	app2 := model.(App)
	top := app2.stack[len(app2.stack)-1]
	if top.ID() != errorScreenID {
		t.Fatalf("errMsg must surface the error screen, got %q", top.ID())
	}
	if v := top.View().Content; !strings.Contains(v, "network down") {
		t.Fatalf("regular failures keep the full cause:\n%s", v)
	}
	logged := buf.String()
	if !strings.Contains(logged, "tui: async failure") {
		t.Fatalf("regular failures keep the async-failure record:\n%s", logged)
	}
	if strings.Contains(logged, "tui: command panic recovered") {
		t.Fatalf("regular failures must not be logged as panics:\n%s", logged)
	}
}
