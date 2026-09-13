package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server runtimes: production hardening timeouts and the shutdown
// grace period (gunicorn_conf.py analogues: keepalive 5s, timeout 60s,
// graceful_timeout 30s — the Go face runs a single process, so the
// values map onto the net/http server knobs).
const (
	serverReadHeaderTimeout = 10 * time.Second
	serverReadTimeout       = 60 * time.Second
	serverWriteTimeout      = 60 * time.Second
	serverIdleTimeout       = 120 * time.Second
	serverShutdownGrace     = 30 * time.Second
)

// NewServer builds the production http.Server for the App router.
func (a *App) NewServer() *http.Server {
	return &http.Server{
		Addr:              a.cfg.API.Bind,
		Handler:           a.Router(),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

// Serve runs the server until ctx is cancelled, then drains in-flight
// requests within the shutdown grace period. The listener is returned
// closed and the bound address is logged before serving starts.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := a.NewServer()

	errCh := make(chan error, 1)
	go func() {
		a.log.Info("api server listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("api server shutdown: %w", err)
	}
	return <-errCh
}

// ServeBind binds cfg.API.Bind and runs Serve on the bound listener.
func (a *App) ServeBind(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.cfg.API.Bind)
	if err != nil {
		return fmt.Errorf("api bind %s: %w", a.cfg.API.Bind, err)
	}
	return a.Serve(ctx, ln)
}

// logDiscard returns a logger writing nowhere (test convenience).
func logDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
