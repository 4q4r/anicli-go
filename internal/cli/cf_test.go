package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// executeCF runs the cf command tree with args against a buffer.
func executeCF(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	root := &cobra.Command{Use: "root"}
	root.AddCommand(newCFCommand())
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"cf"}, args...))
	err := root.Execute()
	return out.String(), err
}

// pinHermeticConfig points $ANICLI_CONFIG at a minimal temp settings
// file: these tests must not depend on the developer's real
// ~/.config/anicli/settings.toml (PR80's removed-key migration error
// fires on legacy real files and would fail the run).
func pinHermeticConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte("[cf]\nchannel = \"auto\"\n"), 0o600); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	t.Setenv("ANICLI_CONFIG", path)
}

func TestCFStatusOutputFakeBinary(t *testing.T) {
	pinHermeticConfig(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "chromium-146.0.7680.177.5", "chrome")
	if err := os.MkdirAll(filepath.Dir(bin), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOAKBROWSER_CACHE_DIR", dir)
	t.Setenv("CLOAKBROWSER_BINARY_PATH", "")

	out, err := executeCF(t, "status")
	if err != nil {
		t.Fatalf("status: %v (out: %s)", err, out)
	}
	for _, want := range []string{
		"146.0.7680.177.5",
		"free",     // license tier
		"chromium", // binary mention
		filepath.Base(bin),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output must mention %q:\n%s", want, out)
		}
	}
}

