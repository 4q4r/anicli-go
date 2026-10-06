package providers

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

func TestAllRosterComplete(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 30 {
		t.Fatalf("All() = %d providers, want 30", len(bare))
	}

	wantIDs := []string{
		"anilibria", "animevost", "anilib", "animego",
		"gogoanime", "kickassanime", "anizone",
		"sameband", "kodik",
		"anidub", "animedia", "shiza", "yummy", "hdrezka", "anistar",
		"anifilm", "animemobi", "anitokyo",
		"animiku", "anikado", "animevib",
		"animeheaven", "anikoto", "anipub",
		"anilibria-torrent", "animetosho", "tokyotosho", "rutor", "anirena", "subsplease",
	}
	seen := map[string]bool{}
	for _, p := range bare {
		if seen[p.ID()] {
			t.Errorf("duplicate provider ID %q", p.ID())
		}
		seen[p.ID()] = true
	}
	for _, id := range wantIDs {
		if !seen[id] {
			t.Errorf("All() missing provider %q", id)
		}
	}
}

// TestProviderSettingsForFlattensConfig pins the PR140 flatten table:
// every per-provider string setting config.Settings carries today,
// exposed under its providers.<id>.<key> name — and nothing else (a
// provider without a settings section reads nil in its scripts).
func TestProviderSettingsForFlattensConfig(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Providers.Kodik.Token = "tok"
	cfg.Providers.HDRezka.BaseURL = "https://rezka.example"

	kodik := providerSettingsFor(cfg)("kodik")
	if len(kodik) != 1 || kodik["token"] != "tok" {
		t.Fatalf("providerSettingsFor(kodik) = %v, want {token: tok}", kodik)
	}
	rezka := providerSettingsFor(cfg)("hdrezka")
	if len(rezka) != 1 || rezka["base_url"] != "https://rezka.example" {
		t.Fatalf("providerSettingsFor(hdrezka) = %v, want {base_url: https://rezka.example}", rezka)
	}
	if got := providerSettingsFor(cfg)("animego"); got != nil {
		t.Fatalf("providerSettingsFor(animego) = %v, want nil (no settings section)", got)
	}
}

// TestFactoryProviderSettingsReachScripts pins the PR140 config-read
// seam end to end: cfg.Providers.Kodik.Token flattens through the
// factory's settingsFor, rides LoadSources into the sandbox and
// surfaces as anicli.provider_setting("token") inside a script.
func TestFactoryProviderSettingsReachScripts(t *testing.T) {
	dir := luaXDG(t)
	// A user kodik script whose search surfaces the token it reads
	// (user-dir sources take LoadSources precedence over the bundled
	// embeds, so this shadows whatever serves the id).
	sub := filepath.Join(dir, "kodik")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	src := `return {
	id = "kodik",
	search = function(query)
		return { { title = tostring(anicli.provider_setting("token")), url = "/u" } }
	end,
	episodes = function(anime_url) return {} end,
	streams = function(episode_url, dub) return { dub_name = dub, links = {} } end,
}`
	if err := os.WriteFile(filepath.Join(sub, "main.lua"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "from-config"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		if p.ID() != "kodik" {
			continue
		}
		results, err := p.Search(t.Context(), "q")
		if err != nil {
			t.Fatalf("kodik Search: %v", err)
		}
		if len(results) != 1 || results[0].Title != "from-config" {
			t.Fatalf("script saw token %v, want from-config", results)
		}
		return
	}
	t.Fatal("All() missing the kodik provider")
}

// TestAllKodikSlotIsLuaPinned pins the PR140 migration shape: the
// kodik roster slot keeps its place, name and base with the bundled
// Lua script serving the id. (There is no compiled constructor to
// type-assert against anymore — the Lua-only proof is the
// [providers.lua] disabled leg in disabled_test.go, plus the token
// flow pinned by TestFactoryProviderSettingsReachScripts and the
// search form pin in kodik_test.go.)
func TestAllKodikSlotIsLuaPinned(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "from-config"

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		if p.ID() != "kodik" {
			continue
		}
		if got := p.Name(); got != "Kodik" {
			t.Errorf("kodik Name = %q, want the script's declaration", got)
		}
		if got := p.BaseURL(); got != KodikAPIBase {
			t.Errorf("kodik BaseURL = %q, want %q", got, KodikAPIBase)
		}
		return
	}
	t.Fatal("All() missing the kodik provider")
}

