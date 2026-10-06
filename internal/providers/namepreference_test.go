package providers

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestTorrentProvidersNamePreference pins the PR42 name-preference
// routing: the foreign torrent feeds (animetosho, tokyotosho)
// index romaji/english release names only — a Cyrillic query there is
// guaranteed-zero — so they declare NamePrefLatin and the search
// fan-out routes them the latin variants. anilibria-torrent (PR37)
// and rutor (PR87) stay in the RU group (RU sites whose indexes match
// RU names — rutor verified live 2026-09-23: RU queries are
// first-class, е/ё treated alike) — they must NOT declare the latin
// preference.
func TestTorrentProvidersNamePreference(t *testing.T) {
	t.Parallel()

	latin := map[string]contracts.Provider{
		"animetosho": newAnimeTosho(AnimeToshoFeedBase, testClient(t, "animetosho"), nil),
		"tokyotosho": newTokyoTosho(TokyoToshoBase, testClient(t, "tokyotosho"), nil),
	}
	for id, p := range latin {
		np, ok := p.(contracts.NamePreferenceProvider)
		if !ok {
			t.Errorf("%s must implement contracts.NamePreferenceProvider", id)
			continue
		}
		if got := np.NamePreference(); got != contracts.NamePrefLatin {
			t.Errorf("%s name preference = %v, want NamePrefLatin", id, got)
		}
	}

	for _, id := range []string{"anilibria-torrent", "rutor"} {
		var p contracts.Provider
		switch id {
		case "anilibria-torrent":
			// The bundled script (PR145): no name_preference
			// declaration on the provider table. The capability
			// adapter carries the field with its zero value —
			// observationally the RU group (the caps.go doctrine),
			// so the assertion is on the VALUE, not the interface.
			p = luaProviderAtProduction(t, id)
		case "rutor":
			p = newRutor(RutorBase, testClient(t, id), nil)
		}
		if np, declares := p.(contracts.NamePreferenceProvider); declares && np.NamePreference() != contracts.NamePrefDefault {
			t.Errorf("%s must stay in the RU group (no latin preference declaration), got %v", id, np.NamePreference())
		}
	}
}

// TestRegistryNamePreference pins the registry accessor: declared
// preferences survive the wrapper layers, undeclared providers and
// unknown ids fall back to NamePrefDefault.
func TestRegistryNamePreference(t *testing.T) {
	t.Parallel()

	r := NewEmptyRegistry()
	if err := r.Register(newTokyoTosho(TokyoToshoBase, testClient(t, "tokyotosho"), nil)); err != nil {
		t.Fatalf("register tokyotosho: %v", err)
	}
	if err := r.Register(luaProviderAtProduction(t, "anilibria-torrent")); err != nil {
		t.Fatalf("register anilibria-torrent: %v", err)
	}

	if got := r.NamePreference("tokyotosho"); got != contracts.NamePrefLatin {
		t.Errorf("tokyotosho = %v, want NamePrefLatin", got)
	}
	if got := r.NamePreference("anilibria-torrent"); got != contracts.NamePrefDefault {
		t.Errorf("anilibria-torrent = %v, want NamePrefDefault (RU group)", got)
	}
	if got := r.NamePreference("unknown-provider"); got != contracts.NamePrefDefault {
		t.Errorf("unknown id = %v, want NamePrefDefault", got)
	}
}
