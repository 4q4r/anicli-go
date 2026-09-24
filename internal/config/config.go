// Package config loads anicli settings from TOML with layered overrides:
// defaults first, then the settings file, then ANICLI_* environment variables
// for secrets only (proxy URL, shikimori session cookie, database URL,
// kodik API token).
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
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	// EnvAPISecret overrides the API auth signing secret.
	EnvAPISecret = "ANICLI_API_AUTH_SECRET" //nolint:gosec // variable name, not a secret
	// EnvKodikToken carries the NAME of the kodik token environment
	// variable, not a credential value.
	EnvKodikToken = "ANICLI_KODIK_TOKEN" //nolint:gosec // variable name, not a secret
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
	// SearchTimeout bounds ONE provider's whole participation in a
	// search fan-out or doctor check (all its query variants) — the
	// PR24 slow-provider ceiling. Default 30s.
	SearchTimeout time.Duration `toml:"search_timeout"`
	MaxParallel   int           `toml:"max_parallel"`
	UserAgent     string        `toml:"user_agent"`
	// ProxyURL is the global proxy (http/https/socks5); empty = direct.
	ProxyURL string `toml:"proxy_url"`
}

// Player configures the external video player.
type Player struct {
	Path    string `toml:"path"`
	Quality string `toml:"quality"`
}

// Shikimori configures tracker integration. Two credential paths:
// the _kawai_session cookie (session) or OAuth2 (access_token plus the
// refresh material); the token wins when both are configured.
type Shikimori struct {
	Enabled     bool   `toml:"enabled"`
	Session     string `toml:"session"`
	AccessToken string `toml:"access_token"`
	// RefreshToken is the OAuth2 refresh token (`anicli shikimori auth`).
	RefreshToken string `toml:"refresh_token"`
	// TokenExpiresAt is the unix timestamp when AccessToken expires
	// (Shikimori access tokens live one day; refreshed automatically
	// within the last five minutes).
	TokenExpiresAt int64 `toml:"token_expires_at"`
	// ClientID and ClientSecret are the OAuth2 application credentials
	// (shikimori.io/apps); needed for token refresh.
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
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
	// RefreshTokenTTL bounds the rotating refresh token session
	// (python api.refresh_token_ttl_seconds).
	RefreshTokenTTL time.Duration `toml:"refresh_token_ttl"`
	// AuthSecret is the HMAC key signing access tokens (python
	// api.auth_secret_key). Secret: also settable via
	// ANICLI_API_AUTH_SECRET (env wins over the file).
	AuthSecret string `toml:"auth_secret_key"`
}

// Web holds the API user registry (python [web.users.<login>]
// password_hash ruling; legacy api.auth_users removed).
type Web struct {
	// Users maps login -> credential entry for token auth login.
	Users map[string]WebUser `toml:"users"`
}

// WebUser is one API login entry.
type WebUser struct {
	// PasswordHash is a pbkdf2_sha256$<iterations>$<salt_b64url>$<digest_b64url>
	// hash, verified with constant-time comparison at login.
	PasswordHash string `toml:"password_hash"`
}

// Providers holds provider-set settings: search exclusions plus the
// per-provider credentials (only providers that need user-supplied
// credentials appear here; the rest run credential-free).
type Providers struct {
	// Exclude drops providers from the search fan-out by id (e.g.
	// ["animepahe", "kodik"]). Default: nothing excluded.
	Exclude []string `toml:"exclude"`
	// ExcludeStreams drops dub streams whose name matches any of
	// these regular expressions (e.g. ["трейлер", "реклама"] discards
	// trash streams). Patterns are validated (fail-loud) at load.
	ExcludeStreams []string `toml:"exclude_streams"`
	// Kodik configures the Kodik API source.
	Kodik ProvidersKodik `toml:"kodik"`
	// HDRezka configures the hdrezka source route (PR72).
	HDRezka ProvidersHDRezka `toml:"hdrezka"`
}