func TestNewRegistryWrapsEveryProvider(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	list := reg.List()
	if len(list) != 30 {
		t.Fatalf("List() = %d providers, want 30", len(list))
	}
	// Registration order follows All() (stable render/fan-out order).
	// Wave-2 integration (fix/60) seated the five parallel providers
	// next to their peers: kickassanime (PR58) and anizone (PR59) in
	// the latin block; animedia (PR56) and shiza
	// (PR57) in the RU-dub block; yummy (PR68) and hdrezka (PR69)
	// closing it. fix/93 froze the 6-provider wave into grouped
	// blocks; anifilm (PR91) and animemobi (PR92) closed the RU-dub
	// block after anistar; anitokyo (PR100) joined after animemobi;
	// animiku (PR101) joined after anitokyo; anikado (PR102) joined
	// after animiku; animevib (PR103) joined after anikado;
	// animeheaven (PR105, the animeheaven.me EN sub-only direct-MP4
	// catalog) joined after animevib; anikoto (PR104) joined after
	// animeheaven; anipub (PR107, the anipub.xyz EN catalog) joined
	// after anikoto; the torrent providers close the roster (rutor, PR87, then anirena,
	// PR88, then subsplease, PR89).
	wantOrder := []string{
		"anilibria", "animevost", "anilib", "animego",
		"gogoanime", "kickassanime", "anizone",
		"sameband", "kodik",
		"anidub", "animedia", "shiza", "yummy", "hdrezka", "anistar",
		"anifilm", "animemobi", "anitokyo",
		"animiku", "anikado", "animevib",
		"animeheaven", "anikoto", "anipub",
		"anilibria-torrent", "animetosho", "tokyotosho", "rutor", "anirena", "subsplease",
	}
	for i, p := range list {
		if p.ID() != wantOrder[i] {
			t.Errorf("list[%d] = %s, want %s", i, p.ID(), wantOrder[i])
		}
	}

	// Every entry must be wrapped in the search-stat delegator: a failing
	// search returns a wrapped error even when the provider forgets to.
	got, ok := reg.Get("anilibria")
	if !ok {
		t.Fatal("Get(anilibria) not found")
	}
	if _, ok := got.(SearchDelegator); !ok {
		t.Fatalf("registry entry %T is not a SearchDelegator", got)
	}
}

func TestNewRegistryRecordsSearchStatsWiring(t *testing.T) {
	// No-network proof of the wiring: SearchDelegator + ProviderStatRepo
	// recording is behavior-tested in registry_test.go against a stub;
	// this test pins that NewRegistry produces exactly that composition.
	// (A registry search here would hit production URLs, which tests must
	// never do.)
	st, err := storage.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	for _, p := range reg.List() {
		if _, ok := p.(SearchDelegator); !ok {
			t.Fatalf("registry entry %s (%T) is not a SearchDelegator", p.ID(), p)
		}
	}
}

func TestNewRegistryPropagatesClientError(t *testing.T) {
	t.Parallel()

	// An invalid proxy URL makes per-provider client construction fail;
	// NewRegistry must surface it instead of dropping providers.
	cfg := config.Default()
	cfg.Network.ProxyURL = "://not-a-url"

	if _, err := NewRegistry(cfg, nil); err == nil {
		t.Fatal("NewRegistry with invalid proxy must fail")
	}
}

// TestAllProvidersSourceTypeBoth pins the corrected SourceType
// semantics (PR23): SourceType describes content SUITABILITY for the
// user's wanted audio languages (EN/JA/RU), and every current roster
// member serves wanted-language audio with acceptable video — so the
// whole roster is BOTH. VIDEO is for unwanted/absent audio (future
// providers), AUDIO for catalog-wide hardsubs/unwatchable video.
func TestAllProvidersSourceTypeBoth(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Kodik.Token = "test-token" // keep kodik in the roster (PR24)

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 30 {
		t.Fatalf("All() = %d providers, want 30", len(bare))
	}
	for _, p := range bare {
		if got := p.SourceType(); got != contracts.SourceTypeBoth {
			t.Errorf("provider %s SourceType() = %q, want %q", p.ID(), got, contracts.SourceTypeBoth)
		}
	}
}

