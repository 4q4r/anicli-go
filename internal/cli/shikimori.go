package cli

// The `anicli shikimori` command group (PR25): the full OAuth2
// authorization-code flow against shikimori.io (`auth`) and the
// credential diagnostics (`status`), plus the shared status probe the
// doctor table renders.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// shikiAuthWaitTimeout bounds the whole interactive flow: five minutes
// from printing the URL to receiving the redirect.
const shikiAuthWaitTimeout = 5 * time.Minute

// shikiOAuthClient is the auth-flow surface: exchange a code, then
// resolve the user with the fresh token. *shikimori.Client satisfies
// it; tests inject fakes.
type shikiOAuthClient interface {
	ExchangeCode(ctx context.Context, clientID, clientSecret, redirectURI, code string) (*shikimori.TokenSet, error)
	GetUserID(ctx context.Context) (int64, error)
}

// newShikiOAuthClient builds the flow client against the real site
// (test seam: swapped for fakes).
var newShikiOAuthClient = func(settings config.Settings) (shikiOAuthClient, error) {
	net, err := netclient.New(settings.Network, netclient.WithProvider("shikimori"))
	if err != nil {
		return nil, fmt.Errorf("build shikimori transport: %w", err)
	}
	return shikimori.New(settings.Shikimori, net, nil), nil
}

// shikiOpenBrowser best-effort opens the authorization URL in the
// user's browser; failure is never fatal (the URL is printed too).
var shikiOpenBrowser = func(rawURL string) error {
	var bin string
	switch runtime.GOOS {
	case "darwin":
		bin = "open"
	case "windows":
		bin = "cmd" // /c start
	default:
		bin = "xdg-open"
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(bin, "/c", "start", "", rawURL) //nolint:gosec // fixed opener; URL built by shikimori.AuthorizeURL
	} else {
		cmd = exec.Command(bin, rawURL) //nolint:gosec // fixed xdg-open/open; URL built by shikimori.AuthorizeURL
	}
	return cmd.Start()
}

// newShikimoriCommand builds the `anicli shikimori` group.
func newShikimoriCommand() *cobra.Command {
	shiki := &cobra.Command{
		Use:   "shikimori",
		Short: "Интеграция с Shikimori: OAuth2-вход и статус",
		Long: "Управление учётными данными трекера Shikimori: полный OAuth2-поток " +
			"с автоматическим обновлением токенов (auth) и диагностика режима " +
			"аутентификации (status).",
	}
	shiki.AddCommand(newShikimoriAuthCommand(), newShikimoriStatusCommand())
	return shiki
}

// newShikimoriAuthCommand builds `anicli shikimori auth`.
func newShikimoriAuthCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "OAuth2-вход: браузер + локальный redirect + обмен кода на токены",
		Long: "Полный OAuth2 authorization-code flow (PR25): печатает ссылку " +
			"авторизации (и пытается открыть браузер), ждёт redirect на локальный " +
			"сервер 127.0.0.1, обменивает код на access/refresh токены (жизнь " +
			"access-токена — сутки, продление автоматическое) и сохраняет всё " +
			"в [shikimori] settings.toml. Приложение создайте на " +
			"https://shikimori.io/apps; redirect_uri вводите как " +
			"http://127.0.0.1:<порт>/callback (--port, по умолчанию случайный порт).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			clientID, _ := cmd.Flags().GetString("client-id")
			clientSecret, _ := cmd.Flags().GetString("client-secret")
			port, _ := cmd.Flags().GetInt("port")
			return runShikimoriAuth(cmd.Context(), cmd.OutOrStdout(), settingsPath, clientID, clientSecret, port)
		},
	}
	cmd.Flags().String("client-id", "", "client_id OAuth2-приложения (по умолчанию [shikimori] client_id)")
	cmd.Flags().String("client-secret", "", "client_secret OAuth2-приложения (по умолчанию [shikimori] client_secret)")
	cmd.Flags().Int("port", 0, "порт локального redirect-сервера (0 = случайный; должен совпадать с redirect_uri приложения)")
	return cmd
}

// newShikimoriStatusCommand builds `anicli shikimori status`.
func newShikimoriStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Режим аутентификации, пользователь и срок жизни токена",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			settingsPath, err := ConfigPathFrom(cmd)
			if err != nil {
				return err
			}
			return runShikimoriStatus(cmd.Context(), cmd.OutOrStdout(), settingsPath)
		},
	}
}