// ProvidersHDRezka carries the hdrezka mirror route override (PR72).
// The rezka mirror family geo-fences per domain: from datacenter
// exits hdrezka-home.tv (and its canonicalized twins hdrezka.ag /
// rezka.ag) withhold the stream links (success:true, url:false; the
// site's own session JWT attests geo:"de"), while rezka-ua.tv serves
// full stream lists from the same exit [LIVE-VERIFIED 2026-09-19].
// The built-in default follows the serving mirror; when the family
// rotates again, base_url re-points the provider without a rebuild.
type ProvidersHDRezka struct {
	// BaseURL overrides the provider's site root (e.g.
	// "https://rezka-ua.tv"); empty means the built-in default.
	BaseURL string `toml:"base_url"`
}

// Torrent configures the BitTorrent subsystem (PR35): realtime
// streaming playback over a local HTTP server, no external programs
// (pure-Go client). The engine stays idle until a torrent provider
// adds a link — enabling it never starts network machinery on app
// boot by itself. The PR40 removal of the [torrent] links ingestion
// list took the only direct-config consumer with it: links now enter
// the engine exclusively through the torrent search providers.
type Torrent struct {
	// Enabled turns the torrent subsystem on. Default true by design
	// ruling (commissioned feature, Shikimori-enabled precedent):
	// combined with the lazy engine this is inert until a torrent
	// provider resolves a release — nothing boots on its own — and
	// no_upload=false only means ethical seeding after playback (flip
	// no_upload for leech-only).
	Enabled bool `toml:"enabled"`
	// Dir is the torrent data directory; empty means
	// DataDir()/torrents (created on demand).
	Dir string `toml:"dir"`
	// Port is the BitTorrent listen port; 0 picks an ephemeral port.
	Port int `toml:"port"`
	// NoUpload turns off seeding (leech-only).
	NoUpload bool `toml:"no_upload"`
	// ReadaheadMB is the streaming readahead window for playback.
	ReadaheadMB int `toml:"readahead_mb"`
	// Proxy routes tracker/webseed/metadata HTTP(S) traffic through
	// an http/https/socks5 proxy (same scheme set as
	// network.proxy_url). Library limitation, documented honestly:
	// BitTorrent PEER traffic (TCP/uTP data exchange) and udp://
	// tracker announces stay DIRECT — anacrolix v1.61 only threads
	// the HTTP layer through the proxy.
	Proxy string `toml:"proxy"`
	// Trackers are user-specified announce URLs (udp://, http://,
	// https://, ws://, wss://) added to every torrent for faster peer
	// discovery. The engine health-checks them concurrently and keeps
	// the responsive ones (recheck on demand).
	Trackers []string `toml:"trackers"`
	// TrackerLists are URLs of plain-text tracker lists (PR41): one
	// announce URL per line, '#' comments, e.g. the ngosang/trackerslist
	// feeds. Fetched ONCE per engine start through the shared netclient
	// (network.proxy_url applies, not [torrent] proxy), parsed, deduped
	// against [torrent] trackers and merged into the same health-checked
	// pool. A failed list URL degrades (static trackers keep working);
	// the per-URL outcome is visible via the engine's tracker-list
	// statuses and the log.
	TrackerLists []string `toml:"tracker_lists"`
}

