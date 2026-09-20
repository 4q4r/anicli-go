package cfbrowser

import (
	"net/http"
	"testing"
	"time"
)

// DownloadHTTPClient is the [cf] proxy transport for download/update
// traffic (PR80): empty = direct (regression), http/https ride the
// proxy URL, socks5/socks5h dial through x/net/proxy, unknown schemes
// fail loud.
func TestDownloadHTTPClientDirectRegression(t *testing.T) {
	hc, err := DownloadHTTPClient("", 30*time.Second)
	if err != nil {
		t.Fatalf("direct client: %v", err)
	}
	if hc.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", hc.Timeout)
	}
	if hc.Transport != nil {
		t.Errorf("direct transport = %+v, want nil (net/http default dialing)", hc.Transport)
	}
}

func TestDownloadHTTPClientHTTPProxy(t *testing.T) {
	hc, err := DownloadHTTPClient("http://127.0.0.1:10809", time.Minute)
	if err != nil {
		t.Fatalf("http proxy client: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("transport = %+v, want an http.Transport with a Proxy func", hc.Transport)
	}
	u, err := tr.Proxy(nil)
	if err != nil {
		t.Fatalf("proxy func: %v", err)
	}
	if u == nil || u.String() != "http://127.0.0.1:10809" {
		t.Errorf("proxy func resolved %v, want http://127.0.0.1:10809", u)
	}
}

func TestDownloadHTTPClientSocks5(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		hc, err := DownloadHTTPClient(scheme+"://127.0.0.1:1080", time.Minute)
		if err != nil {
			t.Fatalf("%s client: %v", scheme, err)
		}
		tr, ok := hc.Transport.(*http.Transport)
		if !ok || tr.DialContext == nil {
			t.Fatalf("%s: transport = %+v, want a socks5 DialContext", scheme, hc.Transport)
		}
	}
}

func TestDownloadHTTPClientUnknownSchemeFailsLoud(t *testing.T) {
	if _, err := DownloadHTTPClient("ftp://127.0.0.1:21", time.Minute); err == nil {
		t.Fatal("err = nil, want the unsupported-scheme error")
	}
}

// ValidateProxyScheme mirrors the load-time gate (config calls it):
// empty is valid (direct), unknown schemes are loud.
func TestValidateProxyScheme(t *testing.T) {
	for _, ok := range []string{"", "http://p:1", "https://p:1", "socks5://p:1", "socks5h://p:1"} {
		if err := ValidateProxyScheme(ok); err != nil {
			t.Errorf("ValidateProxyScheme(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"ftp://p:1", "://noparse", "just-a-word"} {
		if err := ValidateProxyScheme(bad); err == nil {
			t.Errorf("ValidateProxyScheme(%q) = nil, want an error", bad)
		}
	}
}
