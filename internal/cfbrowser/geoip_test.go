package cfbrowser

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveGeoDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/line/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("Europe/Moscow\nRU\n"))
	}))
	defer srv.Close()

	info, err := resolveGeo(context.Background(), "", srv.URL+"/line/?fields=timezone,countryCode", nil)
	if err != nil {
		t.Fatalf("resolveGeo: %v", err)
	}
	if info.Timezone != "Europe/Moscow" || info.CountryCode != "RU" {
		t.Errorf("info = %+v", info)
	}
}

func TestResolveGeoViaProxy(t *testing.T) {
	// Upstream geo service.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Asia/Tokyo\nJP\n"))
	}))
	defer upstream.Close()

	// Proxy: asserts the request is routed through it.
	proxied := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- struct{}{}
		if r.Method != http.MethodGet || r.Host != upstream.Listener.Addr().String() {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		// Minimal absolute-URI GET forwarding.
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
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

	info, err := resolveGeo(context.Background(), proxy.URL, upstream.URL+"/line/", nil)
	if err != nil {
		t.Fatalf("resolveGeo via proxy: %v", err)
	}
	select {
	case <-proxied:
	default:
		t.Fatal("request must be routed through the proxy")
	}
	if info.Timezone != "Asia/Tokyo" || info.CountryCode != "JP" {
		t.Errorf("info = %+v", info)
	}
}

func TestResolveGeoFailureIsTyped(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()
	if _, err := resolveGeo(context.Background(), "", deadURL+"/line/", &http.Client{Timeout: time.Second}); err == nil {
		t.Fatal("expected error from dead endpoint")
	}
	// Garbage payload: not an error worth failing the solve — returns
	// zero info without error only when the endpoint answers with
	// parseable lines; unparseable content must NOT silently pass as
	// valid (the caller logs and falls back to system defaults).
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>hi</html>"))
	}))
	defer garbage.Close()
	info, err := resolveGeo(context.Background(), "", garbage.URL, nil)
	if err != nil {
		t.Fatalf("garbage endpoint: %v", err)
	}
	if info.Timezone != "" || info.CountryCode != "" {
		t.Errorf("garbage must yield zero info, got %+v", info)
	}
}

func TestLocaleForCountry(t *testing.T) {
	cases := map[string]string{
		"RU": "ru-RU",
		"JP": "ja-JP",
		"US": "en-US",
		"DE": "de-DE",
		"XX": "en-US", // unknown → default
		"":   "en-US",
	}
	for cc, want := range cases {
		if got := LocaleForCountry(cc); got != want {
			t.Errorf("LocaleForCountry(%q) = %q, want %q", cc, got, want)
		}
	}
}
