package providers

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
)

func cfWiredSettings(t *testing.T) (config.Settings, string) {
	t.Helper()
	s := config.Default()
	s.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)
	s.CF.SolveTimeout = 5 * time.Second
	s.CF.UpdateInterval = time.Hour
	cache := t.TempDir()
	t.Setenv("CLOAKBROWSER_CACHE_DIR", cache)
	// Fake installed binary in the cache, served through the explicit
	// $CLOAKBROWSER_BINARY_PATH override: the override channel is
	// user-owned and never probe-gated (PR76), so the wiring tests
	// exercise composition, not binary health (the fake bytes cannot
	// launch a real browser).
	dir := filepath.Join(cache, "chromium-146.0.7680.177.5")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "chrome")
	if err := os.WriteFile(fake, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOAKBROWSER_BINARY_PATH", fake)
	// Store lives under the data dir: pin it.
	t.Setenv("ANICLI_DATA", t.TempDir())
	return s, cache
}

// TestRegistryMissingBinaryStillBoots (PR80 always-on): a missing
// stealth-Chromium binary no longer fails the registry construction —
// the app boots and CF consumers surface typed errors at use, while
// the startup auto-download and the background updater self-heal the
// install (retired with the manual command in PR86).
func TestRegistryMissingBinaryStillBoots(t *testing.T) {
	s := config.Default()
	s.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)
	t.Setenv("CLOAKBROWSER_CACHE_DIR", t.TempDir())
	t.Setenv("CLOAKBROWSER_BINARY_PATH", "")
	t.Setenv("ANICLI_DATA", t.TempDir())

	reg, err := NewRegistry(s, nil)
	if err != nil {
		t.Fatalf("registry with a missing binary must still boot (degradation, not abort): %v", err)
	}
	defer func() { _ = reg.Close() }()
	// The full roster still registers — CF degradation is per-use, not
	// a registry-level exclusion.
	if len(reg.List()) != 23 {
		t.Errorf("roster = %d, want 23", len(reg.List()))
	}
}

func TestRegistryEnabledWiresSolverAndCloses(t *testing.T) {
	s, _ := cfWiredSettings(t)
	reg, err := NewRegistry(s, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// Close must be safe to call twice.
	if err := reg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if len(reg.List()) != 23 {
		t.Fatalf("all providers must be registered, got %d", len(reg.List()))
	}
}

func TestRegistryDisabledKeepsPlainClients(t *testing.T) {
	s := config.Default()                  // cf disabled
	s.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)
	t.Setenv("ANICLI_DATA", t.TempDir())
	reg, err := NewRegistry(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 23 {
		t.Fatalf("providers = %d, want 23", len(reg.List()))
	}
}

func TestCFSolverAdapterConvertsClearance(t *testing.T) {
	s, _ := cfWiredSettings(t)
	mgr, err := cfbrowser.NewManager(s)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	if mgr.Store == nil || mgr.Solver == nil || mgr.Updater == nil {
		t.Fatal("manager must expose solver, store and updater")
	}

	adapter := cfSolverAdapter{solver: mgr.Solver, store: mgr.Store}
	if err := mgr.Store.Put("animego.one", cfbrowser.Clearance{
		Cookies: []cfbrowser.Cookie{
			{Name: "cf_clearance", Value: "v", Domain: "animego.one", Path: "/"},
		},
		UserAgent:      "StealthUA/1",
		AcceptLanguage: "ru-RU,ru;q=0.9",
		Obtained:       time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := adapter.SolveChallenge(context.Background(), "https://animego.one/anime")
	if err != nil {
		t.Fatalf("adapter solve: %v", err)
	}
	if got.UserAgent != "StealthUA/1" || len(got.Cookies) != 1 || got.Cookies[0].Name != "cf_clearance" {
		t.Errorf("converted clearance = %+v", got)
	}
	if got.AcceptLanguage != "ru-RU,ru;q=0.9" {
		t.Errorf("accept-language = %q", got.AcceptLanguage)
	}

	adapter.InvalidateHost("animego.one")
	if _, ok := mgr.Store.Get("animego.one"); ok {
		t.Error("InvalidateHost must drop the cached clearance")
	}
}

// TestRegistryWiresCFBrowserLogger (PR85): WithCFBrowserLogger must
// reach the cfbrowser manager built inside NewRegistry — the updater
// cycle and verdict re-probe then log to the file sink, never stderr.
func TestRegistryWiresCFBrowserLogger(t *testing.T) {
	probe := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := config.Default()
	s.Providers.Kodik.Token = "test-token"
	t.Setenv("CLOAKBROWSER_CACHE_DIR", t.TempDir())
	t.Setenv("ANICLI_DATA", t.TempDir())

	reg, err := NewRegistry(s, nil, WithCFBrowserLogger(probe))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if reg.cfMgr == nil {
		t.Fatal("the registry must keep the cfbrowser manager")
	}
	if reg.cfMgr.Logger() != probe {
		t.Fatal("the wired logger did not reach the cfbrowser manager")
	}
}
