package providers

import (
	"fmt"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/storage"
)

// allFactories lists the wave-1 constructors in registry order. Each
// entry gets its own netclient client: providers never share cookie
// jars, and errors are tagged with the provider id.
var allFactories = []struct {
	id    string
	build func(http *netclient.Client) contracts.Provider
}{
	{"anilibria", func(http *netclient.Client) contracts.Provider {
		return newAnilibria(AniLibriaAPIBase, AniLibriaHost, http)
	}},
	{"animevost", func(http *netclient.Client) contracts.Provider {
		return newAnimevost(AnimeVostBase, http)
	}},
	{"anilib", func(http *netclient.Client) contracts.Provider {
		return newAnilib(AnilibAPIBase, http)
	}},
	{"animego", func(http *netclient.Client) contracts.Provider {
		return newAnimego(AnimeGoBase, http)
	}},
	{"sovetromantica", func(http *netclient.Client) contracts.Provider {
		return newSovetRomantica(SovetRomanticaBase, http)
	}},
}

// All builds every implemented provider: one netclient client each
// (browser-fingerprint profile, own cookie jar, provider-tagged errors)
// constructed from netCfg. Grow allFactories as later waves land.
func All(netCfg config.Network) ([]contracts.Provider, error) {
	out := make([]contracts.Provider, 0, len(allFactories))
	for _, factory := range allFactories {
		client, err := netclient.New(netCfg, netclient.WithProvider(factory.id))
		if err != nil {
			return nil, fmt.Errorf("build %s client: %w", factory.id, err)
		}
		out = append(out, factory.build(client))
	}
	return out, nil
}

// NewRegistry builds the full provider set with every provider wrapped
// in a SearchDelegator recording into stats. stats may be nil: searches
// then simply are not recorded.
func NewRegistry(netCfg config.Network, stats *storage.ProviderStatRepo) (*Registry, error) {
	bare, err := All(netCfg)
	if err != nil {
		return nil, err
	}

	reg := NewEmptyRegistry()
	for _, p := range bare {
		if err := reg.Register(SearchDelegator{Provider: p, stats: stats}); err != nil {
			return nil, err
		}
	}
	return reg, nil
}
