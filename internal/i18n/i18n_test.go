package i18n

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"testing/fstest"

	anicli "github.com/an0nx/anicli-go"
)

// testFS builds an in-memory bundled-locales filesystem shaped like the
// real embed.FS (paths relative to the repo root: "locales/<lang>.toml").
func testFS(files map[string]string) fstest.MapFS {
	fs := fstest.MapFS{}
	for name, content := range files {
		fs[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fs
}

const enToml = "\"menu.watch\" = \"▶ Watch\"\n" +
	"\"menu.exit\" = \"🚪 Exit\"\n" +
	"\"search.found\" = \"Found: {found} · No results/errors: {failed}\"\n"

const ruToml = "\"menu.watch\" = \"▶ Смотреть\"\n"

// TestLoadActiveLocaleLoadsAndTranslates covers the primary contract:
// T(key) returns the value from the ACTIVE locale's table.
func TestLoadActiveLocaleLoadsAndTranslates(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml, "locales/ru.toml": ruToml})
	b, err := Load(fs, t.TempDir(), "ru")
	if err != nil {
		t.Fatalf("Load(ru) failed: %v", err)
	}
	if got := b.T("menu.watch"); got != "▶ Смотреть" {
		t.Fatalf("T(menu.watch) with locale=ru = %q, want %q", got, "▶ Смотреть")
	}
}

// TestLoadMissingKeyFallsBackToEN covers the hard fallback chain: a key
// present in en.toml but missing from the active locale resolves from en.
func TestLoadMissingKeyFallsBackToEN(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml, "locales/ru.toml": ruToml})
	b, err := Load(fs, t.TempDir(), "ru")
	if err != nil {
		t.Fatalf("Load(ru) failed: %v", err)
	}
	if got := b.T("menu.exit"); got != "🚪 Exit" {
		t.Fatalf("T(menu.exit) with locale=ru = %q, want the en fallback %q", got, "🚪 Exit")
	}
}

// TestMissingKeyEverywhereReturnsKey covers the end of the fallback chain:
// locale → en → the key itself (grep-able, honest failure display).
func TestMissingKeyEverywhereReturnsKey(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml})
	b, err := Load(fs, t.TempDir(), "en")
	if err != nil {
		t.Fatalf("Load(en) failed: %v", err)
	}
	if got := b.T("no.such.key"); got != "no.such.key" {
		t.Fatalf("T(no.such.key) = %q, want the key itself", got)
	}
}

// TestPlaceholderInterpolation covers {name} substitution in values.
func TestPlaceholderInterpolation(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml})
	b, err := Load(fs, t.TempDir(), "en")
	if err != nil {
		t.Fatalf("Load(en) failed: %v", err)
	}
	got := b.T("search.found", Vals{"found": "2", "failed": "1"})
	want := "Found: 2 · No results/errors: 1"
	if got != want {
		t.Fatalf("T(search.found) = %q, want %q", got, want)
	}
}

// TestUnknownLocaleIsAnError covers the fail-loud contract: a locale with
// neither a bundled file nor a user file is a startup error naming the
// locale and the searched locations.
func TestUnknownLocaleIsAnError(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml})
	_, err := Load(fs, t.TempDir(), "xx")
	if err == nil {
		t.Fatal("Load(xx) succeeded, want an error for an unknown locale")
	}
	for _, want := range []string{"xx", "xx.toml"} {
		if !contains(err.Error(), want) {
			t.Fatalf("Load(xx) error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestUserFileOverridesBundled covers community contributions: a user
// file <userDir>/<locale>.toml overrides the bundled table of the same
// locale (customization), key by key.
func TestUserFileOverridesBundled(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml, "locales/ru.toml": ruToml})
	userDir := t.TempDir()
	writeFile(t, userDir+"/ru.toml", "\"menu.watch\" = \"▶ СМОТРЕТЬ (custom)\"\n")
	b, err := Load(fs, userDir, "ru")
	if err != nil {
		t.Fatalf("Load(ru) with user file failed: %v", err)
	}
	if got := b.T("menu.watch"); got != "▶ СМОТРЕТЬ (custom)" {
		t.Fatalf("T(menu.watch) = %q, want the user override", got)
	}
	// Keys absent from the user file still resolve from the bundled table.
	if got := b.T("menu.exit"); got != "🚪 Exit" {
		t.Fatalf("T(menu.exit) = %q, want the en fallback (user file has no such key)", got)
	}
}

// TestUserOnlyLocaleLoads covers the ja.toml story: a locale that exists
// ONLY in the user dir (no bundled file) loads with en fallback.
func TestUserOnlyLocaleLoads(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml})
	userDir := t.TempDir()
	writeFile(t, userDir+"/ja.toml", "\"menu.watch\" = \"▶ 視聴\"\n")
	b, err := Load(fs, userDir, "ja")
	if err != nil {
		t.Fatalf("Load(ja) from user dir failed: %v", err)
	}
	if got := b.T("menu.watch"); got != "▶ 視聴" {
		t.Fatalf("T(menu.watch) = %q, want the user-contributed value", got)
	}
	if got := b.T("menu.exit"); got != "🚪 Exit" {
		t.Fatalf("T(menu.exit) = %q, want the en fallback", got)
	}
}

