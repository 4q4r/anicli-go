package cli

// The `anicli mal` command group (PR112): the full OAuth2
// authorization-code flow with PKCE against myanimelist.net (`auth`)
// and the credential diagnostics (`status`), plus the TUI setup seams
// shared with the provider menu.

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/mal"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/tui"
)

// malAuthWaitTimeout bounds the whole interactive flow: five minutes
// from printing the URL to receiving the redirect.
const malAuthWaitTimeout = 5 * time.Minute

// listenLoopback opens the loopback redirect listener on port (0 =
// a random free port).
func listenLoopback(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// malAuthClient is the auth-flow surface: exchange a code (answering
// the plain PKCE challenge), then resolve the user with the fresh
// token. *mal.Client satisfies it; tests inject fakes.
type malAuthClient interface {
	ExchangeCode(ctx context.Context, redirectURI, code, verifier string) (*mal.TokenSet, error)
	WhoAmI(ctx context.Context) (int64, string, error)
}

// newMALAuthClient builds the flow client against the real site (test
// seam: swapped for fakes).
var newMALAuthClient = func(settings config.Settings) (malAuthClient, error) {
	net, err := netclient.New(settings.Network, netclient.WithProvider("mal"))
	if err != nil {
		return nil, fmt.Errorf("build mal transport: %w", err)
	}
	return mal.New(settings.MAL, mal.APIBaseURL, mal.OAuthBaseURL, net), nil
}

// newMALCommand builds the `anicli mal` group.
func newMALCommand() *cobra.Command {
	malCmd := &cobra.Command{
		Use:   "mal",
		Short: "Интеграция с MyAnimeList: OAuth2-вход (PKCE) и статус",
		Long: "Управление учётными данными трекера MyAnimeList: полный " +
			"OAuth2 authorization-code flow с PKCE и автоматическим " +
			"обновлением токенов (auth) и диагностика режима " +
			"аутентификации (status). Shikimori и MyAnimeList могут быть " +
			"авторизованы одновременно.",
	}
	malCmd.AddCommand(newMALAuthCommand(), newMALStatusCommand())
	return malCmd
}

// newMALAuthCommand builds `anicli mal auth`.
func newMALAuthCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "OAuth2-вход с PKCE: браузер + локальный redirect + обмен кода на токены",
		Long: "Полный OAuth2 authorization-code flow с PKCE (PR112): печатает " +
			"ссылку авторизации (и пытается открыть браузер), ждёт redirect на " +
			"локальный сервер 127.0.0.1, обменивает код на access/refresh токены " +
			"(продление автоматическое) и сохраняет всё в [mal] settings.toml. " +
			"Приложение создайте на https://myanimelist.net/en/apiproxy " +
			"(redirect_uri вида http://127.0.0.1:<порт>/callback; --port, по " +
			"умолчанию случайный порт).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			clientID, _ := cmd.Flags().GetString("client-id")
			clientSecret, _ := cmd.Flags().GetString("client-secret")
			port, _ := cmd.Flags().GetInt("port")
			return runMALAuth(cmd.Context(), cmd.OutOrStdout(), settingsPath, clientID, clientSecret, port)
		},
	}
	cmd.Flags().String("client-id", "", "client_id OAuth2-приложения (по умолчанию [mal] client_id)")
	cmd.Flags().String("client-secret", "", "client_secret OAuth2-приложения (по умолчанию [mal] client_secret)")
	cmd.Flags().Int("port", 0, "порт локального redirect-сервера (0 = случайный; должен совпадать с redirect_uri приложения)")
	return cmd
}

// newMALStatusCommand builds `anicli mal status`.
func newMALStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Режим аутентификации и срок жизни токена",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			return runMALStatus(cmd.Context(), cmd.OutOrStdout(), settingsPath)
		},
	}
}

