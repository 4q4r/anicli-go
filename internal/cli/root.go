// Package cli builds the anicli command tree.
//
// Everything here is factory-shaped (no package-level cobra command vars) so
// tests and embedders construct independent trees. cobra is imported only in
// this package by design ruling; the rest of internal/ stays CLI-agnostic.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/api"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/providers"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
	"github.com/an0nx/anicli-go/internal/tui"
)

// Build information, overridden at link time via -ldflags:
//
//	-X github.com/an0nx/anicli-go/internal/cli.Version=1.2.3
//	-X github.com/an0nx/anicli-go/internal/cli.Commit=abc1234
//	-X github.com/an0nx/anicli-go/internal/cli.Date=2026-09-12T00:00:00Z
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// ConfigFlagName is the persistent settings-file flag on the root command.
// Its value feeds config.ResolveConfigPath in the commands that need
// settings (wired from PR2 onward).
const ConfigFlagName = "config"

// NewRootCommand builds the anicli root command with all subcommands.
// Bare invocation runs the TUI.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "anicli",
		Short: "Anime search, play and download tool",
		Long: "anicli — anime search/play/download tool with a TUI and an " +
			"HTTP-API face sharing one core.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			return runTUI(cmd.Context(), cmd.OutOrStdout(), settingsPath)
		},
	}

	root.PersistentFlags().String(ConfigFlagName, "",
		"path to settings.toml (default: $ANICLI_CONFIG > $XDG_CONFIG_HOME/anicli/settings.toml > ~/.config/anicli/settings.toml)")

	root.AddCommand(
		newServeCommand(),
		newDoctorCommand(),
		newVersionCommand(),
		newCFCommand(),
		newShikimoriCommand(),
	)
	return root
}

// newServeCommand builds the HTTP API server command.
func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			return runServe(cmd.Context(), cmd.OutOrStdout(), settingsPath)
		},
	}
}

// newDoctorCommand builds the environment diagnostics command. The
// provider enumeration is live; the full check suite lands at G6.
func newDoctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local environment (providers, player, paths)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			return runDoctor(cmd.Context(), settingsPath, cmd.OutOrStdout())
		},
	}
}

// newVersionCommand builds the version command printing ldflags-injected
// build information.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return printVersion(cmd.OutOrStdout())
		},
	}
}

// runTUI is the default face: loads settings, wires the real service
// set and runs the bubbletea v2 application until quit; SIGINT/SIGTERM
// cancel the app context for a graceful exit. Providers disabled for
// missing configuration print a red startup notice before the
// alt-screen takes over (PR24).
func runTUI(ctx context.Context, out io.Writer, settingsPath string) error {
	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}

	// Startup notices render INSIDE the TUI (on the root screen), not
	// here — pre-alt-screen terminal output is invisible after the TUI
	// takes over.
	notices := startupNotices(*settings)

	dbPath, err := settings.DBPath()
	if err != nil {
		return fmt.Errorf("resolve db path: %w", err)
	}
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	real, err := tui.NewRealDeps(*settings, store,
		tui.WithShikiPersister(shikiTokenPersister(settingsPath)))
	if err != nil {
		return fmt.Errorf("build tui services: %w", err)
	}
	defer real.Close()

	// PR26: the first-run Shikimori setup gate — the TUI gets the
	// config snapshot and the persistence/verification/OAuth seams.
	wireShikiSetup(real.Deps, *settings, settingsPath)
	// PR27: the startup two-way list sync seam.
	wireStartupSync(real.Deps, settingsPath, real.ShikiNet, store.Progress)
	real.Deps.StartupNotices = notices

	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	return tui.Run(signalCtx, real.Deps, slog.Default())
}

// wireShikiSetup installs the PR26 setup seams onto the TUI deps: the
// [shikimori] snapshot, the settings writer (read-modify-write through
// config.UpdateShikimori), the candidate-section whoami probe and the
// OAuth loopback flow shared with `anicli shikimori auth`.
func wireShikiSetup(deps *tui.Deps, settings config.Settings, settingsPath string) {
	deps.ShikiCfg = settings.Shikimori

	// The section replaces the file's [shikimori] wholesale; the
	// screens compose it from the startup snapshot so unrelated
	// fields (client credentials) survive. UpdateShikimori layers no
	// environment overrides, so an env-provided session cookie can
	// never bake into the file.
	deps.SettingsWriter = func(section config.Shikimori) error {
		return config.UpdateShikimori(settingsPath, func(s *config.Shikimori) { *s = section })
	}

	deps.ShikiWhoAmI = func(ctx context.Context, section config.Shikimori) (tui.ShikiUser, error) {
		probe := settings
		probe.Shikimori = section
		client, err := newShikiOAuthClient(probe)
		if err != nil {
			return tui.ShikiUser{}, err
		}
		id, nickname, err := client.WhoAmI(ctx)
		if err != nil {
			return tui.ShikiUser{}, err
		}
		return tui.ShikiUser{ID: id, Nickname: nickname}, nil
	}

	deps.ShikiOAuth = func(clientID, clientSecret string, port int) (string, func(context.Context) (tui.ShikiOAuthResult, error), error) {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return "", nil, fmt.Errorf("shikimori oauth: локальный redirect-сервер: %w", err)
		}
		redirectURI := "http://" + ln.Addr().String() + "/callback"
		authURL := shikimori.AuthorizeURL(clientID, redirectURI)

		srv, codeCh, errCh := startShikiCallbackServer(ln)
		// Best-effort browser open: the TUI renders the URL too.
		_ = shikiOpenBrowser(authURL)

		resolve := func(ctx context.Context) (tui.ShikiOAuthResult, error) {
			defer func() { _ = srv.Close() }()
			var code string
			select {
			case code = <-codeCh:
			case err := <-errCh:
				return tui.ShikiOAuthResult{}, err
			case <-ctx.Done():
				return tui.ShikiOAuthResult{}, fmt.Errorf("shikimori oauth: ожидание кода авторизации прервано: %w", ctx.Err())
			}

			// A clean section for the exchange: no stale cookie or
			// token headers on the token endpoint.
			flowSettings := settings
			flowSettings.Shikimori = config.Shikimori{Enabled: true}
			client, err := newShikiOAuthClient(flowSettings)
			if err != nil {
				return tui.ShikiOAuthResult{}, err
			}
			set, err := client.ExchangeCode(ctx, clientID, clientSecret, redirectURI, code)
			if err != nil {
				return tui.ShikiOAuthResult{}, fmt.Errorf("shikimori oauth: обмен кода на токены: %w", err)
			}
			return tui.ShikiOAuthResult{
				AccessToken:  set.AccessToken,
				RefreshToken: set.RefreshToken,
				ExpiresAt:    set.ExpiresAt,
			}, nil
		}
		return authURL, resolve, nil
	}
}