// TestLocaleDefaultsToENWhenActiveFileMissing — requesting "en" with only
// en.toml bundled is the zero-config path and must never error.
func TestLocaleDefaultsToENWhenActiveFileMissing(t *testing.T) {
	fs := testFS(map[string]string{"locales/en.toml": enToml})
	if _, err := Load(fs, t.TempDir(), "en"); err != nil {
		t.Fatalf("Load(en) failed: %v", err)
	}
}

// TestTBeforeInitUsesEmbeddedEN covers the hermetic default: T() before
// Init() resolves from the REAL embedded en.toml (never panics, never
// returns empty for a known key).
func TestTBeforeInitUsesEmbeddedEN(t *testing.T) {
	t.Cleanup(resetInstalled)
	resetInstalled()
	if got := T("menu.watch"); got == "" || got == "menu.watch" {
		t.Fatalf("T(menu.watch) before Init = %q, want the embedded en value", got)
	}
}

// TestInitInstallsBundleForT covers the startup path: Init(locale) makes
// T() translate in the requested locale, SetBundle/Active round-trip.
func TestInitInstallsBundleForT(t *testing.T) {
	t.Cleanup(resetInstalled)
	resetInstalled()
	if err := Init("en"); err != nil {
		t.Fatalf("Init(en) failed: %v", err)
	}
	if got := T("menu.watch"); got != "▶ Watch" {
		t.Fatalf("T(menu.watch) after Init(en) = %q, want the embedded en value", got)
	}
	if err := Init("ru"); err != nil {
		t.Fatalf("Init(ru) failed: %v", err)
	}
	if got := T("menu.watch"); got != "▶ Смотреть" {
		t.Fatalf("T(menu.watch) after Init(ru) = %q, want the embedded ru value", got)
	}
	if Active() == nil || Active().Locale() != "ru" {
		t.Fatalf("Active() after Init(ru) = %+v, want a bundle with locale ru", Active())
	}
}

// TestInitEmptyLocaleMeansRU covers the hand-edited empty value: it
// must degrade to the default locale ("ru" as of PR113b — the original
// app is Russian), not fail loud.
func TestInitEmptyLocaleMeansRU(t *testing.T) {
	t.Cleanup(resetInstalled)
	resetInstalled()
	if err := Init(""); err != nil {
		t.Fatalf("Init(\"\") failed: %v", err)
	}
	if got := T("menu.watch"); got != "▶ Смотреть" {
		t.Fatalf("T(menu.watch) after Init(\"\") = %q, want the ru value", got)
	}
	if Active() == nil || Active().Locale() != "ru" {
		t.Fatalf("Active() after Init(\"\") = %+v, want a bundle with locale ru", Active())
	}
}

// TestInitUnknownLocaleFailsLoud covers Init's fail-loud contract.
func TestInitUnknownLocaleFailsLoud(t *testing.T) {
	t.Cleanup(resetInstalled)
	resetInstalled()
	if err := Init("xx"); err == nil {
		t.Fatal("Init(xx) succeeded, want an error")
	}
}

// TestBundledParityENRU guards the translation inventory: ru.toml must
// declare EXACTLY the same key set as en.toml — no missing translations,
// no stale keys. New strings added to en.toml without ru break this test.
func TestBundledParityENRU(t *testing.T) {
	en := mustBundled(t, "en")
	ru := mustBundled(t, "ru")
	enActive, ruActive := en.active, ru.active
	for _, key := range en.keys() {
		if _, ok := ruActive[key]; !ok {
			t.Errorf("ru.toml is missing key %q (present in en.toml)", key)
		}
	}
	for _, key := range ru.keys() {
		if _, ok := enActive[key]; !ok {
			t.Errorf("ru.toml has stale key %q (absent from en.toml)", key)
		}
	}
}

// writeFile creates a user-contributed locale table in userDir.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	//nolint:gosec // test fixture path, not sensitive
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustBundled loads a bundled locale table through the REAL embedded
// locales (the parity test guards the shipped files, not fixtures).
func mustBundled(t *testing.T, locale string) *Bundle {
	t.Helper()
	b, err := Load(anicli.Locales, "", locale)
	if err != nil {
		t.Fatalf("Load(bundled %s) failed: %v", locale, err)
	}
	return b
}

// resetInstalled clears the process-wide bundle (tests of the
// pre-Init lazy path install nothing).
func resetInstalled() { SetBundle(nil) }

// TestEveryReferencedKeyExists guards the code→table direction: every
// i18n key referenced in the tui package source must exist in the
// embedded en table (the ru side is guarded by TestBundledParityENRU).
// A renamed key without a table update renders the raw key on screen —
// this test turns that into a build-time failure.
func TestEveryReferencedKeyExists(t *testing.T) {
	en := mustBundled(t, "en")
	files, err := filepath.Glob("../tui/*.go")
	if err != nil {
		t.Fatalf("glob tui sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no tui sources found — wrong relative path?")
	}
	re := regexp.MustCompile(`i18n\.T\("([a-z_.]+)"`)
	for _, f := range files {
		//nolint:gosec // the test reads its own package sources
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			key := m[1]
			if _, ok := en.active[key]; !ok {
				t.Errorf("%s references key %q which is missing from locales/en.toml", f, key)
			}
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
