package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pythonUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

func TestDefaults(t *testing.T) {
	t.Parallel()

	got := Default()

	if got.General.DataDir != "" {
		t.Errorf("General.DataDir = %q, want empty (auto-resolve)", got.General.DataDir)
	}
	if want := 10 * time.Second; got.Network.ConnectTimeout != want {
		t.Errorf("Network.ConnectTimeout = %v, want %v (python connect_timeout, verbatim)", got.Network.ConnectTimeout, want)
	}
	if want := 30 * time.Second; got.Network.RequestTimeout != want {
		t.Errorf("Network.RequestTimeout = %v, want %v (python read_timeout, verbatim)", got.Network.RequestTimeout, want)
	}
	if got.Network.MaxParallel != 4 {
		t.Errorf("Network.MaxParallel = %d, want 4", got.Network.MaxParallel)
	}
	if got.Network.UserAgent != pythonUA {
		t.Errorf("Network.UserAgent = %q, want python default verbatim", got.Network.UserAgent)
	}
	if got.Network.ProxyURL != "" {
		t.Errorf("Network.ProxyURL = %q, want empty (direct)", got.Network.ProxyURL)
	}
	if got.Player.Path != "mpv" {
		t.Errorf("Player.Path = %q, want mpv (python bin_path)", got.Player.Path)
	}
	if got.Player.Quality != "1080" {
		t.Errorf("Player.Quality = %q, want 1080 (python default_quality)", got.Player.Quality)
	}
	if !got.Shikimori.Enabled {
		t.Error("Shikimori.Enabled = false, want true (core feature: tracking + first-run setup)")
	}
	if got.Shikimori.Session != "" || got.Shikimori.AccessToken != "" {
		t.Error("Shikimori secrets must default empty")
	}
	wantOrder := []string{"aniskip", "anime_skip", "intro_skipper"}
	if len(got.Skip.ProvidersOrder) != len(wantOrder) {
		t.Fatalf("Skip.ProvidersOrder = %v, want %v", got.Skip.ProvidersOrder, wantOrder)
	}
	for i, p := range wantOrder {
		if got.Skip.ProvidersOrder[i] != p {
			t.Fatalf("Skip.ProvidersOrder = %v, want %v", got.Skip.ProvidersOrder, wantOrder)
		}
	}
	if !got.Skip.AnimeSkipEnabled || !got.Skip.IntroSkipperEnabled {
		t.Error("Skip provider toggles must default true")
	}
	if got.Download.Dir != "" || got.Download.MaxConcurrency != 2 {
		t.Errorf("Download = {%q %d}, want {empty 2}", got.Download.Dir, got.Download.MaxConcurrency)
	}
	if got.API.Enabled {
		t.Error("API.Enabled = true, want false (opt-in)")
	}
	if got.API.Bind != "127.0.0.1:8765" {
		t.Errorf("API.Bind = %q, want 127.0.0.1:8765", got.API.Bind)
	}
	if want := 15 * time.Minute; got.API.TokenTTL != want {
		t.Errorf("API.TokenTTL = %v, want %v", got.API.TokenTTL, want)
	}
	if got.Providers.Kodik.Token != "" {
		t.Errorf("Providers.Kodik.Token = %q, want empty (must be user-supplied)", got.Providers.Kodik.Token)
	}
	if got.Providers.HDRezka.BaseURL != "" {
		t.Errorf("Providers.HDRezka.BaseURL = %q, want empty (built-in default)", got.Providers.HDRezka.BaseURL)
	}
}

