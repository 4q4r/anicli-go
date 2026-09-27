package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/i18n"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// errPanic marks a recovered panic; errors surfacing from screens or
// commands wrap it so the app can route panics and failures through
// the same error screen.
var errPanic = errors.New("tui: recovered panic")

// Screen is one navigation level of the application. Screens are
// value-safe: Update returns the (possibly replaced) next state of
// the same level.
type Screen interface {
	// ID is the stable screen identity (used by tests and logs).
	ID() string
	// Init runs when the screen enters the stack.
	Init() tea.Cmd
	// Update handles one message; the returned Screen replaces this
	// level in place.
	Update(msg tea.Msg) (Screen, tea.Cmd)
	// View renders the screen.
	View() tea.View
}

// Navigation messages: screens emit them via the push/pop/replace
// command constructors and the App mutates the stack. Keeping the
// navigation in messages makes every transition testable without a
// terminal.
type (
	// pushMsg adds a screen on top of the stack.
	pushMsg struct{ screen Screen }
	// popMsg removes the top screen (Back semantics).
	popMsg struct{}
	// popToRootMsg unwinds the stack to the root screen.
	popToRootMsg struct{}
	// replaceMsg swaps the top screen without changing depth.
	replaceMsg struct{ screen Screen }
	// quitMsg terminates the program.
	quitMsg struct{}
	// errMsg reports an asynchronous failure from a command. For
	// recovered panics (PR96) it additionally carries the brief
	// one-liner shown on the error screen and the debug.Stack captured
	// at the recover — the file logger prints the stack, the user
	// never does.
	errMsg struct {
		screen string
		err    error
		brief  string
		stack  string
	}
)

// push schedules adding a screen on top of the stack.
func push(screen Screen) tea.Cmd {
	return func() tea.Msg { return pushMsg{screen: screen} }
}

// pop schedules removing the top screen (Back).
func pop() tea.Cmd {
	return func() tea.Msg { return popMsg{} }
}

// popToRoot schedules unwinding the stack to the root screen.
func popToRoot() tea.Cmd {
	return func() tea.Msg { return popToRootMsg{} }
}

// replace schedules swapping the top screen.
func replace(screen Screen) tea.Cmd {
	return func() tea.Msg { return replaceMsg{screen: screen} }
}

// quit schedules application exit.
func quit() tea.Cmd {
	return func() tea.Msg { return quitMsg{} }
}

// safeCmd wraps a tea.Cmd so a panic inside its goroutine is
// converted into an errMsg instead of crashing the process (§5
// exception rule). The origin tags the failing screen in logs.
// PR96: the recover captures debug.Stack (go-chi Recoverer pattern) —
// the file logger gets the full stack when the errMsg is processed,
// the user sees only the brief one-liner.
func safeCmd(origin string, cmd tea.Cmd) tea.Cmd {
	return func() (msg tea.Msg) {
		defer func() {
			if r := recover(); r != nil {
				msg = errMsg{
					screen: origin,
					err:    fmt.Errorf("%w: %v", errPanic, r),
					brief:  panicBrief(r),
					stack:  string(debug.Stack()),
				}
			}
		}()
		return cmd()
	}
}

// panicBrief renders the one-line user-facing panic form: the message
// travels to the screen, the full debug.Stack travels to the file log.
func panicBrief(r any) string {
	return fmt.Sprintf("panic: %v — детали в логе", r)
}

// errorScreenID is the identity of the recovery screen.
const errorScreenID = "__error__"

// errorScreen is the §5 recovery surface: a panic or async failure
// swaps it in; dismissing it (any key) returns one level up — the
// exception never propagates to process exit. Recovered panics (PR96)
// render only the brief one-liner; the full stack lives in the file
// log.
type errorScreen struct {
	origin string
	err    error
	brief  string
}

// ID implements Screen.
func (e *errorScreen) ID() string { return errorScreenID }

// Init implements Screen.
func (e *errorScreen) Init() tea.Cmd { return nil }

// Update implements Screen: any key dismisses, popping one level.
func (e *errorScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return e, pop()
	}
	return e, nil
}

// View implements Screen.
func (e *errorScreen) View() tea.View {
	// PR96: recovered panics show the brief one-liner (the full stack
	// went to the file log); regular failures keep the full cause.
	cause := e.err.Error()
	if e.brief != "" {
		cause = e.brief
	}
	body := theme.Title.Render("⚠ Произошла ошибка") + "\n\n" +
		theme.Error.Render(cause) + "\n" +
		theme.Dim.Render("источник: "+e.origin) + "\n\n" +
		theme.StatusLine.Render("Нажмите любую клавишу, чтобы вернуться")
	return tea.NewView(body)
}

