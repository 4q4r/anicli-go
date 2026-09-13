package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/cfbrowser"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/providers"
)

// newCFCommand builds the `anicli cf` group: stealth-Chromium
// lifecycle and manual challenge solving for the CF bypass.
func newCFCommand() *cobra.Command {
	cf := &cobra.Command{
		Use:   "cf",
		Short: "Обход Cloudflare (стелс-браузер CloakBrowser)",
		Long: "Управление встроенным обходом Cloudflare: установка и обновление стелс-" +
			"Chromium, статус кэша и лицензии, ручное решение challenge для источника, " +
			"очистка сохранённых clearance-куки.",
	}
	cf.AddCommand(
		newCFInstallCommand(),
		newCFStatusCommand(),
		newCFSolveCommand(),
		newCFClearCommand(),
	)
	return cf
}

// newCFInstallCommand builds `anicli cf install`.
func newCFInstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Скачать стелс-Chromium в кэш (проверка SHA-256)",
		Long: "Скачивает свежий CloakBrowser Chromium линии free с GitHub Releases " +
			"(~/.cloakbrowser, переиспользует уже установленные версии без повторной " +
			"загрузки), проверяет SHA-256 из метаданных релиза и распаковывает архив.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCFInstall(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// runCFInstall resolves-or-downloads the stealth binary with progress
// logged to stderr.
func runCFInstall(ctx context.Context, out io.Writer) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	info, err := cfbrowser.Install(ctx, cfbrowser.InstallOptions{Logger: logger})
	if err != nil {
		return fmt.Errorf("cf install: %w", err)
	}
	printCFBinary(out, info)
	return nil
}

// newCFStatusCommand builds `anicli cf status`.
func newCFStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Статус: бинарник, тираж лицензии, ожидающее обновление",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runCFStatus(cmd.OutOrStdout())
		},
	}
}

// runCFStatus prints the binary, license tier and update bookkeeping.
func runCFStatus(out io.Writer) error {
	cacheDir, err := cfbrowser.ResolveCacheDir("")
	if err != nil {
		return fmt.Errorf("cf status: %w", err)
	}
	bin, binErr := cfbrowser.ResolveCurrentBinary(cfbrowser.ResolveOptions{})
	_, _ = fmt.Fprintf(out, "кэш:            %s\n", cacheDir)
	if binErr != nil {
		_, _ = fmt.Fprintf(out, "бинарник:       не установлен — выполните: %s\n", cfbrowser.InstallHint)
	} else {
		printCFBinary(out, bin)
	}
	_, _ = fmt.Fprintf(out, "лицензия:       %s\n", cfbrowser.LicenseTier(cacheDir))

	if st, ok := cfbrowser.ReadUpdateStatus(cacheDir); ok {
		switch {
		case st.Deferred:
			_, _ = fmt.Fprintf(out, "обновление:     ожидает сеть (установлено: %s)\n", orDash(st.InstalledVersion))
		case st.UpdatedTo != "":
			_, _ = fmt.Fprintf(out, "обновление:     %s установлено %s\n", st.UpdatedTo, st.CheckedAt.Format(time.RFC3339))
		case st.LatestVersion != "":
			_, _ = fmt.Fprintf(out, "обновление:     актуально (%s), проверено %s\n",
				st.LatestVersion, st.CheckedAt.Format(time.RFC3339))
		default:
			_, _ = fmt.Fprintf(out, "обновление:     проверено %s\n", st.CheckedAt.Format(time.RFC3339))
		}
		if st.LastError != "" {
			_, _ = fmt.Fprintf(out, "посл. ошибка:   %s\n", st.LastError)
		}
	} else {
		_, _ = fmt.Fprintln(out, "обновление:     ещё не проверялось")
	}
	return nil
}

// printCFBinary renders one resolved binary line set.
func printCFBinary(out io.Writer, bin *cfbrowser.BinaryInfo) {
	_, _ = fmt.Fprintf(out, "бинарник:       %s\n", bin.Path)
	_, _ = fmt.Fprintf(out, "версия:         %s (канал %s)\n", bin.Version, bin.Channel)
}

// orDash renders empty strings as a dash.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// newCFSolveCommand builds `anicli cf solve <provider-id>`.
func newCFSolveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "solve <provider-id>",
		Short: "Решить challenge источника (headless-браузер)",
		Long: "Открывает стелс-браузер в headless-режиме на базовом URL источника, " +
			"ждёт прохождения Cloudflare challenge (для интерактивного Turnstile " +
			"выполняется автоматический клик — best-effort), сохраняет clearance-куки " +
			"и User-Agent в хранилище, после чего браузер закрывается.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			return runCFSolve(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

// runCFSolve solves the provider's base URL and stores the clearance.
func runCFSolve(ctx context.Context, out io.Writer, providerID string) error {
	baseURL, ok := providers.BaseURLFor(providerID)
	if !ok {
		known := make([]string, 0, len(providerBaseURLList()))
		known = append(known, providerBaseURLList()...)
		return fmt.Errorf("неизвестный источник %q (доступны: %s)", providerID, strings.Join(known, ", "))
	}

	settings, err := loadSettingsOrFail(config.ResolveConfigPath(""))
	if err != nil {
		return err
	}
	// Explicit command: the ladder is wanted even when [cf] is off.
	settings.CF.Enabled = true

	mgr, err := cfbrowser.NewManager(*settings)
	if err != nil {
		return fmt.Errorf("cf solve: %w", err)
	}
	if mgr == nil {
		// Unreachable with Enabled=true; kept for safety.
		return fmt.Errorf("cf solve: обход не сконфигурирован")
	}
	defer func() { _ = mgr.Close() }()

	_, _ = fmt.Fprintf(out, "решаю challenge: %s (%s)…\n", providerID, baseURL)
	clearance, err := mgr.Solver.SolveChallenge(ctx, baseURL, settings.CF.SolveTimeout)
	if err != nil {
		return fmt.Errorf("cf solve: %w", err)
	}
	_, _ = fmt.Fprintf(out, "готово: cf_clearance=%t, куки=%d, User-Agent=%s\n",
		clearance.HasCFClearance(), len(clearance.Cookies), clearance.UserAgent)
	return nil
}

// providerBaseURLList renders the known provider IDs in stable order.
func providerBaseURLList() []string {
	return []string{
		"anilibria", "animevost", "anilib", "animego", "sovetromantica",
		"gogoanime", "animepahe", "dreamcast", "sameband", "kodik", "allanime",
	}
}

// newCFClearCommand builds `anicli cf clear`.
func newCFClearCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Очистить сохранённые clearance-куки",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settings, err := loadSettingsOrFail(config.ResolveConfigPath(""))
			if err != nil {
				return err
			}
			base := settings.General.DataDir
			if base == "" {
				base, err = config.DataDir()
				if err != nil {
					return fmt.Errorf("cf clear: %w", err)
				}
			}
			store := cfbrowser.NewClearanceStore(filepath.Join(base, "cfstore.json"), 0)
			if err := store.Clear(); err != nil {
				return fmt.Errorf("cf clear: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "clearance-куки очищены")
			return nil
		},
	}
}
