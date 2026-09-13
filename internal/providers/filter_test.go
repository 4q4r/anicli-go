package providers

import (
	"context"
	"reflect"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// episodesStub is a contracts.Provider double returning a fixed
// episode list, used to exercise the dub stream filter.
type episodesStub struct {
	stubProvider
	episodes []contracts.Episode
}

func (s *episodesStub) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.episodes, nil
}

func TestNewStreamFilterInvalidRegexFailsLoud(t *testing.T) {
	t.Parallel()

	if _, err := NewStreamFilter([]string{"([unclosed"}); err == nil {
		t.Fatal("NewStreamFilter with an invalid regex must fail, got nil")
	}
	if _, err := NewStreamFilter([]string{"трейлер", "реклама"}); err != nil {
		t.Fatalf("NewStreamFilter with valid regexes must pass: %v", err)
	}
}

func TestStreamFilterFilterEmbeds(t *testing.T) {
	t.Parallel()

	filter, err := NewStreamFilter([]string{"(?i)трейлер", "^[0-9]+$"})
	if err != nil {
		t.Fatalf("NewStreamFilter: %v", err)
	}

	embeds := map[string][]string{
		"AniLibria.TV": {"https://e/1"},
		"трейлер":      {"https://e/2"},
		"Трейлер (HD)": {"https://e/3"},
		"Studio Dub":   {"https://e/4"},
		"720":          {"https://e/5"},
	}
	got := filter.FilterEmbeds(embeds)
	want := map[string][]string{
		"AniLibria.TV": {"https://e/1"},
		"Studio Dub":   {"https://e/4"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterEmbeds:\n got: %v\nwant: %v", got, want)
	}
	// The input map must not be mutated in place.
	if _, ok := embeds["трейлер"]; !ok {
		t.Fatal("FilterEmbeds must not mutate the input map")
	}
}

func TestStreamFilterPassthroughWithoutPatterns(t *testing.T) {
	t.Parallel()

	var nilFilter *StreamFilter
	embeds := map[string][]string{"AniLibria.TV": {"https://e/1"}}
	if got := nilFilter.FilterEmbeds(embeds); !reflect.DeepEqual(got, embeds) {
		t.Fatalf("nil filter must pass embeds through, got %v", got)
	}

	empty, err := NewStreamFilter(nil)
	if err != nil {
		t.Fatalf("NewStreamFilter(nil): %v", err)
	}
	if got := empty.FilterEmbeds(embeds); !reflect.DeepEqual(got, embeds) {
		t.Fatalf("empty filter must pass embeds through, got %v", got)
	}
}

func TestDubFilteredProviderGetEpisodes(t *testing.T) {
	t.Parallel()

	filter, err := NewStreamFilter([]string{"(?i)трейлер"})
	if err != nil {
		t.Fatalf("NewStreamFilter: %v", err)
	}
	inner := &episodesStub{episodes: []contracts.Episode{
		{
			Num: "1", RawID: "e1",
			RawEmbeds: map[string][]string{
				"AniLibria.TV": {"https://e/1"},
				"Трейлер":      {"https://e/2"},
			},
		},
		{Num: "2", RawID: "e2"}, // no embeds at all
	}}
	wrapped := dubFilteredProvider{Provider: inner, filter: filter}

	got, err := wrapped.GetEpisodes(context.Background(), "https://x")
	if err != nil {
		t.Fatalf("GetEpisodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("episode count must be preserved, got %d", len(got))
	}
	if _, ok := got[0].RawEmbeds["AniLibria.TV"]; !ok {
		t.Errorf("kept dub missing: %v", got[0].RawEmbeds)
	}
	if _, ok := got[0].RawEmbeds["Трейлер"]; ok {
		t.Errorf("excluded dub leaked through: %v", got[0].RawEmbeds)
	}
	if got[1].RawEmbeds != nil {
		t.Errorf("nil embeds must stay nil, got %v", got[1].RawEmbeds)
	}

	// Non-episode operations delegate unchanged.
	if wrapped.ID() != inner.id {
		t.Errorf("ID() = %q, want %q", wrapped.ID(), inner.id)
	}
}

func TestDubFilteredProviderGetEpisodesPropagatesError(t *testing.T) {
	t.Parallel()

	inner := &episodesStub{}
	inner.err = contracts.ErrProvider403
	wrapped := dubFilteredProvider{Provider: inner, filter: nil}

	if _, err := wrapped.GetEpisodes(context.Background(), "https://x"); err == nil {
		t.Fatal("GetEpisodes must propagate the inner error, got nil")
	}
}
