package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestCFStatusOutputFakeBinary(t *testing.T) {
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
	for _, want := range []string{"install", "status", "solve", "clear"} {
		if !strings.Contains(out, want) {
			t.Errorf("cf help must list %q:\n%s", want, out)
		}
	}
}
