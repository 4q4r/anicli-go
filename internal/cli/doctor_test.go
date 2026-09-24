package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// TestStartupNotices: the PR24/PR55 startup warning lines — one
// per unconfigured provider, exact RU wording.
func TestStartupNotices(t *testing.T) {
	cfg := config.Default()
	cfg.Providers.Kodik.Token = ""

	notices := startupNotices(cfg)
	// Shikimori defaults to enabled=true (core feature) with empty
	// credentials, so the default config yields the kodik and
	// Shikimori notices.
	if len(notices) != 2 {
		t.Fatalf("want two notices (kodik + shikimori), got %v", notices)
	}
	wantKodik := "⚠ Провайдер 'kodik' отключён: не задан токен (providers.kodik.token)"
	if notices[0] != wantKodik {
		t.Fatalf("notice[0] = %q, want %q", notices[0], wantKodik)
	}

	cfg.Providers.Kodik.Token = "set"
	cfg.Shikimori.Session = "configured"
	if got := startupNotices(cfg); len(got) != 0 {
		t.Fatalf("configured providers must not warn, got %v", got)
	}
}

// TestStartupNoticesShikimori: the PR26 first-run warning — enabled
// integration without any credentials warns; every configured or
// disabled state stays silent.
func TestStartupNoticesShikimori(t *testing.T) {
	hasShikiNotice := func(notices []string) bool {
		for _, n := range notices {
			if strings.Contains(n, "Shikimori не настроен") {
				return true
			}
		}
		return false
	}

	t.Run("enabled without credentials warns", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "set"
		cfg.Shikimori.Enabled = true
		notices := startupNotices(cfg)
		if !hasShikiNotice(notices) {
			t.Fatalf("want the Shikimori notice, got %v", notices)
		}
		want := "⚠ Shikimori не настроен: нет ни cookie, ни OAuth токена (выберите способ в TUI)"
		found := false
		for _, n := range notices {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("notice wording mismatch, want %q, got %v", want, notices)
		}
	})

	t.Run("session configured stays silent", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "set"
		cfg.Shikimori.Enabled = true
		cfg.Shikimori.Session = "kawai"
		if hasShikiNotice(startupNotices(cfg)) {
			t.Fatal("a configured session must not warn")
		}
	})

	t.Run("access token configured stays silent", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "set"
		cfg.Shikimori.Enabled = true
		cfg.Shikimori.AccessToken = "at"
		if hasShikiNotice(startupNotices(cfg)) {
			t.Fatal("a configured token must not warn")
		}
	})

	t.Run("disabled stays silent (explicitly off)", func(t *testing.T) {
		cfg := config.Default()
		cfg.Providers.Kodik.Token = "set"
		cfg.Shikimori.Enabled = false
		if hasShikiNotice(startupNotices(cfg)) {
			t.Fatal("shikimori.enabled = false must not warn")
		}
	})
}

// stubProbe replaces the live doctor probe (no network egress in
// tests) and records the providers it saw. The doctor probes
// concurrently, so the log is mutex-guarded.
type stubProbe struct {
	results int
	err     error

	mu   sync.Mutex
	seen []string
}

func (s *stubProbe) probe(_ context.Context, p contracts.Provider, _ time.Duration) (int, error) {
	s.mu.Lock()
	s.seen = append(s.seen, p.ID())
	s.mu.Unlock()
	return s.results, s.err
}

// TestDoctorSearchBasedCheck (PR24): doctor probes every registered
// provider with the two test queries through the probe seam, renders
// Provider|Статус|Результатов rows and marks unconfigured providers
// ОТКЛЮЧЁН without probing them.
func TestDoctorSearchBasedCheck(t *testing.T) {
	t.Setenv("ANICLI_DATA", t.TempDir())

	stub := &stubProbe{results: 7}
	orig := doctorProbe
	doctorProbe = stub.probe
	t.Cleanup(func() { doctorProbe = orig })

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), "", &buf); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Провайдер", "Статус", "Результатов",
		"anilibria", "OK", "7",
		"kodik", "ОТКЛЮЧЁН", "не задан токен",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q, got:\n%s", want, out)
		}
	}
	// kodik is disabled → never probed.
	for _, id := range stub.seen {
		if id == "kodik" {
			t.Errorf("disabled kodik must not be probed, saw %v", stub.seen)
		}
	}
	if len(stub.seen) == 0 {
		t.Fatalf("the enabled providers must be probed, saw none")
	}
}