// wireStartupSync installs the PR27 startup two-way list sync seam:
// the closure re-reads the settings file on every run, so credentials
// persisted by the first-run setup screens authorize the very first
// sync, and the OAuth token persister keeps refreshed tokens durable.
// The shikimori transport is shared with the rest of the TUI.
func wireStartupSync(deps *tui.Deps, settingsPath string, shikiNet *netclient.Client, progress *storage.ProgressRepo) {
	deps.SyncFull = func(ctx context.Context) (*shikimori.SyncResult, error) {
		fresh, err := config.Load(settingsPath)
		if err != nil {
			return nil, fmt.Errorf("startup sync: load settings: %w", err)
		}
		client := shikimori.New(fresh.Shikimori, shikiNet, nil,
			shikimori.WithTokenPersister(shikiTokenPersister(settingsPath)))
		return shikimori.NewSyncer(client, progress, nil).SyncFull(ctx)
	}
}

// runServe is the HTTP API face: loads settings, opens storage, builds
// the provider registry and shikimori client, and serves the API until
// SIGTERM/SIGINT with graceful drain.
func runServe(ctx context.Context, out io.Writer, settingsPath string) error {
	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}
	if !settings.API.Enabled {
		return fmt.Errorf("api server is disabled: set api.enabled = true in settings (bind %s)",
			settings.API.Bind)
	}

	dbPath, err := settings.DBPath()
	if err != nil {
		return fmt.Errorf("resolve db path: %w", err)
	}
	store, err := storage.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	reg, err := providers.NewRegistry(*settings, store.ProviderStats)
	if err != nil {
		return fmt.Errorf("build provider registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	shikiNet, err := netclient.New(settings.Network, netclient.WithProvider("shikimori"))
	if err != nil {
		return fmt.Errorf("build shikimori transport: %w", err)
	}
	var shiki api.ShikiClient = shikimori.New(settings.Shikimori, shikiNet, nil,
		shikimori.WithTokenPersister(shikiTokenPersister(settingsPath)))

	app, err := api.NewApp(api.Config{
		Settings: *settings,
		Store:    store,
		Registry: reg,
		Shiki:    shiki,
		ShikiNet: shikiNet,
	})
	if err != nil {
		return fmt.Errorf("build api app: %w", err)
	}
	defer app.Close()

	_, _ = fmt.Fprintf(out, "anicli serve: listening on %s\n", settings.API.Bind)

	signalCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	return app.ServeBind(signalCtx)
}

// runDoctor lives in doctor.go (PR24 search-based diagnostics).

// shikiTokenPersister builds the Shikimori OAuth persistence hook
// (PR25 E): a refreshed token pair is written back to the settings
// file read-modify-write (token fields only — the file-side session
// cookie and every other section stay untouched).
func shikiTokenPersister(settingsPath string) func(config.Shikimori) error {
	return func(section config.Shikimori) error {
		return config.UpdateShikimori(settingsPath, func(s *config.Shikimori) {
			s.AccessToken = section.AccessToken
			s.RefreshToken = section.RefreshToken
			s.TokenExpiresAt = section.TokenExpiresAt
		})
	}
}

// loadSettingsOrFail resolves the effective settings for a command,
// failing loudly on a broken settings file (config.Load already treats a
// missing file as defaults).
func loadSettingsOrFail(path string) (*config.Settings, error) {
	settings, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	return settings, nil
}

// printVersion renders the build triple.
func printVersion(out io.Writer) error {
	_, _ = fmt.Fprintf(out, "anicli %s (commit %s, built %s)\n", Version, Commit, Date)
	return nil
}

// ConfigPathFrom resolves the effective settings path for a parsed root
// command: --config flag value, else config.ResolveConfigPath.
func ConfigPathFrom(cmd *cobra.Command) (string, error) {
	explicit, err := cmd.Flags().GetString(ConfigFlagName)
	if err != nil {
		return "", fmt.Errorf("read --%s flag: %w", ConfigFlagName, err)
	}
	return config.ResolveConfigPath(explicit), nil
}