// startShikiCallbackServer serves the loopback redirect target on ln:
// /callback?code=… publishes the code, /callback?error=… publishes the
// failure, everything else gets the hint page. Channels are buffered so
// the handler never blocks on the consumer.
func startShikiCallbackServer(ln net.Listener) (*http.Server, <-chan string, <-chan error) {
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			select {
			case errCh <- fmt.Errorf("shikimori: %s (%s)", e, q.Get("error_description")):
			default:
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html><body><h3>Авторизация отклонена</h3>" +
				"<p>Вернитесь в терминал.</p></body></html>"))
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		select {
		case codeCh <- code:
		default:
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><h3>Готово</h3>" +
			"<p>Код получен — вернитесь в терминал.</p></body></html>"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><h3>anicli</h3>" +
			"<p>Ожидание redirect на /callback…</p></body></html>"))
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv, codeCh, errCh
}

// runShikimoriAuth performs the interactive flow (PR25 B): authorize
// URL + browser, loopback callback server, code exchange, token
// persistence, user info.
func runShikimoriAuth(ctx context.Context, out io.Writer, settingsPath, flagClientID, flagClientSecret string, port int) error {
	if settingsPath == "" {
		return fmt.Errorf("shikimori auth: не удалось определить путь settings.toml (задайте --config)")
	}
	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}

	clientID, clientSecret := flagClientID, flagClientSecret
	if clientID == "" {
		clientID = settings.Shikimori.ClientID
	}
	if clientSecret == "" {
		clientSecret = settings.Shikimori.ClientSecret
	}
	if clientID == "" || clientSecret == "" {
		return fmt.Errorf("shikimori auth: не заданы учётные данные приложения — " +
			"укажите --client-id/--client-secret или [shikimori] client_id/client_secret " +
			"(приложение: https://shikimori.io/apps)")
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("shikimori auth: локальный redirect-сервер: %w", err)
	}
	redirectURI := "http://" + ln.Addr().String() + "/callback"
	authURL := shikimori.AuthorizeURL(clientID, redirectURI)

	_, _ = fmt.Fprintln(out, "Авторизация Shikimori (OAuth2)")
	_, _ = fmt.Fprintln(out, "Откройте в браузере и разрешите доступ:")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, authURL)
	_, _ = fmt.Fprintln(out)

	srv, codeCh, errCh := startShikiCallbackServer(ln)
	defer func() { _ = srv.Close() }()
	if err := shikiOpenBrowser(authURL); err != nil {
		_, _ = fmt.Fprintln(out, "(браузер не открылся автоматически — откройте ссылку вручную)")
	}

	waitCtx, cancel := context.WithTimeout(ctx, shikiAuthWaitTimeout)
	defer cancel()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-waitCtx.Done():
		return fmt.Errorf("shikimori auth: ожидание кода авторизации прервано: %w", waitCtx.Err())
	}

	flowSettings := *settings
	flowSettings.Shikimori = config.Shikimori{Enabled: true}
	flowClient, err := newShikiOAuthClient(flowSettings)
	if err != nil {
		return err
	}
	set, err := flowClient.ExchangeCode(waitCtx, clientID, clientSecret, redirectURI, code)
	if err != nil {
		return fmt.Errorf("shikimori auth: обмен кода на токены: %w", err)
	}

	// Tokens first (they are the point of the flow), user info after.
	if err := config.UpdateShikimori(settingsPath, func(s *config.Shikimori) {
		s.Enabled = true
		s.AccessToken = set.AccessToken
		s.RefreshToken = set.RefreshToken
		s.TokenExpiresAt = set.ExpiresAt
		s.ClientID = clientID
		s.ClientSecret = clientSecret
	}); err != nil {
		return fmt.Errorf("shikimori auth: сохранить токены в %s: %w", settingsPath, err)
	}
	_, _ = fmt.Fprintf(out, "Токены сохранены в %s (access-токен живёт сутки, продление автоматическое)\n", settingsPath)

	tokenSettings := flowSettings
	tokenSettings.Shikimori.AccessToken = set.AccessToken
	userClient, err := newShikiOAuthClient(tokenSettings)
	if err != nil {
		return err
	}
	uid, uidErr := userClient.GetUserID(waitCtx)
	if uidErr != nil {
		_, _ = fmt.Fprintf(out, "Предупреждение: не удалось получить пользователя (whoami): %v\n", uidErr)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Пользователь Shikimori: id %d\n", uid)
	return nil
}

// shikiStatusReport is the settled Shikimori credential probe the
// status command and the doctor table render.
type shikiStatusReport struct {
	// Mode is the auth-mode diagnostic: disabled|none|cookie|bearer.
	Mode string
	// UserID/UserErr carry the live whoami answer (authed modes only).
	UserID  int64
	UserErr error
	// ExpiresAt is the OAuth access-token expiry (unix seconds;
	// 0 = unknown).
	ExpiresAt int64
	// HasClientID reports configured OAuth app credentials.
	HasClientID bool
}

