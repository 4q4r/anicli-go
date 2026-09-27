// Package i18n is the locale system of anicli: bundled TOML translation
// tables (embed.FS from the repository-root locales/ directory) plus
// user-contributed tables dropped into ~/.config/anicli/locales/.
//
// Contract:
//   - en.toml is the source of truth; every other table (bundled ru,
//     user-contributed ja.toml, …) may be sparse — missing keys resolve
//     from en, and a key missing everywhere renders as the key itself
//     (grep-able, honest failure display);
//   - a user file <userDir>/<locale>.toml overrides the bundled table of
//     the same locale key by key (customization without a rebuild);
//   - requesting a locale with neither a bundled nor a user table is a
//     fail-loud startup error naming the locale and searched paths;
//   - the locale is chosen once at startup from [general] locale in
//     settings.toml (default "en") and installed process-wide via Init;
//   - T() before Init() (tests, miswired callers) resolves from the
//     embedded en table — hermetic degradation, never a panic.
//
// Dependency direction: tui depends on i18n; this package imports
// nothing from the TUI (it only reads the root embed package for the
// bundled tables).
package i18n

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/BurntSushi/toml"

	anicli "github.com/an0nx/anicli-go"
)

// bundledDir is the prefix of the locale tables inside the embed.FS.
const bundledDir = "locales"

// Vals carries placeholder substitutions for one T() call: every
// {name} token in the value is replaced by the matching entry. Values
// are plain strings so call sites control formatting (strconv, as they
// already do).
type Vals map[string]string

// Bundle is an immutable pair of lookup tables: the active locale and
// the hard en fallback. Build one with Load; install process-wide with
// SetBundle (or Init).
type Bundle struct {
	locale string
	active map[string]string
	en     map[string]string
}

// Locale reports the locale name the bundle was built for.
func (b *Bundle) Locale() string { return b.locale }

// T resolves key against the active table, then the en table, then
// returns the key itself; {placeholders} are interpolated from vals
// (later maps win). Unknown {tokens} are left verbatim.
func (b *Bundle) T(key string, vals ...Vals) string {
	s, ok := b.active[key]
	if !ok {
		s, ok = b.en[key]
		if !ok {
			return key
		}
	}
	if len(vals) == 0 {
		return s
	}
	return interpolate(s, vals...)
}

// interpolate replaces {name} tokens in a single left-to-right pass so
// substituted values are never rescanned.
func interpolate(s string, vals ...Vals) string {
	merged := Vals{}
	for _, v := range vals {
		for k, val := range v {
			merged[k] = val
		}
	}
	var b strings.Builder
	for {
		open := strings.IndexByte(s, '{')
		if open < 0 {
			b.WriteString(s)
			return b.String()
		}
		close_ := strings.IndexByte(s[open:], '}')
		if close_ < 0 {
			b.WriteString(s)
			return b.String()
		}
		close_ += open
		b.WriteString(s[:open])
		if v, ok := merged[s[open+1 : close_]]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[open : close_+1])
		}
		s = s[close_+1:]
	}
}

// Load builds a bundle for locale from the bundled tables (an fs.FS
// whose tables live under locales/) overlaid by the user-contributed
// table userDir/<locale>.toml when present. The fallback table is en,
// likewise overlaid by userDir/en.toml. An empty userDir skips the user
// overlay (tests of the pure bundled path).
func Load(bundled fs.FS, userDir, locale string) (*Bundle, error) {
	en, err := loadTable(bundled, bundledDir+"/en.toml", userTable(userDir, "en.toml"))
	if err != nil {
		return nil, err
	}
	localeFile := bundledDir + "/" + locale + ".toml"
	var active map[string]string
	if locale == "en" {
		active = en
	} else {
		_, bundledErr := fs.Stat(bundled, localeFile)
		_, userErr := statUser(userDir, locale+".toml")
		if bundledErr != nil && userErr != nil {
			return nil, fmt.Errorf("i18n: unknown locale %q: no bundled %s in the binary and no user table at %s",
				locale, localeFile, filepath.Join(userDir, locale+".toml"))
		}
		active, err = loadTable(bundled, localeFile, userTable(userDir, locale+".toml"))
		if err != nil {
			return nil, err
		}
	}
	return &Bundle{locale: locale, active: active, en: en}, nil
}

