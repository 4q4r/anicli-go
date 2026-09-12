package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestManagerWiringWithRealClients proves the manager drives the real
// provider clients (not fakes) in the configured order over httptest
// endpoints and merges their aliases.
func TestManagerWiringWithRealClients(t *testing.T) {
	t.Parallel()

	var order []string
	var mu sync.Mutex
	record := func(id string) {
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
	}

	anilistSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		record("anilist")
		_, _ = w.Write([]byte(`{"data": {"Page": {"media": [
			{"title": {"romaji": "Wire Anime", "english": "Wired"}, "synonyms": ["WA"]}
		]}}}`))
	}))
	defer anilistSrv.Close()

	kitsuSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		record("kitsu")
		_, _ = w.Write([]byte(`{"data": [
			{"attributes": {"canonicalTitle": "Kitsu Title", "titles": {"ja_jp": "キツ"}}}
		]}`))
	}))
	defer kitsuSrv.Close()

	providers := []Provider{
		NewKitsuClient(newTestNet(t, "kitsu"), kitsuSrv.URL, nil),
		NewAniListClient(newTestNet(t, "anilist"), anilistSrv.URL, nil),
	}

	// Kitsu is configured first: it must be consulted before anilist.
	m := NewManager(providers, []string{"kitsu", "anilist"}, nil)
	m.perProviderTimeout = defaultPerProviderTimeout

	got, err := m.SearchAlternativeTitles(context.Background(), "query")
	if err != nil {
		t.Fatalf("SearchAlternativeTitles: %v", err)
	}

	mu.Lock()
	consulted := strings.Join(order, ",")
	mu.Unlock()
	if consulted != "kitsu,anilist" {
		t.Errorf("consultation order = %q, want kitsu,anilist", consulted)
	}

	joined := strings.Join(got, "|")
	for _, want := range []string{"Kitsu Title", "Wire Anime", "Wired", "WA", "キツ"} {
		if !strings.Contains(joined, want) {
			t.Errorf("merged aliases %q missing %q", joined, want)
		}
	}
}