// probeShikimoriStatus resolves the live report (test seam: swapped
// for fakes). The mode itself is config-only; whoami is one network
// round-trip bounded by budget.
var probeShikimoriStatus = func(ctx context.Context, cfg config.Settings, budget time.Duration) shikiStatusReport {
	if budget <= 0 {
		budget = 30 * time.Second
	}
	rep := shikiStatusReport{
		ExpiresAt:   cfg.Shikimori.TokenExpiresAt,
		HasClientID: cfg.Shikimori.ClientID != "" && cfg.Shikimori.ClientSecret != "",
	}

	// Mode without any network: a nil-transport client answers only
	// config diagnostics.
	rep.Mode = shikimori.New(cfg.Shikimori, nil, nil).Mode()

	if rep.Mode == "disabled" {
		return rep
	}
	if rep.Mode != "bearer" && rep.Mode != "cookie" {
		return rep // public mode: no user to resolve
	}

	pctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	net, err := netclient.New(cfg.Network, netclient.WithProvider("shikimori"))
	if err != nil {
		rep.UserErr = err
		return rep
	}
	client := shikimori.New(cfg.Shikimori, net, nil)
	rep.UserID, rep.UserErr = client.GetUserID(pctx)
	return rep
}

// runShikimoriStatus prints the credential diagnostics (PR25: mode,
// user, token expiry).
func runShikimoriStatus(ctx context.Context, out io.Writer, settingsPath string) error {
	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}
	rep := probeShikimoriStatus(ctx, *settings, settings.Network.SearchTimeout)

	_, _ = fmt.Fprintln(out, "Shikimori")
	switch rep.Mode {
	case "disabled":
		_, _ = fmt.Fprintln(out, "режим:        отключён (shikimori.enabled = false)")
	case "none":
		_, _ = fmt.Fprintln(out, "режим:        публичный (без учётных данных)")
		if rep.HasClientID {
			_, _ = fmt.Fprintln(out, "приложение:   client_id настроен — выполните: anicli shikimori auth")
		}
	case "cookie":
		_, _ = fmt.Fprintln(out, "режим:        cookie (_kawai_session)")
	default:
		_, _ = fmt.Fprintln(out, "режим:        bearer (OAuth2)")
	}
	if rep.Mode == "cookie" || rep.Mode == "bearer" {
		if rep.UserErr != nil {
			_, _ = fmt.Fprintf(out, "пользователь: ОШИБКА: %v\n", rep.UserErr)
		} else {
			_, _ = fmt.Fprintf(out, "пользователь: id %d\n", rep.UserID)
		}
	}
	if rep.Mode == "bearer" {
		if line := formatShikiExpiry(rep.ExpiresAt); line != "" {
			_, _ = fmt.Fprintf(out, "токен:        %s\n", line)
		} else {
			_, _ = fmt.Fprintln(out, "токен:        срок неизвестен (обновится по 401)")
		}
		if rep.HasClientID {
			_, _ = fmt.Fprintln(out, "обновление:   автоматическое (client_id/secret и refresh-токен настроены)")
		} else {
			_, _ = fmt.Fprintln(out, "обновление:   невозможно — нет client_id/secret (перезапустите anicli shikimori auth)")
		}
	}
	return nil
}

// formatShikiExpiry renders the access-token lifetime line; "" when
// the expiry is unknown.
func formatShikiExpiry(unix int64) string {
	if unix <= 0 {
		return ""
	}
	expiry := time.Unix(unix, 0)
	left := time.Until(expiry).Round(time.Minute)
	switch {
	case left <= 0:
		return fmt.Sprintf("истёк %s", expiry.Format("2006-01-02 15:04"))
	case left < time.Hour:
		return fmt.Sprintf("до %s (через %s)", expiry.Format("2006-01-02 15:04"), left)
	default:
		return fmt.Sprintf("до %s (через %s)", expiry.Format("2006-01-02 15:04"), left.Round(time.Hour))
	}
}

// shikiDoctorRow renders the doctor-table status cell and the failure
// flag for one report (PR25 F).
func shikiDoctorRow(rep shikiStatusReport) (status string, failed bool) {
	switch rep.Mode {
	case "disabled":
		return "ОТКЛЮЧЁН (shikimori.enabled = false)", false
	case "none":
		return "OK: публичный режим (без учётных данных)", false
	}
	if rep.UserErr != nil {
		return "ОШИБКА: " + rep.UserErr.Error(), true
	}
	line := "OK: режим " + rep.Mode + " · пользователь " + strconv.FormatInt(rep.UserID, 10)
	if rep.Mode == "bearer" {
		if line2 := formatShikiExpiry(rep.ExpiresAt); line2 != "" {
			line += " · токен " + line2
		}
	}
	return line, false
}