// runMALAuth performs the interactive PKCE flow: authorize URL +
// browser, loopback callback server, code exchange (the verifier
// answers the plain challenge), token persistence, user info.
func runMALAuth(ctx context.Context, out io.Writer, settingsPath, flagClientID, flagClientSecret string, port int) error {
	if settingsPath == "" {
		return fmt.Errorf("mal auth: не удалось определить путь settings.toml (задайте --config)")
	}
	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}

	clientID, clientSecret := flagClientID, flagClientSecret
	if clientID == "" {
		clientID = settings.MAL.ClientID
	}
	if clientSecret == "" {
		clientSecret = settings.MAL.ClientSecret
	}
	if clientID == "" {
		return fmt.Errorf("mal auth: не заданы учётные данные приложения — " +
			"укажите --client-id/--client-secret или [mal] client_id/client_secret " +
			"(приложение: https://myanimelist.net/en/apiproxy)")
	}

	ln, err := listenLoopback(port)
	if err != nil {
		return fmt.Errorf("mal auth: локальный redirect-сервер: %w", err)
	}
	redirectURI := "http://" + ln.Addr().String() + "/callback"
	// PKCE: a fresh verifier per flow; the plain challenge IS the
	// verifier (the only method MAL supports).
	verifier, err := mal.NewCodeVerifier()
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("mal auth: %w", err)
	}
	state, err := mal.NewOAuthState()
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("mal auth: %w", err)
	}
	authURL := mal.AuthorizeURL(clientID, redirectURI, state, verifier)

	_, _ = fmt.Fprintln(out, "Авторизация MyAnimeList (OAuth2 + PKCE)")
	_, _ = fmt.Fprintln(out, "Откройте в браузере и разрешите доступ:")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, authURL)
	_, _ = fmt.Fprintln(out)

	srv, codeCh, errCh := startShikiCallbackServer(ln, state)
	defer func() { _ = srv.Close() }()
	if err := shikiOpenBrowser(authURL); err != nil {
		_, _ = fmt.Fprintln(out, "(браузер не открылся автоматически — откройте ссылку вручную)")
	}

	waitCtx, cancel := context.WithTimeout(ctx, malAuthWaitTimeout)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-waitCtx.Done():
		return fmt.Errorf("mal auth: ожидание кода авторизации прервано: %w", waitCtx.Err())
	}

	flowSettings := *settings
	flowSettings.MAL = config.MAL{Enabled: true, ClientID: clientID, ClientSecret: clientSecret}
	flowClient, err := newMALAuthClient(flowSettings)
	if err != nil {
		return err
	}
	set, err := flowClient.ExchangeCode(waitCtx, redirectURI, code, verifier)
	if err != nil {
		return fmt.Errorf("mal auth: обмен кода на токены: %w", err)
	}

	// Tokens first (they are the point of the flow), user info after.
	if err := config.UpdateMAL(settingsPath, func(s *config.MAL) {
		s.Enabled = true
		s.AccessToken = set.AccessToken
		s.RefreshToken = set.RefreshToken
		s.TokenExpiresAt = set.ExpiresAt
		s.ClientID = clientID
		s.ClientSecret = clientSecret
	}); err != nil {
		return fmt.Errorf("mal auth: сохранить токены в %s: %w", settingsPath, err)
	}
	_, _ = fmt.Fprintf(out, "Токены сохранены в %s (продление автоматическое)\n", settingsPath)

	tokenSettings := flowSettings
	tokenSettings.MAL.AccessToken = set.AccessToken
	userClient, err := newMALAuthClient(tokenSettings)
	if err != nil {
		return err
	}
	uid, name, uidErr := userClient.WhoAmI(waitCtx)
	if uidErr != nil {
		_, _ = fmt.Fprintf(out, "Предупреждение: не удалось получить пользователя (whoami): %v\n", uidErr)
		return nil
	}
	if name != "" {
		_, _ = fmt.Fprintf(out, "Пользователь MyAnimeList: %s (id %d)\n", name, uid)
	} else {
		_, _ = fmt.Fprintf(out, "Пользователь MyAnimeList: id %d\n", uid)
	}
	return nil
}

