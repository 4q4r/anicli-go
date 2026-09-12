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
	"strings"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/providers"
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
			return runTUI(cmd.Context(), cmd.OutOrStdout())
		},
	}

	root.PersistentFlags().String(ConfigFlagName, "",
		"path to settings.toml (default: $ANICLI_CONFIG > $XDG_CONFIG_HOME/anicli/settings.toml > ~/.config/anicli/settings.toml)")

	root.AddCommand(
		newServeCommand(),
		newDoctorCommand(),
		newVersionCommand(),
	)
	return root
}

// newServeCommand builds the HTTP API server command (stub until G5).
func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runServe(cmd.Context(), cmd.OutOrStdout())
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

// runTUI is the default face (bubbletea v2 app, planned for G5).
func runTUI(_ context.Context, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "anicli tui: not implemented yet")
	return nil
}

// runServe is the HTTP API face (chi server, planned for G5).
func runServe(_ context.Context, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "anicli serve: not implemented yet")
	return nil
}

// runDoctor prints environment diagnostics. For now it enumerates the
// registered providers: registry construction only builds clients, no
// network egress happens. Real health checks land at G6.
func runDoctor(_ context.Context, settingsPath string, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "anicli doctor")

	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}

	reg, err := providers.NewRegistry(*settings, nil)
	if err != nil {
		return fmt.Errorf("build provider registry: %w", err)
	}

	ids := make([]string, 0, len(reg.List()))
	for _, p := range reg.List() {
		ids = append(ids, p.ID())
	}
	_, _ = fmt.Fprintf(out, "providers (%d): %s\n", len(ids), strings.Join(ids, ", "))
	_, _ = fmt.Fprintln(out, "health checks: not implemented yet")
	return nil
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