func TestCFStatusBinaryOverride(t *testing.T) {
	pinHermeticConfig(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-chrome")
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOAKBROWSER_BINARY_PATH", bin)
	t.Setenv("CLOAKBROWSER_CACHE_DIR", t.TempDir())

	out, err := executeCF(t, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, bin) {
		t.Errorf("status must show the override binary %q:\n%s", bin, out)
	}
	if !strings.Contains(out, "user") {
		t.Errorf("status must mark the override channel:\n%s", out)
	}
}

func TestCFClearWipesStore(t *testing.T) {
	pinHermeticConfig(t)
	data := t.TempDir()
	t.Setenv("ANICLI_DATA", data)
	// Seed a store entry via the same path cf clear reads.
	storePath := filepath.Join(data, "cfstore.json")
	if err := os.WriteFile(storePath, []byte(`{"animego.one":{"cookies":[],"user_agent":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := executeCF(t, "clear"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	raw, err := os.ReadFile(storePath) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("store file must survive clear (wiped content): %v", err)
	}
	if strings.Contains(string(raw), "animego.one") {
		t.Errorf("clear must wipe hosts, got %s", raw)
	}
}

func TestCFSolveUnknownProvider(t *testing.T) {
	t.Setenv("ANICLI_DATA", t.TempDir())
	_, err := executeCF(t, "solve", "nope")
	if err == nil {
		t.Fatal("unknown provider must fail")
	}
	if !strings.Contains(err.Error(), "anilibria") {
		t.Errorf("error must list known providers, got: %v", err)
	}
}

func TestCFCommandTreeRUHelp(t *testing.T) {
	out, err := executeCF(t, "--help")
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"status", "solve", "clear", "login", "logout"} {
		if !strings.Contains(out, want) {
			t.Errorf("cf help must list %q:\n%s", want, out)
		}
	}
}

// licenseAPIServer serves the validate endpoint for login/logout
// tests (routed through $CLOAKBROWSER_API_URL).
func licenseAPIServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/license/validate" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCFLoginSavesKey(t *testing.T) {
	srv := licenseAPIServer(t, `{"valid":true,"plan":"pro","expires":"2099-01-01"}`)
	cache := t.TempDir()
	t.Setenv("CLOAKBROWSER_API_URL", srv.URL)
	t.Setenv("CLOAKBROWSER_CACHE_DIR", cache)
	t.Setenv("CLOAKBROWSER_LICENSE_KEY", "")

	out, err := executeCF(t, "login", "KEY-1")
	if err != nil {
		t.Fatalf("login: %v (out: %s)", err, out)
	}
	for _, want := range []string{"pro", "2099-01-01", "license.key"} {
		if !strings.Contains(out, want) {
			t.Errorf("login output must mention %q:\n%s", want, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(cache, "license.key")) //nolint:gosec // test-owned temp path
	if err != nil || strings.TrimSpace(string(raw)) != "KEY-1" {
		t.Errorf("license.key = %q, %v — want KEY-1", raw, err)
	}
}

func TestCFLoginInvalidKeyRejected(t *testing.T) {
	srv := licenseAPIServer(t, `{"valid":false,"plan":"","expires":""}`)
	cache := t.TempDir()
	t.Setenv("CLOAKBROWSER_API_URL", srv.URL)
	t.Setenv("CLOAKBROWSER_CACHE_DIR", cache)
	t.Setenv("CLOAKBROWSER_LICENSE_KEY", "")

	_, err := executeCF(t, "login", "KEY-BAD")
	if err == nil {
		t.Fatal("invalid key must fail login")
	}
	if _, statErr := os.Stat(filepath.Join(cache, "license.key")); !os.IsNotExist(statErr) {
		t.Errorf("invalid key must not be saved")
	}
}

func TestCFLoginWithoutKeyPrintsInstructions(t *testing.T) {
	t.Setenv("CLOAKBROWSER_CACHE_DIR", t.TempDir())
	out, err := executeCF(t, "login")
	if err != nil {
		t.Fatalf("login without arg must not fail: %v", err)
	}
	if !strings.Contains(out, "https://cloakbrowser.dev/free") {
		t.Errorf("instructions must point at the free key URL:\n%s", out)
	}
}

func TestCFLogoutRemovesKey(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("CLOAKBROWSER_CACHE_DIR", cache)
	if err := os.WriteFile(filepath.Join(cache, "license.key"), []byte("KEY-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeCF(t, "logout")
	if err != nil {
		t.Fatalf("logout: %v (out: %s)", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(cache, "license.key")); !os.IsNotExist(statErr) {
		t.Errorf("logout must remove license.key")
	}
}

func TestCFStatusShowsLicenseTierFromCache(t *testing.T) {
	pinHermeticConfig(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "chromium-146.0.7680.177.5", "chrome")
	if err := os.MkdirAll(filepath.Dir(bin), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOAKBROWSER_CACHE_DIR", dir)
	t.Setenv("CLOAKBROWSER_BINARY_PATH", "")

	// Prime a valid license cache for KEY-1 directly (status must
	// serve it without network).
	entry := `{"key_sha256":"` + sha256HexRaw("KEY-1") + `","valid":true,"plan":"pro","expires":"2099-01-01","fetched_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, ".license_cache"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "license.key"), []byte("KEY-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeCF(t, "status")
	if err != nil {
		t.Fatalf("status: %v (out: %s)", err, out)
	}
	for _, want := range []string{"pro", "2099-01-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("status must show the cached pro license (%q):\n%s", want, out)
		}
	}
}

func TestCFStatusProLicenseFreeBinaryShowsGapNote(t *testing.T) {
	pinHermeticConfig(t)
	// The tier display reflects what actually launches: a valid key
	// with only a free-line binary cached shows the free binary plus
	// an explicit "pro not installed" gap note — never "канал pro" on
	// a free binary's path.
	dir := t.TempDir()
	bin := filepath.Join(dir, "chromium-146.0.7680.177.5", "chrome")
	if err := os.MkdirAll(filepath.Dir(bin), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOAKBROWSER_CACHE_DIR", dir)
	t.Setenv("CLOAKBROWSER_BINARY_PATH", "")

	entry := `{"key_sha256":"` + sha256HexRaw("KEY-1") + `","valid":true,"plan":"pro","expires":"2099-01-01","fetched_at":"` +
		time.Now().UTC().Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(dir, ".license_cache"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "license.key"), []byte("KEY-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := executeCF(t, "status")
	if err != nil {
		t.Fatalf("status: %v (out: %s)", err, out)
	}
	if want := "(канал free)"; !strings.Contains(out, want) {
		t.Errorf("the free-line binary must render its factual channel %q:\n%s", want, out)
	}
	if !strings.Contains(out, "не установлен") {
		t.Errorf("a pro license over a free-only cache must show the pro-not-installed gap:\n%s", out)
	}
	// PR86: cf install is removed — the gap note carries the
	// auto-install wording instead of the removed command.
	if strings.Contains(out, "anicli cf install") {
		t.Errorf("the gap note must not reference the removed command:\n%s", out)
	}
}

// sha256HexRaw renders the bare hex sha256 of s.
func sha256HexRaw(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