// runMALStatus prints the MAL credential diagnostics (PR112: mode,
// token expiry).
func runMALStatus(ctx context.Context, out io.Writer, settingsPath string) error {
	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintln(out, "MyAnimeList")
	// Mode without any network: a nil-transport client answers only
	// config diagnostics.
	mode := mal.New(settings.MAL, mal.APIBaseURL, mal.OAuthBaseURL, nil).Mode()
	switch mode {
	case "disabled":
		_, _ = fmt.Fprintln(out, "режим:        отключён (mal.enabled = false)")
	case "none":
		_, _ = fmt.Fprintln(out, "режим:        публичный (без учётных данных)")
		if settings.MAL.ClientID != "" {
			_, _ = fmt.Fprintln(out, "приложение:   client_id настроен — выполните: anicli mal auth")
		}
	default:
		_, _ = fmt.Fprintln(out, "режим:        bearer (OAuth2)")
	}
	if mode == "bearer" {
		if line := formatShikiExpiry(settings.MAL.TokenExpiresAt); line != "" {
			_, _ = fmt.Fprintf(out, "токен:        %s\n", line)
		} else {
			_, _ = fmt.Fprintln(out, "токен:        срок неизвестен (обновится по 401)")
		}
	}
	return nil
}

// wireMALSetup installs the PR112 setup seams onto the TUI deps: the
// [mal] snapshot, the settings writer (read-modify-write through
// config.UpdateMAL), the candidate-section whoami probe and the PKCE
// OAuth loopback flow shared with `anicli mal auth`.
func wireMALSetup(deps *tui.Deps, settings config.Settings, settingsPath string) {
	deps.MALCfg = settings.MAL

	deps.MALSettingsWriter = func(section config.MAL) error {
		return config.UpdateMAL(settingsPath, func(s *config.MAL) { *s = section })
	}

	deps.MALWhoAmI = func(ctx context.Context, section config.MAL) (tui.MALUser, error) {
		probe := settings
		probe.MAL = section
		client, err := newMALAuthClient(probe)
		if err != nil {
			return tui.MALUser{}, err
		}
		id, name, err := client.WhoAmI(ctx)
		if err != nil {
			return tui.MALUser{}, err
		}
		return tui.MALUser{ID: id, Name: name}, nil
	}

	deps.MALOAuth = func(clientID, clientSecret string, port int) (string, func(context.Context) (tui.MALOAuthResult, error), error) {
		ln, err := listenLoopback(port)
		if err != nil {
			return "", nil, fmt.Errorf("mal oauth: локальный redirect-сервер: %w", err)
		}
		redirectURI := "http://" + ln.Addr().String() + "/callback"
		verifier, err := mal.NewCodeVerifier()
		if err != nil {
			_ = ln.Close()
			return "", nil, fmt.Errorf("mal oauth: %w", err)
		}
		state, err := mal.NewOAuthState()
		if err != nil {
			_ = ln.Close()
			return "", nil, fmt.Errorf("mal oauth: %w", err)
		}
		authURL := mal.AuthorizeURL(clientID, redirectURI, state, verifier)

		srv, codeCh, errCh := startShikiCallbackServer(ln, state)
		// Best-effort browser open: the TUI renders the URL too.
		_ = shikiOpenBrowser(authURL)

		resolve := func(ctx context.Context) (tui.MALOAuthResult, error) {
			defer func() { _ = srv.Close() }()
			var code string
			select {
			case code = <-codeCh:
			case err := <-errCh:
				return tui.MALOAuthResult{}, err
			case <-ctx.Done():
				return tui.MALOAuthResult{}, fmt.Errorf("mal oauth: ожидание кода авторизации прервано: %w", ctx.Err())
			}

			// A clean section for the exchange: only the app
			// credentials ride the token endpoint.
			flowSettings := settings
			flowSettings.MAL = config.MAL{Enabled: true, ClientID: clientID, ClientSecret: clientSecret}
			client, err := newMALAuthClient(flowSettings)
			if err != nil {
				return tui.MALOAuthResult{}, err
			}
			set, err := client.ExchangeCode(ctx, redirectURI, code, verifier)
			if err != nil {
				return tui.MALOAuthResult{}, fmt.Errorf("mal oauth: обмен кода на токены: %w", err)
			}
			return tui.MALOAuthResult{
				AccessToken:  set.AccessToken,
				RefreshToken: set.RefreshToken,
				ExpiresAt:    set.ExpiresAt,
			}, nil
		}
		return authURL, resolve, nil
	}
}
