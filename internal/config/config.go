// Package config loads anicli settings from TOML with layered overrides:
// defaults first, then the settings file, then ANICLI_* environment variables
// for secrets only (proxy URL, shikimori session cookie, database URL).
//
// It is hand-rolled on top of github.com/BurntSushi/toml by design ruling:
// no viper anywhere in this codebase.
//
// Fail-loud contract:
//   - a missing settings file is not an error (defaults apply);
//   - malformed TOML fails with the file path and parser line info;
//   - any key present in the file but unknown to Settings fails with the
//     file path and the offending key path.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Environment variable names. ANICLI_DB_URL is consumed by Settings.DBPath;
// ANICLI_CONFIG and ANICLI_DATA feed ResolveConfigPath and DataDir.
const (
	EnvConfigPath       = "ANICLI_CONFIG"
	EnvDataDir          = "ANICLI_DATA"
	EnvProxyURL         = "ANICLI_PROXY_URL"
	EnvShikimoriSession = "ANICLI_SHIKIMORI_SESSION"
	EnvDBURL            = "ANICLI_DB_URL"
)

// Filesystem names and default values.
const (
	AppName          = "anicli"
	SettingsFileName = "settings.toml"
	DBFileName       = "anicli.db"
	// DefaultBind is loopback-only: the API is opt-in and must not be
	// reachable from the network unless the user says so.
	DefaultBind = "127.0.0.1:8765"
)

// General holds process-wide paths.
type General struct {
	// DataDir overrides the data directory; empty means auto-resolve via
	// DataDir() ($ANICLI_DATA > $XDG_DATA_HOME/anicli > ~/.local/share/anicli).
	DataDir string `toml:"data_dir"`
}

// Network configures the shared HTTP client.
// Timeout values are ported verbatim from the Python original
// (anicli/config.py: connect_timeout=10, read_timeout=30).
type Network struct {
	ConnectTimeout time.Duration `toml:"connect_timeout"`
	RequestTimeout time.Duration `toml:"request_timeout"`
	MaxParallel    int           `toml:"max_parallel"`
	UserAgent      string        `toml:"user_agent"`
	// ProxyURL is the global proxy (http/https/socks5); empty = direct.
	ProxyURL string `toml:"proxy_url"`
}

// Player configures the external video player.
type Player struct {
	Path    string `toml:"path"`
	Quality string `toml:"quality"`
}

// Shikimori configures tracker integration.
type Shikimori struct {
	Enabled     bool   `toml:"enabled"`
	Session     string `toml:"session"`
	AccessToken string `toml:"access_token"`
}

// Skip configures skip-time providers.
type Skip struct {
	// ProvidersOrder is the ordered fallback chain of skip providers.
	ProvidersOrder []string `toml:"providers_order"`
	// AnimeSkipEnabled toggles the anime_skip GraphQL provider.
	AnimeSkipEnabled bool `toml:"anime_skip_enabled"`
	// IntroSkipperEnabled toggles the local ffprobe/ffmpeg heuristics.
	IntroSkipperEnabled bool `toml:"intro_skipper_enabled"`
}

// Download configures the episode download manager.
type Download struct {
	// Dir is the download root; empty means DataDir()/downloads.
	Dir string `toml:"dir"`
	// MaxConcurrency bounds simultaneous downloads.
	MaxConcurrency int `toml:"max_concurrency"`
}

// API configures the optional HTTP API face.
type API struct {
	Enabled  bool          `toml:"enabled"`
	Bind     string        `toml:"bind"`
	TokenTTL time.Duration `toml:"token_ttl"`
}

// Settings is the full configuration surface.
type Settings struct {
	General   General   `toml:"general"`
	Network   Network   `toml:"network"`
	Player    Player    `toml:"player"`
	Shikimori Shikimori `toml:"shikimori"`
	Skip      Skip      `toml:"skip"`
	Download  Download  `toml:"download"`
	API       API       `toml:"api"`
}

