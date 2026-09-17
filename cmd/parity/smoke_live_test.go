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
	"strings"
	"testing"
)

// liveSmokeArgs composes the parity invocation from the env knobs.
func liveSmokeArgs(target string) []string {
	args := []string{}
	if cfg := os.Getenv("ANICLI_CONFIG"); cfg != "" {
		args = append(args, "--config", cfg)
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
	var out, errOut strings.Builder
	code := run(liveSmokeArgs("all"), &out, &errOut, realDeps())

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
	code := run(liveSmokeArgs(target), &out, &errOut, realDeps())

	t.Logf("smoke table:\n%s", out.String())
	if code != 0 {
		t.Fatalf("live smoke of %s FAILED (exit %d), stderr: %s", target, code, errOut.String())
	}
}
