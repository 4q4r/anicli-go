package extractors

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// testNetConfig mirrors the providers test helper: user-tuned defaults,
// proxy stripped, so extractor tests never egress.
func testNetConfig() config.Network {
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	return cfg
}

// testHTTPClient builds a netclient against an httptest-compatible
// config (mirrors the providers test helper; extractor tests never touch
// the real network).
func testHTTPClient(t *testing.T) *netclient.Client {
	t.Helper()
	c, err := netclient.New(testNetConfig(), netclient.WithProvider("extractors-test"))
	if err != nil {
		t.Fatalf("netclient.New: %v", err)
	}
	return c
}