// CF configures the always-on Cloudflare bypass (CloakBrowser stealth
// Chromium + clearance ladder). PR80 owner ruling: the enabled knob is
// removed — the machinery is integral and non-optional; the stealth
// browser self-installs at startup when missing. Headless-only by
// design ruling: the solve browser always runs headless (no window,
// no display dependency).
type CF struct {
	// SolveTimeout bounds one challenge solve.
	SolveTimeout time.Duration `toml:"solve_timeout"`
	// BrowserIdleTimeout is how long an idle browser session survives
	// after the last solve finishes before it is torn down (default
	// 15s; "0s" closes the browser immediately — sessions are
	// ephemeral so the ~300-600 MB RSS stays resident only while
	// actually solving).
	BrowserIdleTimeout time.Duration `toml:"browser_idle_timeout"`
	// AutoUpdate keeps the cached stealth Chromium current
	// (network-gated, never blocks solves; $CLOAKBROWSER_AUTO_UPDATE
	// can force it off).
	AutoUpdate bool `toml:"auto_update"`
	// UpdateInterval is the auto-update retry ticker cadence.
	UpdateInterval time.Duration `toml:"update_interval"`
	// Proxy is the proxy for cfbrowser DOWNLOAD/UPDATE traffic only —
	// free GitHub fetches, pro version/download calls, license checks
	// and update checks (PR80). It never touches the stealth browser's
	// own page traffic and never touches netclient/provider traffic
	// (that is network.proxy_url). Empty = direct. Schemes: http,
	// https, socks5, socks5h — validated at load.
	Proxy string `toml:"proxy"`
	// Channel selects the stealth-Chromium line (PR73):
	//
	//   - "auto" (default): free is the base — public GitHub free
	//     releases, no pro traffic without a key; a valid license key
	//     (anicli cf login) upgrades installs and updates to the pro
	//     line, best-effort and only while the pro build stays
	//     compatible with the bundled chromedp driver;
	//   - "free": the pro channel is never touched, even with a key;
	//   - "pro": always the license-keyed pro line (requires a valid
	//     key; pre-PR73 behavior).
	//
	// Anything else fails validation at startup.
	Channel string `toml:"channel"`
}

// ProvidersKodik carries the Kodik API token (https://kodik-api.com
// answers 401 without one). Empty by default: the kodik provider fails
// loud on use, never at startup, so the credential is only demanded from
// users who actually select that source.
type ProvidersKodik struct {
	// Token is the Kodik API token; also settable via ANICLI_KODIK_TOKEN
	// (env wins over the file).
	Token string `toml:"token"`
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
	Web       Web       `toml:"web"`
	Providers Providers `toml:"providers"`
	Torrent   Torrent   `toml:"torrent"`
	CF        CF        `toml:"cf"`
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
			SearchTimeout:  30 * time.Second,
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
			Enabled:        true,
			Session:        "",
			AccessToken:    "",
			RefreshToken:   "",
			TokenExpiresAt: 0,
			ClientID:       "",
			ClientSecret:   "",
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
			Enabled:         false,
			Bind:            DefaultBind,
			TokenTTL:        15 * time.Minute,
			RefreshTokenTTL: 30 * 24 * time.Hour,
			AuthSecret:      "",
		},
		Web: Web{Users: map[string]WebUser{}},
		Torrent: Torrent{
			Enabled:     true,
			Dir:         "",
			Port:        42069,
			NoUpload:    false,
			ReadaheadMB: 32,
		},
		CF: CF{
			SolveTimeout:       90 * time.Second,
			BrowserIdleTimeout: 15 * time.Second,
			AutoUpdate:         true,
			UpdateInterval:     30 * time.Minute,
			Channel:            "auto",
			Proxy:              "", // download/update traffic only; empty = direct
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
		} else if !errors.Is(err, os.ErrNotExist) {
			// Exists but is not stat-able (e.g. permission denied on a
			// parent directory): must not masquerade as "missing file".
			return nil, fmt.Errorf("stat settings %s: %w", path, err)
		}
		// Missing file: defaults apply, not an error.
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
	unknown := undecodedKeys(md)
	// PR80: [cf] enabled was removed (always-on ruling) — a legacy file
	// carrying it gets the TARGETED migration message naming the exact
	// fix, not the generic unknown-key error.
	for _, key := range unknown {
		if key == "cf.enabled" {
			return fmt.Errorf("settings %s: [cf] enabled удалён — CF теперь всегда включён; удалите эту строку из settings.toml", path)
		}
	}
	if len(unknown) > 0 {
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
	if v, ok := lookupEnv(EnvKodikToken); ok {
		s.Providers.Kodik.Token = v
	}
	if v, ok := lookupEnv(EnvAPISecret); ok {
		s.API.AuthSecret = v
	}
}