// App is the root bubbletea model: it owns the screen stack, routes
// navigation messages and guarantees that neither panicking screens
// nor failing commands terminate the process.
type App struct {
	stack  []Screen
	deps   *Deps
	log    *slog.Logger
	width  int
	height int
	// ctx is the app-lifecycle context. DIVERGENCE (documented, kept
	// deliberately): screen commands (search fan-out, episode lookups,
	// plays, status patches) build their own context.Background()
	// children with explicit timeouts instead of deriving from ctx —
	// the Screen contract exposes no ctx to commands, and threading
	// one through every constructor is an architectural change outside
	// this package's scope. The per-command timeouts bound runaway
	// work instead; App.Cancel still stops the runtime between
	// updates.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewApp builds the application model with the root screen on the
// stack. Optional overlays push additional screens ON TOP of the root
// (PR26: the first-run Shikimori setup gate); Init runs the topmost
// screen's Init.
func NewApp(root Screen, deps *Deps, log *slog.Logger, overlays ...Screen) App {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	stack := append([]Screen{root}, overlays...)
	return App{stack: stack, deps: deps, log: log, ctx: ctx, cancel: cancel}
}

// Init implements tea.Model: runs the root screen's Init.
func (a App) Init() tea.Cmd {
	if len(a.stack) == 0 {
		return nil
	}
	return a.stack[len(a.stack)-1].Init()
}

// Update implements tea.Model with the §5 exception wrapper: every
// delegated message is guarded by a recover that logs the panic,
// surfaces the error screen and keeps the app alive.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd, recovered := a.updateGuarded(msg)
	if recovered != nil {
		// PR96: the file logger carries the full debug.Stack so the
		// next panic is fully diagnosable from the log alone.
		a.log.Error("tui: screen panic recovered",
			"screen", recovered.screen, "panic", recovered.err,
			"stack", recovered.stack)
	}
	return model, cmd
}

// updateGuarded runs one update cycle. The named return recovered is
// non-nil when a panic was caught (so Update can log it). PR96: the
// recover captures debug.Stack while the panic is still active (the
// go-chi Recoverer pattern) — after the deferred function returns the
// stack is gone.
func (a App) updateGuarded(msg tea.Msg) (model tea.Model, cmd tea.Cmd, recovered *errMsg) {
	defer func() {
		if r := recover(); r != nil {
			recovered = &errMsg{
				screen: a.topID(),
				err:    fmt.Errorf("%w: %v", errPanic, r),
				brief:  panicBrief(r),
				stack:  string(debug.Stack()),
			}
			model, cmd = a.surfaceError(*recovered)
		}
	}()

	switch m := msg.(type) {
	case pushMsg:
		next := append(cloneStack(a.stack), m.screen)
		return App{stack: next, deps: a.deps, log: a.log, width: a.width, height: a.height,
			ctx: a.ctx, cancel: a.cancel}, m.screen.Init(), nil
	case popMsg:
		next := cloneStack(a.stack)
		if len(next) > 1 {
			next = next[:len(next)-1]
		}
		return App{stack: next, deps: a.deps, log: a.log, width: a.width, height: a.height,
			ctx: a.ctx, cancel: a.cancel}, nil, nil
	case popToRootMsg:
		next := cloneStack(a.stack)[:1]
		return App{stack: next, deps: a.deps, log: a.log, width: a.width, height: a.height,
			ctx: a.ctx, cancel: a.cancel}, nil, nil
	case replaceMsg:
		next := cloneStack(a.stack)
		if len(next) > 0 {
			next[len(next)-1] = m.screen
		} else {
			next = []Screen{m.screen}
		}
		return App{stack: next, deps: a.deps, log: a.log, width: a.width, height: a.height,
			ctx: a.ctx, cancel: a.cancel}, m.screen.Init(), nil
	case quitMsg:
		return a, tea.Quit, nil
	case errMsg:
		if errors.Is(m.err, errPanic) {
			// PR96: a safeCmd goroutine panic — its captured
			// debug.Stack reaches the file log here.
			a.log.Error("tui: command panic recovered",
				"screen", m.screen, "panic", m.err, "stack", m.stack)
		} else {
			a.log.Error("tui: async failure", "screen", m.screen, "error", m.err)
		}
		model, cmd = a.surfaceError(m)
		return model, cmd, nil
	case tea.WindowSizeMsg:
		// PR98: the tracked size also reaches the top screen so it can
		// re-render against the new width (the search fan-out table).
		next, cmd := a.stack, tea.Cmd(nil)
		if len(a.stack) > 0 {
			top, c := a.stack[len(a.stack)-1].Update(msg)
			next = cloneStack(a.stack)
			next[len(next)-1] = top
			cmd = c
		}
		return App{stack: next, deps: a.deps, log: a.log, width: m.Width, height: m.Height,
			ctx: a.ctx, cancel: a.cancel}, cmd, nil
	case tea.QuitMsg:
		return a, nil, nil
	default:
		if len(a.stack) == 0 {
			return a, nil, nil
		}
		top := a.stack[len(a.stack)-1]
		next, cmd := top.Update(msg)
		replaced := cloneStack(a.stack)
		replaced[len(replaced)-1] = next
		return App{stack: replaced, deps: a.deps, log: a.log, width: a.width, height: a.height,
			ctx: a.ctx, cancel: a.cancel}, cmd, nil
	}
}