// TestHDRezkaBaseURLFromFile: [providers.hdrezka] base_url overrides the
// provider's mirror route (PR72): the rezka mirror family geo-fences
// differently per domain, so the working route must be user-swappable
// without a rebuild.
func TestHDRezkaBaseURLFromFile(t *testing.T) {
	path := writeTOML(t, `
[providers.hdrezka]
base_url = "https://rezka-mirror.example"
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Providers.HDRezka.BaseURL != "https://rezka-mirror.example" {
		t.Errorf("Providers.HDRezka.BaseURL = %q, want file value", got.Providers.HDRezka.BaseURL)
	}
}

func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

func TestLoadFileOverrides(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[network]
connect_timeout = "3s"
proxy_url = "socks5://127.0.0.1:9050"

[player]
path = "C:\\mpv\\mpv.exe"
quality = "720"

[shikimori]
enabled = true
session = "file-session"

[api]
enabled = true
bind = "127.0.0.1:9999"
auth_secret_key = "test-secret"

[download]
max_concurrency = 5

[providers.kodik]
token = "file-kodik-token"
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Overridden values.
	if got.Network.ConnectTimeout != 3*time.Second {
		t.Errorf("ConnectTimeout = %v, want 3s", got.Network.ConnectTimeout)
	}
	if got.Network.ProxyURL != "socks5://127.0.0.1:9050" {
		t.Errorf("ProxyURL = %q", got.Network.ProxyURL)
	}
	if got.Player.Path != `C:\mpv\mpv.exe` || got.Player.Quality != "720" {
		t.Errorf("Player = {%q %q}", got.Player.Path, got.Player.Quality)
	}
	if !got.Shikimori.Enabled || got.Shikimori.Session != "file-session" {
		t.Errorf("Shikimori = {%v %q}", got.Shikimori.Enabled, got.Shikimori.Session)
	}
	if !got.API.Enabled || got.API.Bind != "127.0.0.1:9999" {
		t.Errorf("API = {%v %q}", got.API.Enabled, got.API.Bind)
	}
	if got.Download.MaxConcurrency != 5 {
		t.Errorf("MaxConcurrency = %d, want 5", got.Download.MaxConcurrency)
	}
	if got.Providers.Kodik.Token != "file-kodik-token" {
		t.Errorf("Providers.Kodik.Token = %q, want file value", got.Providers.Kodik.Token)
	}

	// Untouched values keep their defaults (file must not zero them).
	if got.Network.RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout = %v, want default 30s", got.Network.RequestTimeout)
	}
	if got.Network.UserAgent != pythonUA {
		t.Errorf("UserAgent = %q, want default", got.Network.UserAgent)
	}
	if got.API.TokenTTL != 15*time.Minute {
		t.Errorf("TokenTTL = %v, want default 15m", got.API.TokenTTL)
	}
	if len(got.Skip.ProvidersOrder) != 3 || got.Skip.ProvidersOrder[0] != "aniskip" {
		t.Errorf("ProvidersOrder = %v, want default", got.Skip.ProvidersOrder)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	got, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing file must yield defaults without error, got %v", err)
	}
	if got.API.Bind != "127.0.0.1:8765" {
		t.Fatalf("missing file must yield defaults, got bind %q", got.API.Bind)
	}
}

func TestLoadMalformed(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, "[network\nconnect_timeout = ")
	_, err := Load(path)
	if err == nil {
		t.Fatal("malformed TOML must fail loud")
	}
	if !strings.Contains(err.Error(), filepath.Base(path)) {
		t.Errorf("error must point at the file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error must include line information, got: %v", err)
	}
}

func TestLoadUnknownKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		key     string
	}{
		{
			name:    "unknown top-level section",
			content: "[downloadz]\ndir = \"/tmp\"\n",
			key:     "downloadz",
		},
		{
			name:    "unknown key inside known section",
			content: "[network]\nfrobnicate = true\n",
			key:     "network.frobnicate",
		},
		{
			name:    "unknown key inside providers.kodik",
			content: "[providers.kodik]\nbogus = true\n",
			key:     "providers.kodik.bogus",
		},
		{
			name:    "unknown top-level key",
			content: "top_level_secret = 1\n",
			key:     "top_level_secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := writeTOML(t, tt.content)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("unknown key %q must fail loud", tt.key)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error must name the unknown key %q, got: %v", tt.key, err)
			}
			if !strings.Contains(err.Error(), filepath.Base(path)) {
				t.Errorf("error must point at the file, got: %v", err)
			}
		})
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	t.Setenv("ANICLI_PROXY_URL", "http://127.0.0.1:8080")
	t.Setenv("ANICLI_SHIKIMORI_SESSION", "env-session")
	t.Setenv("ANICLI_KODIK_TOKEN", "env-kodik-token")

	path := writeTOML(t, `
[network]
proxy_url = "socks5://file-proxy:9050"

[shikimori]
session = "file-session"
access_token = "file-token"

[providers.kodik]
token = "file-kodik-token"
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Env beats file for the two secret env vars.
	if got.Network.ProxyURL != "http://127.0.0.1:8080" {
		t.Errorf("ProxyURL = %q, want env value to win", got.Network.ProxyURL)
	}
	if got.Shikimori.Session != "env-session" {
		t.Errorf("Session = %q, want env value to win", got.Shikimori.Session)
	}
	if got.Providers.Kodik.Token != "env-kodik-token" {
		t.Errorf("Providers.Kodik.Token = %q, want env value to win", got.Providers.Kodik.Token)
	}
	// Non-secret fields stay file-driven.
	if got.Shikimori.AccessToken != "file-token" {
		t.Errorf("AccessToken = %q, want file value", got.Shikimori.AccessToken)
	}
}

func TestLoadEnvOnlyNoFile(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	t.Setenv("ANICLI_PROXY_URL", "http://env-only:3128")

	got, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Network.ProxyURL != "http://env-only:3128" {
		t.Errorf("ProxyURL = %q, want env value with missing file", got.Network.ProxyURL)
	}
}

func TestResolveConfigPathPrecedence(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "explicit beats env",
			env:  map[string]string{"ANICLI_CONFIG": "/from/env/settings.toml"},
			want: "/explicit/settings.toml",
		},
		{
			name: "env beats xdg",
			env: map[string]string{
				"ANICLI_CONFIG":   "/from/env/settings.toml",
				"XDG_CONFIG_HOME": "/xdg-home",
			},
			want: "/from/env/settings.toml",
		},
		{
			name: "xdg beats home",
			// ANICLI_CONFIG reset: an ambient value would win over XDG and
			// make this subtest environment-dependent.
			env:  map[string]string{"ANICLI_CONFIG": "", "XDG_CONFIG_HOME": "/xdg-home"},
			want: "/xdg-home/anicli/settings.toml",
		},
		{
			name: "home fallback",
			// ANICLI_CONFIG and XDG_CONFIG_HOME cleared explicitly: the
			// result must not depend on the ambient developer environment.
			env: map[string]string{
				"ANICLI_CONFIG":   "",
				"HOME":            "/home/tester",
				"XDG_CONFIG_HOME": "",
			},
			want: "/home/tester/.config/anicli/settings.toml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			explicit := ""
			if tt.name == "explicit beats env" {
				explicit = tt.want
			}
			got := ResolveConfigPath(explicit)
			if got != tt.want {
				t.Fatalf("ResolveConfigPath(%q) = %q, want %q", explicit, got, tt.want)
			}
		})
	}
}

