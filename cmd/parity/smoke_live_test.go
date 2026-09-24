//go:build live

// Live smoke suite (PR44): runs the REAL registry against the REAL
// sites. Opt-in only — `go test -tags live -run TestSmoke ./...`;
// the default test run stays network-free.
//
// Routing: the config is resolved exactly like the parity tool
// (ANICLI_CONFIG > XDG > ~/.config/anicli/settings.toml); set
// SMOKE_PROXY (e.g. http://127.0.0.1:10809) to run the whole suite
// through a proxy, or SMOKE_PROVIDER=<id> to smoke one provider.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveSmokeArgs composes the parity invocation from the env knobs.
// PR80: without an explicit ANICLI_CONFIG the suite pins a HERMETIC
// temp config — it must never depend on the developer's real
// settings file (the cf.enabled migration error would fail the run
// on legacy machines).
func liveSmokeArgs(t *testing.T, target string) []string {
	args := []string{}
	if cfg := os.Getenv("ANICLI_CONFIG"); cfg != "" {
		args = append(args, "--config", cfg)
	} else {
		cfg := "channel = \"auto\"\n"
		path := filepath.Join(t.TempDir(), "settings.toml")
		if err := os.WriteFile(path, []byte("[cf]\n"+cfg), 0o600); err != nil {
			t.Fatalf("seed hermetic config: %v", err)
		}
		args = append(args, "--config", path)
	}
	if proxy := os.Getenv("SMOKE_PROXY"); proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	return append(args, "smoke", target)
}

// TestSmokeAllProvidersLive: the full damage table — every registered
// non-credential provider must pass the chain (search>0, dubs>=1,
// streams>=1); any FAIL row fails the test. Known-dead providers are
// the desired visibility: their FAIL is reported honestly, never
// special-cased.
func TestSmokeAllProvidersLive(t *testing.T) {
	// Browser-transport chains serialize a dozen
	// navigations per surfaced result — the 90s plain-HTTP default
	// starves them mid-surface. The live suite is the acceptance
	// harness: it gets the same 6m budget the CLI flag exposes.
	d := realDeps()
	d.smokeTimeout = 6 * time.Minute
	var out, errOut strings.Builder
	code := run(liveSmokeArgs(t, "all"), &out, &errOut, d)

	t.Logf("smoke table:\n%s", out.String())
	if code != 0 {
		t.Fatalf("live smoke FAILED (exit %d), stderr: %s", code, errOut.String())
	}
}

// TestSmokeOneProviderLive: single-provider run for targeted reruns
// (SMOKE_PROVIDER=anilib go test -tags live -run TestSmokeOneProviderLive).
func TestSmokeOneProviderLive(t *testing.T) {
	target := os.Getenv("SMOKE_PROVIDER")
	if target == "" {
		t.Skip("set SMOKE_PROVIDER=<id> to smoke one provider")
	}
	var out, errOut strings.Builder
	code := run(liveSmokeArgs(t, target), &out, &errOut, realDeps())

	t.Logf("smoke table:\n%s", out.String())
	if code != 0 {
		t.Fatalf("live smoke of %s FAILED (exit %d), stderr: %s", target, code, errOut.String())
	}
}