// Default returns the built-in settings: user-tuned timeout values carried
// over verbatim from the Python original, loopback API bind, opt-in
// integrations.
func Default() Settings {
	return Settings{
		General: General{
			DataDir: "",
		},
		Network: Network{
			ConnectTimeout: 10 * time.Second,
			RequestTimeout: 30 * time.Second,
			MaxParallel:    4,
			UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
			ProxyURL: "",
		},
		Player: Player{
			Path:    "mpv",
			Quality: "1080",
		},
		Shikimori: Shikimori{
			Enabled:     false,
			Session:     "",
			AccessToken: "",
		},
		Skip: Skip{
			ProvidersOrder:      []string{"aniskip", "anime_skip", "intro_skipper"},
			AnimeSkipEnabled:    true,
			IntroSkipperEnabled: true,
		},
		Download: Download{
			Dir:            "",
			MaxConcurrency: 2,
		},
		API: API{
			Enabled:  false,
			Bind:     DefaultBind,
			TokenTTL: 15 * time.Minute,
		},
	}
}

// Load builds Settings from defaults, the TOML file at path (if it exists)
// and secret environment overrides, then validates the result.
// A missing path yields defaults without error.
func Load(path string) (*Settings, error) {
	s := Default()

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := applyFile(path, &s); err != nil {
				return nil, err
			}
		}
	}

	applyEnv(&s)

	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid settings: %w", err)
	}
	return &s, nil
}

// applyFile decodes the TOML file into the prefilled settings, then rejects
// any key the file carries that Settings does not know about.
func applyFile(path string, s *Settings) error {
	md, err := toml.DecodeFile(path, s)
	if err != nil {
		// toml.ParseError already renders parser line information.
		return fmt.Errorf("parse settings %s: %w", path, err)
	}
	if unknown := undecodedKeys(md); len(unknown) > 0 {
		return fmt.Errorf("settings %s: unknown setting %s (see settings.example.toml)",
			path, strings.Join(unknown, ", "))
	}
	return nil
}

// undecodedKeys renders every key present in the file but not consumed by
// the struct as a dotted path.
func undecodedKeys(md toml.MetaData) []string {
	keys := md.Undecoded()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.Join(k, "."))
	}
	return out
}

// applyEnv layers secret environment overrides on top of file values.
// Empty environment values are treated as unset.
func applyEnv(s *Settings) {
	if v, ok := lookupEnv(EnvProxyURL); ok {
		s.Network.ProxyURL = v
	}
	if v, ok := lookupEnv(EnvShikimoriSession); ok {
		s.Shikimori.Session = v
	}
}

// Validate checks values that are cheap to verify at startup and cheap to
// get wrong: the API bind address and the proxy URL.
func (s *Settings) Validate() error {
	if s.API.Bind != "" {
		if _, _, err := net.SplitHostPort(s.API.Bind); err != nil {
			return fmt.Errorf("api.bind %q: %w", s.API.Bind, err)
		}
	}
	if s.Network.ProxyURL != "" {
		u, err := url.Parse(s.Network.ProxyURL)
		if err != nil {
			return fmt.Errorf("network.proxy_url %q: %w", s.Network.ProxyURL, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
			// ok
		default:
			return fmt.Errorf("network.proxy_url %q: unsupported scheme %q (want http, https or socks5)",
				s.Network.ProxyURL, u.Scheme)
		}
	}
	return nil
}

// DBPath resolves the SQLite database location: $ANICLI_DB_URL wins,
// otherwise anicli.db under the effective data directory (created if
// missing).
func (s *Settings) DBPath() (string, error) {
	if v, ok := lookupEnv(EnvDBURL); ok {
		return v, nil
	}

	base := s.General.DataDir
	if base == "" {
		var err error
		base, err = DataDir()
		if err != nil {
			return "", fmt.Errorf("resolve data dir: %w", err)
		}
		// DataDir already created the directory.
		return filepath.Join(base, DBFileName), nil
	}

	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", fmt.Errorf("create data dir %q: %w", base, err)
	}
	return filepath.Join(base, DBFileName), nil
}

// lookupEnv reports the environment variable only when set to a non-empty
// value (XDG semantics: empty means unset).
func lookupEnv(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