// Validate checks values that are cheap to verify at startup and cheap to
// get wrong: the API bind address, the proxy URL and the API auth
// secret (an enabled API without a signing key would 500 on every
// guarded request — better to fail at startup).
func (s *Settings) Validate() error {
	if s.API.Bind != "" {
		if _, _, err := net.SplitHostPort(s.API.Bind); err != nil {
			return fmt.Errorf("api.bind %q: %w", s.API.Bind, err)
		}
	}
	if s.API.Enabled && strings.TrimSpace(s.API.AuthSecret) == "" {
		return fmt.Errorf("api.enabled requires api.auth_secret_key (or %s)", EnvAPISecret)
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
	if s.CF.Proxy != "" {
		// PR80: the download/update proxy (download traffic only —
		// never the stealth browser's page traffic, never provider
		// traffic). Scheme gate mirrors network.proxy_url plus socks5h.
		u, err := url.Parse(s.CF.Proxy)
		if err != nil {
			return fmt.Errorf("cf.proxy %q: %w", s.CF.Proxy, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
			// ok
		default:
			return fmt.Errorf("cf.proxy %q: unsupported scheme %q (want http, https, socks5 or socks5h)",
				s.CF.Proxy, u.Scheme)
		}
	}
	// Invalid exclude_streams regexes fail here, at startup, instead
	// of being silently skipped when the filter compiles them.
	for _, pattern := range s.Providers.ExcludeStreams {
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("providers.exclude_streams %q: %w", pattern, err)
		}
	}
	// The torrent listen port must be a valid TCP port (0 = ephemeral);
	// a nonsense port would surface as an obscure bind error deep in
	// the engine instead of at startup.
	if s.Torrent.Port < 0 || s.Torrent.Port > 65535 {
		return fmt.Errorf("torrent.port %d: out of range (0-65535)", s.Torrent.Port)
	}
	if s.Torrent.ReadaheadMB < 0 {
		return fmt.Errorf("torrent.readahead_mb %d: must not be negative", s.Torrent.ReadaheadMB)
	}
	if s.Torrent.Proxy != "" {
		u, err := url.Parse(s.Torrent.Proxy)
		if err != nil {
			return fmt.Errorf("torrent.proxy %q: %w", s.Torrent.Proxy, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5":
			// ok
		default:
			return fmt.Errorf("torrent.proxy %q: unsupported scheme %q (want http, https or socks5)",
				s.Torrent.Proxy, u.Scheme)
		}
	}
	for _, tr := range s.Torrent.Trackers {
		u, err := url.Parse(tr)
		if err != nil {
			return fmt.Errorf("torrent.trackers %q: %w", tr, err)
		}
		switch u.Scheme {
		case "udp", "http", "https", "ws", "wss":
			if u.Host == "" {
				return fmt.Errorf("torrent.trackers %q: missing host", tr)
			}
		default:
			return fmt.Errorf("torrent.trackers %q: unsupported scheme %q (want udp, http, https, ws or wss)",
				tr, u.Scheme)
		}
	}
	// Tracker-list feeds ride the shared netclient (http/https only —
	// github is foreign, network.proxy_url is the route); a bad list
	// URL must fail at startup, not as a fetch error at first add.
	for _, listURL := range s.Torrent.TrackerLists {
		u, err := url.Parse(listURL)
		if err != nil {
			return fmt.Errorf("torrent.tracker_lists %q: %w", listURL, err)
		}
		switch u.Scheme {
		case "http", "https":
			if u.Host == "" {
				return fmt.Errorf("torrent.tracker_lists %q: missing host", listURL)
			}
		default:
			return fmt.Errorf("torrent.tracker_lists %q: unsupported scheme %q (want http or https)",
				listURL, u.Scheme)
		}
	}
	// The stealth-Chromium channel: a typo ("frea") must fail at
	// startup, not silently resolve as some other line mid-run.
	switch s.CF.Channel {
	case "", "auto", "free", "pro":
		// "" only arises for programmatically built Settings — it
		// selects auto downstream.
	default:
		return fmt.Errorf("cf.channel %q: unknown channel (want auto, free or pro)", s.CF.Channel)
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
