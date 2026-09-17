package providers

import (
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestTorrentProvidersNamePreference pins the PR42 name-preference
// routing: the foreign torrent feeds (nyaa, animetosho, tokyotosho)
// index romaji/english release names only — a Cyrillic query there is
// guaranteed-zero — so they declare NamePrefLatin and the search
// fan-out routes them the latin variants. anilibria-torrent stays in
// the RU group (a RU site whose API indexes RU names) — it must NOT
// declare the latin preference.
func TestTorrentProvidersNamePreference(t *testing.T) {
	t.Parallel()

	latin := map[string]contracts.Provider{
		"nyaa":       newNyaa(NyaaBase, testClient(t, "nyaa"), nil),
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

	ru := newAnilibriaTorrent(AniLibriaAPIBase, testClient(t, "anilibria-torrent"), nil)
	if _, declares := any(ru).(contracts.NamePreferenceProvider); declares {
		t.Error("anilibria-torrent must stay in the RU group (no latin preference declaration)")
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
	if err := r.Register(newAnilibriaTorrent(AniLibriaAPIBase, testClient(t, "anilibria-torrent"), nil)); err != nil {
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
