package providers

import (
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestResolveEmbedsDirectFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		link string
	}{
		{name: "direct mp4", link: "https://cdn.example.com/videos/ep1.mp4"},
		{name: "direct m3u8", link: "https://cdn.example.com/hls/master.m3u8"},
		{name: "protocol-relative m3u8 stays untouched", link: "//cdn.example.com/hls/master.m3u8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sources, err := resolveEmbeds([]string{tt.link})
			if err != nil {
				t.Fatalf("resolveEmbeds(%q): %v", tt.link, err)
			}
			src, ok := sources["720"]
			if !ok {
				t.Fatalf("sources = %v, want a 720 entry (ExtractorFactory direct fallback, extractors.py:686-687)", sources)
			}
			if src.URL != tt.link || src.Quality != "720" {
				t.Errorf("source = %+v, want url %q quality 720", src, tt.link)
			}
		})
	}
}

func TestResolveEmbedsPendingExtractorNamed(t *testing.T) {
	t.Parallel()

	// The matcher table ports the ExtractorFactory URL matching order
	// (anicli-py anicli/core/extractors.py:658-671, 673-689).
	tests := []struct {
		name string
		link string
		want string
	}{
		{name: "kodik", link: "https://kodik.info/serial/12345/xyz", want: "kodik"},
		{name: "aniqit is kodik too", link: "https://aniqit.com/serial/1/abc", want: "kodik"},
		{name: "aniboom", link: "https://aniboom.one/embed/123?ep=1", want: "aniboom"},
		{name: "cdnvideohub", link: "https://animego.org/cdn-iframe/1/dub/1/2", want: "cdnvideohub"},
		{name: "alloha", link: "https://alloha.tv/serial/777", want: "alloha"},
		{name: "alloha all. substring", link: "https://all4all.example/watch/1", want: "alloha"},
		{name: "sibnet", link: "https://video.sibnet.ru/shell.php?videoid=1", want: "sibnet"},
		{name: "askor", link: "https://aksor.yani.tv/embed/9", want: "askor"},
		{name: "csst", link: "https://csst.online/embed/2", want: "csst"},
		{name: "sovetromantica embed", link: "https://sovetromantica.com/embed/episode_1", want: "sovetromantica_embed"},
		{name: "gogoplay", link: "https://gogoplay.io/embedplus?id=111", want: "gogoplay"},
		{name: "playtaku", link: "https://playtaku.net/streaming.php?id=5", want: "gogoplay"},
		{name: "streamtape", link: "https://streamtape.com/e/abc123", want: "streamtape"},
		{name: "dood", link: "https://dood.la/e/xyz", want: "dood"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sources, err := resolveEmbeds([]string{tt.link})
			if !errors.Is(err, contracts.ErrExtractFailed) {
				t.Fatalf("resolveEmbeds(%q) err = %v, want ErrExtractFailed", tt.link, err)
			}
			if !strings.Contains(err.Error(), "extractor:"+tt.want+" pending") {
				t.Errorf("err = %v, want extractor:%s pending context", err, tt.want)
			}
			if len(sources) != 0 {
				t.Errorf("sources = %v, want none for a pending extractor", sources)
			}
		})
	}
}

func TestResolveEmbedsDirectMediaBeatsExtractorSubstring(t *testing.T) {
	t.Parallel()

	// A raw media URL that ALSO matches an extractor substring resolves
	// to the direct 720 fallback in Python: the extractor extracts
	// nothing from a bare media file, so get_sources falls through to
	// the .mp4/.m3u8 branch (extractors.py:677-687). The suffix check
	// must therefore win over the extractor match.
	tests := []struct {
		name string
		link string
	}{
		{name: "all.mp4 matches alloha substring", link: "https://cdn.example.com/videos/all.mp4"},
		{name: "dood.mp4 matches dood substring", link: "https://cdn.example.com/videos/dood.mp4"},
		{name: "all. m3u8 matches alloha substring", link: "https://all.cdn.example.com/hls/master.m3u8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sources, err := resolveEmbeds([]string{tt.link})
			if err != nil {
				t.Fatalf("resolveEmbeds(%q) err = %v, want direct fallback (no pending extractor)", tt.link, err)
			}
			src, ok := sources["720"]
			if !ok {
				t.Fatalf("sources = %v, want a direct 720 entry", sources)
			}
			if src.URL != tt.link || src.Quality != "720" {
				t.Errorf("source = %+v, want url %q quality 720", src, tt.link)
			}
		})
	}
}

func TestResolveEmbedsUnmatchedURLYieldsNothing(t *testing.T) {
	t.Parallel()

	// No extractor matches and the URL is not a direct media file:
	// ExtractorFactory.get_sources returns {} (extractors.py:689).
	sources, err := resolveEmbeds([]string{"https://unknown.example/embed/watch?id=1"})
	if err != nil {
		t.Fatalf("resolveEmbeds: %v, want nil (Python returns {})", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want empty", sources)
	}
}

func TestResolveEmbedsMixedDirectAndPending(t *testing.T) {
	t.Parallel()

	// Python blends extractor results via dict.update: a failed extractor
	// contributes nothing while other links still resolve. The pending
	// marker must therefore not fail a mix that still yields a direct
	// source.
	sources, err := resolveEmbeds([]string{
		"https://aniboom.one/embed/123?ep=1",
		"https://cdn.example.com/videos/ep1.mp4",
	})
	if err != nil {
		t.Fatalf("resolveEmbeds mixed: %v, want nil while a direct source resolved", err)
	}
	if _, ok := sources["720"]; !ok {
		t.Errorf("sources = %v, want the direct 720 entry", sources)
	}
}
