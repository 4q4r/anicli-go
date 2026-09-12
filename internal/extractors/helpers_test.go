package extractors

import (
	"github.com/an0nx/anicli-go/internal/config"
)

// testNetConfig mirrors the providers test helper: user-tuned defaults,
// proxy stripped, so extractor tests never egress.
func testNetConfig() config.Network {
	cfg := config.Default().Network
	cfg.ProxyURL = ""
	return cfg
}
