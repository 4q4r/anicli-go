package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/providers"
)

// TestStartupNotices: the PR24 startup warning lines — one per
// unconfigured provider, exact RU wording.
func TestStartupNotices(t *testing.T) {
	cfg := config.Default()
	cfg.Providers.Kodik.Token = ""

	notices := startupNotices(cfg)
	if len(notices) != 1 {
		t.Fatalf("want one notice, got %v", notices)
	}
	want := "⚠ Провайдер 'kodik' отключён: не задан токен (providers.kodik.token)"
	if notices[0] != want {
		t.Fatalf("notice = %q, want %q", notices[0], want)
	}

	cfg.Providers.Kodik.Token = "set"
	if got := startupNotices(cfg); len(got) != 0 {
		t.Fatalf("configured providers must not warn, got %v", got)
	}
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
