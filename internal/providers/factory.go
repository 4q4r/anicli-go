package providers

import (
	"fmt"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/storage"
)

// allFactories lists the provider constructors in registry order. Each
// entry gets its own netclient client: providers never share cookie
// jars, and errors are tagged with the provider id. The build function
// receives the full settings: wave-2 providers consume per-provider
// configuration (kodik's API token).
var allFactories = []struct {
	id    string
	build func(http *netclient.Client, cfg config.Settings) contracts.Provider
}{
	{"anilibria", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newAnilibria(AniLibriaAPIBase, AniLibriaHost, http)
	}},
	{"animevost", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newAnimevost(AnimeVostBase, http)
	}},
	{"anilib", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newAnilib(AnilibAPIBase, http)
	}},
	{"animego", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newAnimego(AnimeGoBase, http)
	}},
	{"sovetromantica", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newSovetRomantica(SovetRomanticaBase, http)
	}},
	{"gogoanime", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newGogoAnime(GogoAnimeBase, GogoAnimeAjaxBase, http)
	}},
	{"animepahe", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newAnimePahe(AnimePaheBase, http)
	}},
	{"dreamcast", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newDreamCast(DreamCastBase, http)
	}},
	{"sameband", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newSameBand(SameBandBase, http)
	}},
	{"kodik", func(http *netclient.Client, cfg config.Settings) contracts.Provider {
		return newKodik(KodikAPIBase, cfg.Providers.Kodik.Token, http)
	}},
	{"allanime", func(http *netclient.Client, _ config.Settings) contracts.Provider {
		return newAllAnime(AllAnimeAPIBase, AllAnimeReferer, AllAnimeInternalBase, http)
	}},
}

// All builds every implemented provider: one netclient client each
// (browser-fingerprint profile, own cookie jar, provider-tagged errors)
// constructed from cfg.Network, plus per-provider settings where a
// source needs them (kodik's token). Grow allFactories as later waves
// land.
func All(cfg config.Settings) ([]contracts.Provider, error) {
	return all(cfg, nil)
}

// all is All with extra netclient options applied to every client
// (the CF solver wiring).
func all(cfg config.Settings, extra []netclient.Option) ([]contracts.Provider, error) {
	out := make([]contracts.Provider, 0, len(allFactories))
	for _, factory := range allFactories {
		opts := append([]netclient.Option{netclient.WithProvider(factory.id)}, extra...)
		client, err := netclient.New(cfg.Network, opts...)
		if err != nil {
			return nil, fmt.Errorf("build %s client: %w", factory.id, err)
		}
		out = append(out, factory.build(client, cfg))
	}
	return out, nil
}

// NewRegistry builds the full provider set with every provider wrapped
// in a SearchDelegator recording into stats. stats may be nil: searches
// then simply are not recorded. When [cf].enabled the CF challenge
// ladder is wired into every client; Close releases it.
func NewRegistry(cfg config.Settings, stats *storage.ProviderStatRepo) (*Registry, error) {
	cfOpts, cfClose, err := buildCFOptions(cfg)
	if err != nil {
		return nil, err
	}
	bare, err := all(cfg, cfOpts)
	if err != nil {
		return nil, err
	}

	reg := NewEmptyRegistry()
	for _, p := range bare {
		if err := reg.Register(SearchDelegator{Provider: p, stats: stats}); err != nil {
			return nil, err
		}
	}
	reg.cfClose = cfClose
	return reg, nil
}
