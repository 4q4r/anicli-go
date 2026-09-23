package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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
		newCFStatusCommand(),
		newCFSolveCommand(),
		newCFClearCommand(),
		newCFLoginCommand(),
		newCFLogoutCommand(),
	)
	return cf
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

// runCFStatus prints the binary, license tier/plan/expiry and update
// bookkeeping. The [cf] channel scopes what "the binary" means: the
// free channel resolves the free line even under a valid key.
func runCFStatus(out io.Writer) error {
	settings, err := loadSettingsOrFail(config.ResolveConfigPath(""))
	if err != nil {
		return fmt.Errorf("cf status: %w", err)
	}
	channel := settings.CF.Channel

	cacheDir, err := cfbrowser.ResolveCacheDir("")
	if err != nil {
		return fmt.Errorf("cf status: %w", err)
	}
	bin, binErr := cfbrowser.ResolveCurrentBinary(cfbrowser.ResolveOptions{Channel: channel, NoProbe: true})
	_, _ = fmt.Fprintf(out, "кэш:            %s\n", cacheDir)
	if binErr != nil {
		_, _ = fmt.Fprintf(out, "бинарник:       не установлен — %s\n", cfbrowser.InstallHint)
	} else {
		printCFBinary(out, bin)
	}
	if vs := cfbrowser.VerdictSummaryLine(cacheDir); vs != "" {
		_, _ = fmt.Fprintf(out, "вердикты:       %s\n", vs)
	}

	tier, plan, expires, note := cfbrowser.StatusLicenseReport(context.Background(),
		cfbrowser.LicenseOptions{Logger: slog.Default()})
	line := "лицензия:       " + tier
	if tier == "pro" {
		line += fmt.Sprintf(" (план %s, до %s)", orDash(plan), orDash(expires))
	}
	_, _ = fmt.Fprintf(out, "%s\n", line)
	// Tier display mirrors what actually launches: a pro license over
	// a free-only cache shows the gap explicitly instead of letting a
	// "pro" license line imply a pro binary. The free channel opted
	// out of pro on purpose — no gap hint there. Under auto/pro the
	// hint stays truthful for both sub-states (no pro dir, or the
	// newest pro being compat-blocked): install retries the pull.
	if tier == "pro" && channel != cfbrowser.ChannelFree && binErr == nil && bin != nil && bin.Channel == cfbrowser.ChannelFree {
		_, _ = fmt.Fprintf(out, "                pro-бинарник не установлен или новейший pro не прошёл проверку запуска — повтор попытки pro при следующем запуске\n")
	}
	if note != "" {
		_, _ = fmt.Fprintf(out, "                %s\n", note)
	}

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
	// PR80: CF is always on — no enabled override needed.

	// PR85: the CLI face runs pre-alt-screen — the explicit default
	// handler is allowed here (the TUI wires the file logger instead).
	mgr, err := cfbrowser.NewManager(*settings, cfbrowser.WithManagerLogger(slog.Default()))
	if err != nil {
		return fmt.Errorf("cf solve: %w", err)
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
		"anilibria", "animevost", "anilib", "animego",
		"gogoanime", "animepahe", "dreamcast", "sameband", "kodik", "allanime",
		"anidub", "anizone",
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

// newCFLoginCommand builds `anicli cf login [ключ]`.
func newCFLoginCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "login [ключ]",
		Short: "Сохранить лицензионный ключ (канал Pro)",
		Long: "Проверяет лицензионный ключ CloakBrowser через API и при успехе " +
			"сохраняет его в ~/.cloakbrowser/license.key — бинарники дальше " +
			"скачиваются и обновляются по каналу Pro. Без аргумента печатает " +
			"инструкцию по получению ключа.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			if len(args) == 0 {
				printCFLoginInstructions(cmd.OutOrStdout())
				return nil
			}
			return runCFLogin(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
}

// printCFLoginInstructions renders the no-argument help text.
func printCFLoginInstructions(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Лицензионный ключ не указан.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Получите бесплатный ключ на https://cloakbrowser.dev/free")
	_, _ = fmt.Fprintln(out, "и выполните: anicli cf login <ключ>")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Ключ сохраняется в ~/.cloakbrowser/license.key; переменная окружения")
	_, _ = fmt.Fprintln(out, "CLOAKBROWSER_LICENSE_KEY имеет приоритет над файлом.")
}

// runCFLogin validates and saves the license key.
func runCFLogin(ctx context.Context, out io.Writer, key string) error {
	st, err := cfbrowser.Login(ctx, key, cfbrowser.LicenseOptions{Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("cf login: %w", err)
	}
	cacheDir, err := cfbrowser.ResolveCacheDir("")
	if err != nil {
		return fmt.Errorf("cf login: %w", err)
	}
	_, _ = fmt.Fprintf(out, "лицензия действительна: план %s, действует до %s\n",
		orDash(st.Plan), orDash(st.Expires))
	_, _ = fmt.Fprintf(out, "ключ сохранён: %s\n", filepath.Join(cacheDir, "license.key"))
	_, _ = fmt.Fprintln(out, "бинарники будут качаться по каналу pro, пока [cf] channel = \"auto\" (по умолчанию); \"free\" ключ игнорирует")
	return nil
}

// newCFLogoutCommand builds `anicli cf logout`.
func newCFLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Удалить сохранённый лицензионный ключ",
		Long: "Удаляет ~/.cloakbrowser/license.key и кэш проверки лицензии — " +
			"загрузки возвращаются на канал free.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if err := cfbrowser.Logout(cfbrowser.LicenseOptions{Logger: slog.Default()}); err != nil {
				return fmt.Errorf("cf logout: %w", err)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "лицензионный ключ и кэш проверки удалены — канал free")
			return nil
		},
	}
}
