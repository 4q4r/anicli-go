package providers

import (
	"context"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/extractors"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// resolveEmbeds is the provider-side name of extractors.Resolve (the
// PR116 extraction moved the implementation next to the factory so
// the Lua SDK's anicli.extract shares the ONE loop — the dict.update
// merge, the .mp4/.m3u8 fast path and the first-error-only rule keep
// their canonical docs there).
func resolveEmbeds(ctx context.Context, http *netclient.Client, links []string) (map[string]contracts.VideoSource, error) {
	return extractors.Resolve(ctx, http, links)
}
