package lua

import (
	"bytes"
	"testing"
)

// The PR140 settings SDK: anicli.provider_setting(key) reads the
// STRING value of providers.<id>.<key> for the script's OWN provider
// id — the config-read leg of the PR116 phase-3 plan, built for the
// kodik migration (the API token). The engine carries the flattened
// settings of the ONE provider being loaded (Config.ProviderSettings —
// the loader/factory wire it per id through the same seam as HTTP), so
// a script can never see another provider's settings.
//
// A missing key or a provider without settings yields Lua nil — NOT an
// error: the credential-gated provider decides what a missing token
// means (the kodik script fails loud naming the settings paths). The
// value is secret-bearing (API tokens): the SDK never logs it.

// newSettingsEngine builds an engine carrying one provider's settings
// plus the capture buffer its logger writes to.
func newSettingsEngine(t *testing.T, settings map[string]string) (*Engine, *bytes.Buffer) {
	t.Helper()
	log, buf := testLogger(t)
	cfg := DefaultConfig()
	cfg.ProviderSettings = settings
	return NewEngine(cfg, log), buf
}

// TestProviderSettingReturnsConfiguredValue: the configured string
// value comes back verbatim.
func TestProviderSettingReturnsConfiguredValue(t *testing.T) {
	t.Parallel()

	e, _ := newSettingsEngine(t, map[string]string{"token": "s3cret-value"})
	got, err := evalSDK(t, e, `return anicli.provider_setting("token")`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret-value" {
		t.Fatalf("provider_setting(token) = %q, want the configured value", got)
	}
}

// TestProviderSettingMissingKeyIsNil: a key the provider has no
// setting for is Lua nil, never an error — the script decides what a
// missing value means.
func TestProviderSettingMissingKeyIsNil(t *testing.T) {
	t.Parallel()

	e, _ := newSettingsEngine(t, map[string]string{"token": "s3cret-value"})
	got, err := evalSDK(t, e, `return type(anicli.provider_setting("nope"))`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "nil" {
		t.Fatalf("provider_setting(nope) type = %q, want nil", got)
	}
}

// TestProviderSettingWithoutSettingsIsNil: a provider whose id has no
// settings section at all reads nil for every key.
func TestProviderSettingWithoutSettingsIsNil(t *testing.T) {
	t.Parallel()

	e, _ := newSettingsEngine(t, nil)
	got, err := evalSDK(t, e, `return type(anicli.provider_setting("token"))`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "nil" {
		t.Fatalf("provider_setting(token) with no settings = %q, want nil", got)
	}
}

// TestProviderSettingNeverLogged: the token value is a secret — the
// SDK leg must never route it into the engine logger, on the success
// path, the deliberate-fail path or the load path.
func TestProviderSettingNeverLogged(t *testing.T) {
	t.Parallel()

	e, buf := newSettingsEngine(t, map[string]string{"token": "s3cret-value"})
	src := `return {
		id = "setty",
		search = function(query)
			local tok = anicli.provider_setting("token")
			anicli.log("info", "reading provider settings")
			anicli.fail("invalid_input", "deliberate probe failure")
		end,
		episodes = function(anime_url) return {} end,
		streams = function(episode_url, dub) return { dub_name = dub, links = {} } end,
	}`
	p, err := e.LoadProvider("setty", src)
	if err != nil {
		t.Fatalf("LoadProvider: %v", err)
	}
	if _, err := p.Search(t.Context(), "q"); err == nil {
		t.Fatal("the probe script must fail loud")
	}
	if got := buf.String(); bytes.Contains([]byte(got), []byte("s3cret-value")) {
		t.Fatalf("the engine logger leaked the settings value:\n%s", got)
	}
}
