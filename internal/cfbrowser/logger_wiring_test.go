package cfbrowser

// PR85 wiring tests: the TUI wires the file logger through NewManager;
// every cfbrowser cycle (updater, verdict re-probe, solver) logs to
// the wired sink and NEVER to slog.Default (stderr corrupts the
// alt-screen).

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
)

// TestNewManagerWiresLoggerIntoSolverAndUpdater: the WithManagerLogger
// option reaches the deep seams (solver diagnostics, updater outcome
// lines); without the option everything degrades to discard.
func TestNewManagerWiresLoggerIntoSolverAndUpdater(t *testing.T) {
	cfg := config.Default()
	// Hermetic: no real cache, no background updater cycle — the test
	// pins the wiring, not the update policy.
	cfg.CF.AutoUpdate = false
	cfg.General.DataDir = t.TempDir()
	t.Setenv(EnvCacheDir, t.TempDir())
	probe := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	mgr, err := NewManager(cfg, WithManagerLogger(probe))
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	if mgr.logger != probe {
		t.Fatal("the wired logger did not reach the manager")
	}
	if mgr.Solver == nil || mgr.Solver.cfg.Logger != probe {
		t.Fatal("the wired logger did not reach the solver config")
	}
	if mgr.Updater == nil || mgr.Updater.cfg.Logger != probe {
		t.Fatal("the wired logger did not reach the updater config")
	}

	// Unwired construction degrades to discard: not slog.Default.
	mgr2, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("manager 2: %v", err)
	}
	t.Cleanup(func() { _ = mgr2.Close() })
	if mgr2.logger == slog.Default() {
		t.Fatal("unwired manager must not fall back to slog.Default")
	}
	if mgr2.logger == nil {
		t.Fatal("unwired manager logger = nil, want the discard sink")
	}
}

// TestUpdaterOfflineCycleNeverOnStderr (PR85 owner evidence): the
// updater cycle logs to the WIRED sink only — the offline-deferred
// warn lands on the wired logger while slog.Default stays silent.
// The updater is built DIRECTLY with the dead fixture URLs (no
// post-construction mutation: u.loop reads u.cfg concurrently).
func TestUpdaterOfflineCycleNeverOnStderr(t *testing.T) {
	defaultBuf := captureDefault(t)
	probeBuf := &bytes.Buffer{}
	probe := slog.New(slog.NewTextHandler(probeBuf, nil))

	cache := t.TempDir()
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	cfg := config.Default()
	cfg.CF.SolveTimeout = 30 * time.Second
	cfg.CF.UpdateInterval = time.Hour
	cfg.CF.Proxy = ""
	cfg.CF.Channel = "free"
	cfg.General.DataDir = t.TempDir()

	u := NewUpdater(UpdaterConfig{
		Enabled:      true,
		Interval:     time.Hour,
		APIBase:      deadURL,
		ProbeURL:     deadURL,
		DownloadBase: deadURL,
		CacheDir:     cache,
		Channel:      "free",
		Logger:       probe,
	})

	if err := u.CheckAndMaybeInstall(context.Background()); err != nil {
		t.Fatalf("deferred update is not a caller error: %v", err)
	}

	if defaultBuf.Len() != 0 {
		t.Fatalf("slog.Default captured a cfbrowser line — stderr leak:\n%s",
			defaultBuf.String())
	}
	if !strings.Contains(probeBuf.String(), "auto-update deferred") {
		t.Fatalf("the wired logger must carry the deferred warn, got:\n%s",
			probeBuf.String())
	}
}
