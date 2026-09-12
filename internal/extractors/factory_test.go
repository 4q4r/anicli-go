package extractors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestGetSourcesFallsThroughFailedExtractor pins the iteration contract
// (extractors.py:677-683): a URL matched by an earlier extractor that
// FAILS does not abort the walk — the factory continues to the next
// matching extractor exactly like the Python except: continue.
func TestGetSourcesFallsThroughFailedExtractor(t *testing.T) {
	t.Parallel()

	// One page serving aniboom's hls-fallback shape but nothing kodik
	// can scrape; the URL carries BOTH matchers' substrings, kodik first
	// in factory order.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<script>var player = new Player({"hls":{"src":"https:\/\/f.example\/y.m3u8"}});</script>`)
	}))
	t.Cleanup(srv.Close)

	sources, err := NewFactory(testHTTPClient(t)).GetSources(context.Background(), srv.URL+"/kodik/aniboom/1")
	if err != nil {
		t.Fatalf("GetSources: %v, want the aniboom result after the kodik failure", err)
	}
	if src, ok := sources["1080"]; !ok || src.URL != "https://f.example/y.m3u8" {
		t.Errorf("sources = %v, want the aniboom hls fallback link", sources)
	}
}

// TestGetSourcesSurfacesErrorWhenNothingResolves pins that a walk where
// every matching extractor fails yields the first typed error instead of
// a silent empty (the Go replacement for Python's swallowed exceptions).
func TestGetSourcesSurfacesErrorWhenNothingResolves(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html>nothing</html>")
	}))
	t.Cleanup(srv.Close)

	sources, err := NewFactory(testHTTPClient(t)).GetSources(context.Background(), srv.URL+"/kwik/1")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want the first extractor's typed error", err)
	}
	if !strings.Contains(err.Error(), "extractor:kwik") {
		t.Errorf("err = %v, want extractor:kwik context", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want none", sources)
	}
}

// newTestFactory builds a Factory around a client that must never be
// contacted by these tests (every URL here is either unmatched, direct
// media, or a skipped extractor).
func newTestFactory(t *testing.T) *Factory {
	t.Helper()
	return NewFactory(nil)
}

// TestFactoryOrder pins the Python ExtractorFactory registration order
// (anicli-py anicli/core/extractors.py:658-671) with the task-mandated
// kwik extractor appended where the Python original disabled it.
func TestFactoryOrder(t *testing.T) {
	t.Parallel()

	f := newTestFactory(t)
	want := []string{
		"kodik", "aniboom", "cdnvideohub", "alloha", "sibnet",
		"askor", "csst", "sovetromantica_embed",
		"gogoplay", "streamtape", "dood", "kwik",
	}
	if len(f.extractors) != len(want) {
		t.Fatalf("factory has %d extractors, want %d", len(f.extractors), len(want))
	}
	for i, name := range want {
		if got := f.extractors[i].Name(); got != name {
			t.Errorf("extractors[%d] = %q, want %q", i, got, name)
		}
	}
}

// TestGetSourcesDirectFallback ports the get_sources tail
// (extractors.py:686-687): a bare media URL no extractor matches resolves
// to a single quality-720 source.
func TestGetSourcesDirectFallback(t *testing.T) {
	t.Parallel()

	for _, link := range []string{
		"https://cdn.example.com/videos/ep1.mp4",
		"https://cdn.example.com/hls/master.m3u8",
	} {
		sources, err := newTestFactory(t).GetSources(context.Background(), link)
		if err != nil {
			t.Fatalf("GetSources(%q): %v", link, err)
		}
		src, ok := sources["720"]
		if !ok {
			t.Fatalf("GetSources(%q) = %v, want a 720 entry (direct fallback)", link, sources)
		}
		if src.URL != link || src.Quality != "720" {
			t.Errorf("source = %+v, want url %q quality 720", src, link)
		}
	}
}

// TestGetSourcesNoMatchYieldsNothing ports the Python {} return for URLs
// no extractor claims (extractors.py:689).
func TestGetSourcesNoMatchYieldsNothing(t *testing.T) {
	t.Parallel()

	sources, err := newTestFactory(t).GetSources(context.Background(), "https://unknown.example/embed/watch?id=1")
	if err != nil {
		t.Fatalf("GetSources: %v, want nil (Python returns {})", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want empty", sources)
	}
}

// TestGetSourcesSkippedExtractors pins the deliberate skip of the three
// factory extractors unreachable from the 11 registered providers (see
// extractors.go for the evidence). Their URLs surface a typed
// ErrExtractFailed naming the extractor instead of a silent {}.
func TestGetSourcesSkippedExtractors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		link string
		want string
	}{
		{name: "askor", link: "https://aksor.yani.tv/embed/9", want: "askor"},
		{name: "csst", link: "https://csst.online/embed/2", want: "csst"},
		{name: "sovetromantica embed", link: "https://sovetromantica.com/embed/episode_1", want: "sovetromantica_embed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sources, err := newTestFactory(t).GetSources(context.Background(), tt.link)
			if !errors.Is(err, contracts.ErrExtractFailed) {
				t.Fatalf("GetSources(%q) err = %v, want ErrExtractFailed", tt.link, err)
			}
			if !strings.Contains(err.Error(), "extractor:"+tt.want) {
				t.Errorf("err = %v, want extractor:%s context", err, tt.want)
			}
			if !strings.Contains(err.Error(), "unreachable") {
				t.Errorf("err = %v, want the unreachable-providers justification", err)
			}
			if len(sources) != 0 {
				t.Errorf("sources = %v, want none", sources)
			}
		})
	}
}