// userTable returns a reader opener for a user-contributed table, or
// nil when userDir is empty. Existence is checked lazily by loadTable.
func userTable(userDir, name string) func() (fs.File, error) {
	if userDir == "" {
		return nil
	}
	path := filepath.Join(userDir, name)
	return func() (fs.File, error) { return os.Open(path) }
}

// statUser reports whether a user table exists (used to distinguish an
// unknown locale from a bundled-only one).
func statUser(userDir, name string) (fs.FileInfo, error) {
	if userDir == "" {
		return nil, fs.ErrNotExist
	}
	return os.Stat(filepath.Join(userDir, name))
}

// loadTable parses a TOML table into a flat string map: the bundled copy
// first, then the user overlay on top (nil opener skips the overlay). A
// missing bundled file with no overlay is NOT an error here — Load
// checks locale existence before calling; the en fallback file is
// mandatory and its absence surfaces as a decode error naming the file.
func loadTable(bundled fs.FS, bundledName string, user func() (fs.File, error)) (map[string]string, error) {
	table := map[string]string{}
	f, err := bundled.Open(bundledName)
	if err == nil {
		if err := decodeInto(f, bundledName, table); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("i18n: close %s: %w", bundledName, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("i18n: open %s: %w", bundledName, err)
	}
	if user != nil {
		uf, uerr := user()
		if uerr == nil {
			if err := decodeInto(uf, "user table", table); err != nil {
				_ = uf.Close()
				return nil, err
			}
			if err := uf.Close(); err != nil {
				return nil, fmt.Errorf("i18n: close user table: %w", err)
			}
		} else if !errors.Is(uerr, fs.ErrNotExist) {
			return nil, fmt.Errorf("i18n: open user table: %w", uerr)
		}
	}
	return table, nil
}

// decodeInto decodes one TOML locale file into table, wrapping parser
// errors with the source name (mirroring the config package's fail-loud
// parser-line contract).
func decodeInto(f fs.File, name string, table map[string]string) error {
	var parsed map[string]string
	if _, err := toml.NewDecoder(f).Decode(&parsed); err != nil {
		return fmt.Errorf("i18n: parse %s: %w", name, err)
	}
	for k, v := range parsed {
		table[k] = v
	}
	return nil
}

// userLocalesDir resolves the community-contributed tables directory:
// $XDG_CONFIG_HOME/anicli/locales > ~/.config/anicli/locales, mirroring
// the config package's settings-path resolution (minus ANICLI_CONFIG,
// which overrides the settings FILE, not the conventional directory).
func userLocalesDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "anicli", "locales")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "anicli", "locales")
}

// The process-wide bundle. Installed once at startup by Init; read on
// every T() call via an atomic pointer so screens may translate from
// any goroutine without locks.
var installed atomic.Pointer[Bundle]

// lazyEN parses the embedded en table once, on first use before Init.
var lazyEN sync.Once

// Init loads the bundled tables plus the user locales directory for
// locale and installs the result process-wide. Called once at startup
// from the CLI layer; unknown locales and malformed tables fail loud.
// An empty locale means the default ("en") — a hand-edited empty value
// must not brick the startup.
func Init(locale string) error {
	if locale == "" {
		locale = "en"
	}
	b, err := Load(anicli.Locales, userLocalesDir(), locale)
	if err != nil {
		return err
	}
	SetBundle(b)
	return nil
}

// SetBundle installs b as the process-wide table (dependency-injection
// seam: the CLI builds the bundle and hands it to the TUI via Deps; the
// TUI installs exactly what it was given).
func SetBundle(b *Bundle) { installed.Store(b) }

// Active returns the installed bundle, or nil before Init/SetBundle.
func Active() *Bundle { return installed.Load() }

// T resolves key against the process-wide bundle. Before Init (tests,
// miswired callers) it resolves from the embedded en table.
func T(key string, vals ...Vals) string {
	if b := installed.Load(); b != nil {
		return b.T(key, vals...)
	}
	lazyEN.Do(func() {
		en, err := loadTable(anicli.Locales, bundledDir+"/en.toml", nil)
		if err != nil {
			// The embedded table is compiled in; a parse failure is a
			// build-time bug. Surface the key (the honest degraded
			// display) instead of panicking inside render paths.
			return
		}
		SetBundle(&Bundle{locale: "en", active: en, en: en})
	})
	if b := installed.Load(); b != nil {
		return b.T(key, vals...)
	}
	return key
}

// keys returns the sorted key set of a table (test helper for parity).
func (b *Bundle) keys() []string {
	out := make([]string, 0, len(b.active))
	for k := range b.active {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