// surfaceError pushes the error screen (replacing nothing) with the
// Back-dismiss semantics.
func (a App) surfaceError(m errMsg) (tea.Model, tea.Cmd) {
	next := append(cloneStack(a.stack), &errorScreen{origin: m.screen, err: m.err, brief: m.brief})
	return App{stack: next, deps: a.deps, log: a.log, width: a.width, height: a.height,
		ctx: a.ctx, cancel: a.cancel}, nil
}

// View implements tea.Model: renders the top screen in alt-screen
// mode.
func (a App) View() tea.View {
	if len(a.stack) == 0 {
		return tea.NewView("")
	}
	v := a.stack[len(a.stack)-1].View()
	v.AltScreen = true
	return v
}

// topID returns the current screen id for panic attribution.
func (a App) topID() string {
	if len(a.stack) == 0 {
		return "<empty>"
	}
	return a.stack[len(a.stack)-1].ID()
}

// cloneStack copies the stack slice so models stay immutable.
func cloneStack(stack []Screen) []Screen {
	return append([]Screen(nil), stack...)
}

// Cancel terminates the app lifecycle context (SIGINT/quit path).
func (a App) Cancel() {
	if a.cancel != nil {
		a.cancel()
	}
}

// Ctx returns the app lifecycle context.
func (a App) Ctx() context.Context { return a.ctx }

// Deps carries the services the screens consume. It is deliberately a
// concrete struct of interfaces: production wires the real core, tests
// inject fakes.
type Deps struct {
	Search   SearchService
	Episode  EpisodeService
	Playback PlaybackService
	History  HistoryService
	Offline  OfflineService
	Database DatabaseService
	Health   HealthService
	Shiki    ShikimoriService
	Download DownloadService
	// Buffered powers «Формат: [буферный]» (PR43 C); nil makes the
	// buffered toggle unavailable with an honest status note.
	Buffered BufferedService
	// Metadata expands the hybrid search variants (PR24); nil skips
	// enrichment.
	Metadata MetadataService
	// SearchTimeout bounds ONE provider's whole fan-out participation
	// (all its query variants); 0 means the package default (30s).
	SearchTimeout time.Duration
	// Log is the diagnostics sink for quiet-skip notes (shikimori
	// binding etc.); nil degrades to slog.Default().
	Log *slog.Logger

	// StartupNotices carries the red warning lines (disabled providers,
	// unconfigured Shikimori) rendered on the root screen INSIDE the
	// TUI — pre-alt-screen terminal output is invisible after the TUI
	// takes over.
	StartupNotices []string

	// ShikiCfg snapshots the [shikimori] section at startup (PR26):
	// the first-run setup screens compose their updates on top of it
	// (preserving unrelated fields such as the OAuth client
	// credentials).
	ShikiCfg config.Shikimori
	// SettingsWriter persists an updated [shikimori] section to the
	// settings file (wired from the CLI's config.UpdateShikimori;
	// nil in embedded builds — the setup screens surface that as an
	// error instead of pretending success).
	SettingsWriter func(config.Shikimori) error
	// ShikiWhoAmI verifies candidate credentials with one whoami
	// round-trip, without touching the running client (nil surfaces
	// as an error in the setup screens).
	ShikiWhoAmI func(ctx context.Context, section config.Shikimori) (ShikiUser, error)
	// ShikiOAuth starts the OAuth2 authorization-code flow: it opens
	// the loopback callback server, returns the authorize URL and a
	// blocking resolve that waits for the code and exchanges it for
	// tokens (nil surfaces as an error in the setup screens).
	ShikiOAuth func(clientID, clientSecret string, port int) (authURL string, resolve func(ctx context.Context) (ShikiOAuthResult, error), err error)
	// SyncFull runs the PR27 startup two-way Shikimori list sync
	// (pull remote rates, create remote-only rows, replay dirty ones);
	// the progress callback receives live updates for the sync screen.
	// nil means no sync capability — the sync screen is skipped.
	SyncFull func(ctx context.Context, progress func(shikimori.SyncProgress)) (*shikimori.SyncResult, error)
	// I18n is the locale bundle built by the CLI at startup from
	// [general] locale (PR110). Run installs it as the process-wide
	// table before the first screen renders; nil keeps the i18n
	// package default (embedded en). The dependency direction is
	// tui → i18n: this package never imports tui from i18n.
	I18n *i18n.Bundle
}

// logger returns the diagnostics sink, defaulting to slog.Default().
func (d *Deps) logger() *slog.Logger {
	if d == nil || d.Log == nil {
		return slog.Default()
	}
	return d.Log
}