// TestContentLanguageRoster pins each provider's declared primary
// content language (PR23): Russian-dub sites tag their dubs "ru",
// Japanese-audio sites with English subtitles "ja". The language is a
// service-level property — every dub a provider emits carries it, no
// per-dub introspection.
func TestContentLanguageRoster(t *testing.T) {
	t.Parallel()

	want := map[string]string{
		"anilibria":         "ru",
		"animevost":         "ru",
		"anilib":            "ru",
		"animego":           "ru",
		"gogoanime":         "ja",
		"kickassanime":      "ja",
		"anizone":           "ja",
		"sameband":          "ru",
		"kodik":             "ru",
		"anidub":            "ru",
		"animedia":          "ru",
		"shiza":             "ru",
		"yummy":             "ru",
		"hdrezka":           "ru",
		"anistar":           "ru",
		"anifilm":           "ru",
		"animemobi":         "ru",
		"anitokyo":          "ru",
		"animiku":           "ru",
		"anikado":           "ru",
		"animevib":          "ru",
		"animeheaven":       "ja",
		"anikoto":           "en",
		"anipub":            "en",
		"anilibria-torrent": "ru",
		"animetosho":        "ja",
		"tokyotosho":        "ja",
		"rutor":             "ru", // RU-dub torrent catalog (PR87)
		"anirena":           "ja",
		"subsplease":        "ja",
	}

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	// kodik stays tokenless here per this test's history.

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range bare {
		lc, ok := p.(interface{ ContentLanguage() string })
		if !ok {
			t.Errorf("provider %s (%T) does not expose ContentLanguage", p.ID(), p)
			continue
		}
		if got := lc.ContentLanguage(); got != want[p.ID()] {
			t.Errorf("provider %s ContentLanguage() = %q, want %q", p.ID(), got, want[p.ID()])
		}
	}
}

// TestAllSkipsExcludedProviders pins [providers].exclude (PR23):
// excluded ids never get a client or a registry slot, the rest of the
// roster order is untouched.
func TestAllSkipsExcludedProviders(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Exclude = []string{"gogoanime", "kodik"}

	bare, err := All(cfg)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(bare) != 28 {
		t.Fatalf("All() = %d providers, want 28", len(bare))
	}
	for _, p := range bare {
		if p.ID() == "gogoanime" || p.ID() == "kodik" {
			t.Errorf("excluded provider %s must not be built", p.ID())
		}
	}
}

// TestNewRegistryStreamFilterWiring pins the exclude_streams wiring
// (PR23): with patterns configured, every registry entry keeps its
// SearchDelegator shell (roster pin) over a dub stream filter, and
// the provider-level language lookup still sees through both layers.
func TestNewRegistryStreamFilterWiring(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.ExcludeStreams = []string{"трейлер"}

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, p := range reg.List() {
		del, ok := p.(SearchDelegator)
		if !ok {
			t.Fatalf("registry entry %s (%T) is not a SearchDelegator", p.ID(), p)
		}
		if _, ok := del.Provider.(dubFilteredProvider); !ok {
			t.Fatalf("registry entry %s: SearchDelegator wraps %T, want dubFilteredProvider",
				p.ID(), del.Provider)
		}
	}
	if got := reg.ContentLanguage("animego"); got != "ru" {
		t.Errorf("ContentLanguage(animego) through the filter wrap = %q, want ru", got)
	}
}

// TestNewRegistryWithoutStreamFilterKeepsBareComposition guards the
// zero-config path: no exclude_streams means no filter layer — the
// registry composition is exactly the pre-PR23 SearchDelegator shape.
func TestNewRegistryWithoutStreamFilterKeepsBareComposition(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""

	reg, err := NewRegistry(cfg, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	del, ok := reg.Get("anilibria")
	if !ok {
		t.Fatal("Get(anilibria) not found")
	}
	sd, ok := del.(SearchDelegator)
	if !ok {
		t.Fatal("registry entry is not a SearchDelegator")
	}
	if _, isFiltered := sd.Provider.(dubFilteredProvider); isFiltered {
		t.Fatal("no exclude_streams configured: SearchDelegator must wrap the bare provider")
	}
}

// TestNewRegistryInvalidStreamRegexFailsLoud: NewRegistry receives
// Settings directly (config.Load's Validate does not run in between),
// so the filter compile must fail loud here too.
func TestNewRegistryInvalidStreamRegexFailsLoud(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.ExcludeStreams = []string{"([unclosed"}

	if _, err := NewRegistry(cfg, nil); err == nil {
		t.Fatal("NewRegistry with an invalid exclude_streams regex must fail")
	}
}

// TestNewRegistryLogsExcludedProviders pins the startup log line so
// silent exclusion can never regress. Not parallel: swaps the default
// slog handler for the capture and restores it.
func TestNewRegistryLogsExcludedProviders(t *testing.T) {
	cfg := config.Default()
	cfg.Network.ProxyURL = ""
	cfg.Providers.Exclude = []string{"gogoanime"}

	var buf lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(previous)

	if _, err := NewRegistry(cfg, nil); err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "provider excluded: gogoanime") {
		t.Fatalf("startup log must name the excluded provider, got:\n%s", got)
	}
}

// lockedBuffer is a mutex-guarded buffer: slog handlers write from
// whatever goroutine logs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