// TestDoctorErrorAndZeroResults: an erroring provider renders the
// error text; zero results still counts as OK (the provider answered).
func TestDoctorErrorAndZeroResults(t *testing.T) {
	t.Setenv("ANICLI_DATA", t.TempDir())

	stub := &stubProbe{results: 0, err: errors.New("dial tcp: i/o timeout")}
	orig := doctorProbe
	doctorProbe = stub.probe
	t.Cleanup(func() { doctorProbe = orig })

	var buf bytes.Buffer
	if err := runDoctor(context.Background(), "", &buf); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ОШИБКА") {
		t.Errorf("failing provider must render ОШИБКА, got:\n%s", out)
	}
	if !strings.Contains(out, "i/o timeout") {
		t.Errorf("the error text must be visible, got:\n%s", out)
	}
}

// TestDoctorExcludedRendersDisabled: explicitly excluded providers
// render as ОТКЛЮЧЁН with the exclusion reason, never probed.
func TestDoctorExcludedRendersDisabled(t *testing.T) {
	t.Setenv("ANICLI_DATA", t.TempDir())

	stub := &stubProbe{results: 3}
	orig := doctorProbe
	doctorProbe = stub.probe
	t.Cleanup(func() { doctorProbe = orig })

	var buf bytes.Buffer
	if err := runDoctorWithConfig(t, &buf, func(cfg *config.Settings) {
		cfg.Providers.Exclude = []string{"animepahe"}
		cfg.Providers.Kodik.Token = "set"
	}); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "animepahe") || !strings.Contains(out, "ОТКЛЮЧЁН") {
		t.Errorf("excluded provider must render ОТКЛЮЧЁН, got:\n%s", out)
	}
	for _, id := range stub.seen {
		if id == "animepahe" {
			t.Errorf("excluded provider must not be probed")
		}
	}
	// Sanity: with the token set kodik is probed normally.
	found := false
	for _, id := range stub.seen {
		if id == "kodik" {
			found = true
		}
	}
	if !found {
		t.Errorf("configured kodik must be probed, saw %v", stub.seen)
	}
}

// runDoctorWithConfig runs the doctor over a settings file written
// from cfg mutations.
func runDoctorWithConfig(t *testing.T, out *bytes.Buffer, mutate func(cfg *config.Settings)) error {
	t.Helper()
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	mutate(&cfg)
	return runDoctor(context.Background(), writeSettings(t, cfg), out)
}

// TestDoctorShikimoriReadsSettingsFileLikeTUI pins the PR32 audit
// invariant: the doctor's Shikimori verdict must derive from the
// settings FILE loaded exactly the way the TUI's startup sync loads it
// (config.Load over the actual settings path) — a session cookie
// persisted by the TUI first-run setup flips the doctor off "публичный
// режим". Mode detection is config-only (nil transport), so this stays
// offline.
func TestDoctorShikimoriReadsSettingsFileLikeTUI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[shikimori]\nenabled = true\nsession = \"kawai-cookie\"\n"), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	// The doctor's own loader (runDoctor -> loadSettingsOrFail)…
	settings, err := loadSettingsOrFail(path)
	if err != nil {
		t.Fatalf("loadSettingsOrFail: %v", err)
	}
	// …feeds the same mode detection probeShikimoriStatus applies
	// (shikimori.New with a nil transport answers config diagnostics).
	if got := shikimori.New(settings.Shikimori, nil, nil).Mode(); got != "cookie" {
		t.Fatalf("doctor shikimori mode = %q, want \"cookie\" — the file session must be seen", got)
	}

	// And the doctor-table row renders the cookie verdict, never the
	// public one.
	status, failed := shikiDoctorRow(shikiStatusReport{
		Mode: shikimori.New(settings.Shikimori, nil, nil).Mode(),
	})
	if status == "OK: публичный режим (без учётных данных)" {
		t.Fatalf("cookie mode must not render as публичный режим, got %q", status)
	}
	if failed {
		t.Fatalf("a detected cookie is not a doctor failure: %q", status)
	}
}

// writeSettings persists cfg to a temp TOML file via the config
// package's example structure (only the fields tests mutate).
func writeSettings(t *testing.T, cfg config.Settings) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/settings.toml"
	var b strings.Builder
	b.WriteString("[network]\nproxy_url = \"\"\n")
	if cfg.Providers.Kodik.Token != "" {
		b.WriteString("\n[providers.kodik]\ntoken = \"" + cfg.Providers.Kodik.Token + "\"\n")
	}
	if len(cfg.Providers.Exclude) > 0 {
		b.WriteString("\n[providers]\nexclude = [")
		for i, id := range cfg.Providers.Exclude {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("\"" + id + "\"")
		}
		b.WriteString("]\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// compile-time: the disabled type stays part of the doctor surface.
var _ = providers.DisabledProvider{}