func TestResolveConfigPathMissing(t *testing.T) {
	t.Parallel()

	got := ResolveConfigPath("/nonexistent/absolute/path/settings.toml")
	if got != "/nonexistent/absolute/path/settings.toml" {
		t.Fatalf("explicit path must be returned verbatim, got %q", got)
	}
}

func TestDataDir(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	t.Run("ANICLI_DATA wins", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "custom-data")
		t.Setenv("ANICLI_DATA", want)
		got, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir: %v", err)
		}
		if got != want {
			t.Fatalf("DataDir = %q, want %q", got, want)
		}
	})

	t.Run("XDG_DATA_HOME next", func(t *testing.T) {
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)
		got, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir: %v", err)
		}
		want := filepath.Join(xdg, "anicli")
		if got != want {
			t.Fatalf("DataDir = %q, want %q", got, want)
		}
	})

	t.Run("home fallback creates dirs", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_DATA_HOME", "")

		got, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir: %v", err)
		}
		want := filepath.Join(home, ".local", "share", "anicli")
		if got != want {
			t.Fatalf("DataDir = %q, want %q", got, want)
		}
		info, err := os.Stat(got)
		if err != nil {
			t.Fatalf("DataDir must create the directory: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("%q is not a directory", got)
		}
	})
}

// TestExampleFileIsValid guards the shipped settings.example.toml against
// rot: it must parse cleanly, carry no unknown keys and pass validation.
func TestExampleFileIsValid(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "settings.example.toml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("example file not found (running from unexpected cwd?): %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("settings.example.toml must stay loadable: %v", err)
	}
	if s.Network.ConnectTimeout != 10*time.Second {
		t.Errorf("example connect_timeout = %v, want 10s", s.Network.ConnectTimeout)
	}
	if s.API.Bind != DefaultBind {
		t.Errorf("example bind = %q, want %q", s.API.Bind, DefaultBind)
	}
}

