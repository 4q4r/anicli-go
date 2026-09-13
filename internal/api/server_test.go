package api

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNewServerTimeouts(t *testing.T) {
	app := newTestApp(t)
	app.cfg.API.Bind = "127.0.0.1:0"

	srv := app.NewServer()
	if srv.ReadHeaderTimeout != serverReadHeaderTimeout || srv.ReadTimeout == 0 ||
		srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
		t.Fatalf("server timeouts not set: %+v", srv)
	}
	if srv.Handler == nil {
		t.Fatal("handler must be the app router")
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	app := newTestApp(t)
	app.log = logDiscard()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx, ln) }()

	// Probe the health route on the live server.
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/api/v1/health") //nolint:gosec // test probe
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("health probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Trace-Id") == "" {
		t.Fatal("trace header missing on live response")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within the grace period")
	}
}
