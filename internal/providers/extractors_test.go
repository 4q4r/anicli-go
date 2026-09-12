package providers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// resolveCtx is the context the wiring tests pass; none of these cases
// perform network I/O (direct suffixes and skipped extractors only).
var resolveCtx = context.Background()

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

			sources, err := resolveEmbeds(resolveCtx, nil, []string{tt.link})
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

// TestResolveEmbedsSkippedExtractorNamed covers the three Python factory
// extractors deliberately not ported (unreachable from the 11 registered
// providers — evidence in internal/extractors/extractors.go). Their URLs
// surface the typed ErrExtractFailed naming the extractor, without any
// network I/O.
func TestResolveEmbedsSkippedExtractorNamed(t *testing.T) {
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

			sources, err := resolveEmbeds(resolveCtx, nil, []string{tt.link})
			if !errors.Is(err, contracts.ErrExtractFailed) {
				t.Fatalf("resolveEmbeds(%q) err = %v, want ErrExtractFailed", tt.link, err)
			}
			if !strings.Contains(err.Error(), "extractor:"+tt.want) {
				t.Errorf("err = %v, want extractor:%s context", err, tt.want)
			}
			if !strings.Contains(err.Error(), "unreachable") {
				t.Errorf("err = %v, want the unreachable-providers justification", err)
			}
			if len(sources) != 0 {
				t.Errorf("sources = %v, want none for a skipped extractor", sources)
			}
		})
	}
}

func TestResolveEmbedsDirectMediaBeatsExtractorSubstring(t *testing.T) {
	t.Parallel()

	// A raw media URL that ALSO matches an extractor substring resolves
	// to the direct 720 fallback without contacting the extractor: the
	// Python extractor extracts nothing from a bare media file, so
	// get_sources falls through to the .mp4/.m3u8 branch
	// (extractors.py:677-687). Checking the suffix first reproduces that
	// observable outcome while skipping the wasted embed-page fetch.
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

			sources, err := resolveEmbeds(resolveCtx, nil, []string{tt.link})
			if err != nil {
				t.Fatalf("resolveEmbeds(%q): %v, want direct fallback", tt.link, err)
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
	sources, err := resolveEmbeds(resolveCtx, nil, []string{"https://unknown.example/embed/watch?id=1"})
	if err != nil {
		t.Fatalf("resolveEmbeds: %v, want nil (Python returns {})", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want empty", sources)
	}
}

func TestResolveEmbedsMixedDirectAndSkipped(t *testing.T) {
	t.Parallel()

	// Python blends extractor results via dict.update: a failed extractor
	// contributes nothing while other links still resolve. A skipped
	// extractor must therefore not fail a mix that still yields a direct
	// source.
	sources, err := resolveEmbeds(resolveCtx, nil, []string{
		"https://csst.online/embed/2",
		"https://cdn.example.com/videos/ep1.mp4",
	})
	if err != nil {
		t.Fatalf("resolveEmbeds mixed: %v, want nil while a direct source resolved", err)
	}
	if _, ok := sources["720"]; !ok {
		t.Errorf("sources = %v, want the direct 720 entry", sources)
	}
}

// TestResolveEmbedsLaterKeysOverwrite pins the Python dict.update merge
// semantics across links (gogoanime.py:128-132, animego.py:134-137,
// kodik.py:178-183, anilib.py:162): when two links resolve to the same
// quality key, the later link wins.
func TestResolveEmbedsLaterKeysOverwrite(t *testing.T) {
	t.Parallel()

	sources, err := resolveEmbeds(resolveCtx, nil, []string{
		"https://cdn.example.com/videos/ep1-first.mp4",
		"https://cdn.example.com/videos/ep1-second.mp4",
	})
	if err != nil {
		t.Fatalf("resolveEmbeds: %v", err)
	}
	src, ok := sources["720"]
	if !ok {
		t.Fatalf("sources = %v, want a 720 entry", sources)
	}
	if src.URL != "https://cdn.example.com/videos/ep1-second.mp4" {
		t.Errorf("720 URL = %q, want the LATER link (dict.update overwrite)", src.URL)
	}
	if len(sources) != 1 {
		t.Errorf("sources = %v, want exactly the one merged quality key", sources)
	}
}