func TestDBPath(t *testing.T) {
	// Uses t.Setenv: no t.Parallel here.
	t.Run("env override wins", func(t *testing.T) {
		t.Setenv("ANICLI_DB_URL", "/var/lib/custom.db")
		s := Default()
		got, err := s.DBPath()
		if err != nil {
			t.Fatalf("DBPath: %v", err)
		}
		if got != "/var/lib/custom.db" {
			t.Fatalf("DBPath = %q, want env value", got)
		}
	})

	t.Run("default under data dir", func(t *testing.T) {
		t.Setenv("ANICLI_DB_URL", "")
		data := t.TempDir()
		s := Default()
		s.General.DataDir = data

		got, err := s.DBPath()
		if err != nil {
			t.Fatalf("DBPath: %v", err)
		}
		want := filepath.Join(data, "anicli.db")
		if got != want {
			t.Fatalf("DBPath = %q, want %q", got, want)
		}
	})
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Settings)
		wantErr string
	}{
		{
			name:   "defaults are valid",
			mutate: func(*Settings) {},
		},
		{
			name:    "bad bind host:port",
			mutate:  func(s *Settings) { s.API.Bind = "no-port-here" },
			wantErr: "bind",
		},
		{
			name:    "bad proxy scheme",
			mutate:  func(s *Settings) { s.Network.ProxyURL = "ftp://bad" },
			wantErr: "proxy",
		},
		{
			name:   "empty proxy is direct and valid",
			mutate: func(s *Settings) { s.Network.ProxyURL = "" },
		},
		{
			name:   "socks5 proxy valid",
			mutate: func(s *Settings) { s.Network.ProxyURL = "socks5://127.0.0.1:9050" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := Default()
			tt.mutate(&s)
			err := s.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error %q missing %q", err, tt.wantErr)
			}
		})
	}
}

// TestProvidersExclusionSettings pins the [providers] exclusion keys
// (PR23): exclude drops providers from the search fan-out by id,
// exclude_streams drops dub streams by name regex. Defaults exclude
// nothing; an invalid exclude_streams regex fails Load loud at startup
// instead of being ignored at request time.
func TestProvidersExclusionSettings(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, `
[providers]
exclude = ["gogoanime", "kodik"]
exclude_streams = ["трейлер", "реклама"]

[providers.kodik]
token = "file-kodik-token"
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Providers.Exclude) != 2 ||
		got.Providers.Exclude[0] != "gogoanime" || got.Providers.Exclude[1] != "kodik" {
		t.Errorf("Providers.Exclude = %v, want [gogoanime kodik]", got.Providers.Exclude)
	}
	if len(got.Providers.ExcludeStreams) != 2 ||
		got.Providers.ExcludeStreams[0] != "трейлер" || got.Providers.ExcludeStreams[1] != "реклама" {
		t.Errorf("Providers.ExcludeStreams = %v, want [трейлер реклама]", got.Providers.ExcludeStreams)
	}
	// The kodik sub-table must keep parsing beside the scalar keys.
	if got.Providers.Kodik.Token != "file-kodik-token" {
		t.Errorf("Providers.Kodik.Token = %q, want file value", got.Providers.Kodik.Token)
	}

	// Defaults: nothing excluded.
	def, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if len(def.Providers.Exclude) != 0 || len(def.Providers.ExcludeStreams) != 0 {
		t.Errorf("defaults must exclude nothing, got %v / %v",
			def.Providers.Exclude, def.Providers.ExcludeStreams)
	}

	// Invalid regex fails loud, naming the pattern.
	bad := writeTOML(t, `
[providers]
exclude_streams = ["([unclosed"]
`)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "exclude_streams") {
		t.Fatalf("invalid exclude_streams regex must fail loud naming the key, got %v", err)
	}
}

// TestLoadCFEnabledRemovedKey (PR80): the [cf] enabled knob is removed —
// CF is always on. A legacy settings file carrying the key must fail
// loud with the TARGETED migration message naming the exact fix, not
// the generic unknown-key error.
func TestLoadCFEnabledRemovedKey(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, "[cf]\nenabled = false\nsolve_timeout = \"90s\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load err = nil, want the removed-key migration error")
	}
	want := "[cf] enabled удалён — CF теперь всегда включён; удалите эту строку из settings.toml"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to contain %q", err, want)
	}
	// The generic unknown-key hint must not shadow the targeted fix.
	if strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("err = %v, want the targeted message without the generic hint", err)
	}
}

// TestLoadCFWithoutEnabledKeyStillLoads: the surviving [cf] keys keep
// loading; only the removed knob triggers the migration message.
func TestLoadCFWithoutEnabledKeyStillLoads(t *testing.T) {
	t.Parallel()

	path := writeTOML(t, "[cf]\nsolve_timeout = \"60s\"\nbrowser_idle_timeout = \"10s\"\n")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CF.SolveTimeout != 60*time.Second {
		t.Errorf("SolveTimeout = %v, want 60s", got.CF.SolveTimeout)
	}
	if got.CF.BrowserIdleTimeout != 10*time.Second {
		t.Errorf("BrowserIdleTimeout = %v, want 10s", got.CF.BrowserIdleTimeout)
	}
}
