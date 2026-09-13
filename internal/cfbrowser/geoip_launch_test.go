package cfbrowser

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestLaunchGeoipAlignmentWithProxy(t *testing.T) {
	geo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Europe/Moscow\nRU\n"))
	}))
	defer geo.Close()

	// Live forwarding proxy: the geo lookup must ride it.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, geo.URL, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	var got LaunchOptions
	store := NewClearanceStore(filepath.Join(t.TempDir(), "cfstore.json"), time.Minute)
	s := NewSolver(SolverConfig{
		ProxyURL:    proxy.URL,
		GeoEndpoint: geo.URL,
		Store:       store,
		Logger:      testLogger(t),
		DriverFactory: func(opts LaunchOptions) (Naviger, error) {
			got = opts
			return &fakeNav{reloadsToSolve: 0, userAgent: "UA"}, nil
		},
	})
	if _, err := s.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if got.Timezone != "Europe/Moscow" {
		t.Errorf("timezone = %q, want Europe/Moscow", got.Timezone)
	}
	if got.Locale != "ru-RU" {
		t.Errorf("locale = %q, want ru-RU", got.Locale)
	}
	if got.ProxyURL != proxy.URL {
		t.Errorf("proxy = %q", got.ProxyURL)
	}
}

func TestLaunchDirectKeepsSystemDefaults(t *testing.T) {
	var got LaunchOptions
	store := NewClearanceStore(filepath.Join(t.TempDir(), "cfstore.json"), time.Minute)
	s := NewSolver(SolverConfig{
		Store:  store,
		Logger: testLogger(t),
		DriverFactory: func(opts LaunchOptions) (Naviger, error) {
			got = opts
			return &fakeNav{reloadsToSolve: 0, userAgent: "UA"}, nil
		},
	})
	if _, err := s.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err != nil {
		t.Fatalf("solve: %v", err)
	}
	if got.Timezone != "" || got.Locale != "" {
		t.Errorf("direct solve must keep system defaults, got tz=%q locale=%q", got.Timezone, got.Locale)
	}
}

func TestLaunchGeoFailureSlogOnly(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	var got LaunchOptions
	store := NewClearanceStore(filepath.Join(t.TempDir(), "cfstore.json"), time.Minute)
	s := NewSolver(SolverConfig{
		ProxyURL:    "http://127.0.0.1:9",
		GeoEndpoint: deadURL,
		Store:       store,
		Logger:      testLogger(t),
		DriverFactory: func(opts LaunchOptions) (Naviger, error) {
			got = opts
			return &fakeNav{reloadsToSolve: 0, userAgent: "UA"}, nil
		},
	})
	if _, err := s.SolveChallenge(context.Background(), "https://animego.one/", time.Second); err != nil {
		t.Fatalf("geo failure must not fail the solve: %v", err)
	}
	if got.Timezone != "" || got.Locale != "" {
		t.Errorf("failed geo must leave system defaults, got %+v", got)
	}
}
