package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
)

func cfEnabledSettings(t *testing.T) (config.Settings, string) {
	t.Helper()
	s := config.Default()
	s.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)
	s.Providers.Yanima.DDoSP1 = "test-p1"  // keep yanima in the roster (PR33)
	s.Providers.Yanima.DDoSP2 = "test-p2"
	s.CF.Enabled = true
	s.CF.SolveTimeout = 5 * time.Second
	s.CF.UpdateInterval = time.Hour
	cache := t.TempDir()
	t.Setenv("CLOAKBROWSER_CACHE_DIR", cache)
	t.Setenv("CLOAKBROWSER_BINARY_PATH", "")
	// Fake installed binary in the cache.
	dir := filepath.Join(cache, "chromium-146.0.7680.177.5")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chrome"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Store lives under the data dir: pin it.
	t.Setenv("ANICLI_DATA", t.TempDir())
	return s, cache
}

func TestRegistryEnabledRequiresBinary(t *testing.T) {
	s := config.Default()
	s.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)
	s.Providers.Yanima.DDoSP1 = "test-p1"  // keep yanima in the roster (PR33)
	s.Providers.Yanima.DDoSP2 = "test-p2"
	s.CF.Enabled = true
	t.Setenv("CLOAKBROWSER_CACHE_DIR", t.TempDir())
	t.Setenv("CLOAKBROWSER_BINARY_PATH", "")
	t.Setenv("ANICLI_DATA", t.TempDir())

	_, err := NewRegistry(s, nil)
	if err == nil {
		t.Fatal("enabled CF without a binary must fail loud")
	}
	if !strings.Contains(err.Error(), "anicli cf install") {
		t.Errorf("error must carry the install hint (RU), got: %v", err)
	}
}

func TestRegistryEnabledWiresSolverAndCloses(t *testing.T) {
	s, _ := cfEnabledSettings(t)
	reg, err := NewRegistry(s, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// Close must be safe to call twice.
	if err := reg.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if len(reg.List()) != 17 {
		t.Fatalf("all providers must be registered, got %d", len(reg.List()))
	}
}

func TestRegistryDisabledKeepsPlainClients(t *testing.T) {
	s := config.Default()                  // cf disabled
	s.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)
	s.Providers.Yanima.DDoSP1 = "test-p1"  // keep yanima in the roster (PR33)
	s.Providers.Yanima.DDoSP2 = "test-p2"
	t.Setenv("ANICLI_DATA", t.TempDir())
	reg, err := NewRegistry(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 17 {
		t.Fatalf("providers = %d", len(reg.List()))
	}
}

func TestCFSolverAdapterConvertsClearance(t *testing.T) {
	s, _ := cfEnabledSettings(t)
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
