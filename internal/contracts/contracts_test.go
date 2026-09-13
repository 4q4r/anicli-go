package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// JSON key parity with the Python original (anicli-py anicli/core/models.py,
// pydantic snake_case serialization) is part of the port contract; the exact
// JSON assertions below pin it.

func TestSourceTypeValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  SourceType
		want string
	}{
		{"video", SourceTypeVideo, "video"},
		{"audio", SourceTypeAudio, "audio"},
		{"both", SourceTypeBoth, "both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if string(tt.got) != tt.want {
				t.Fatalf("SourceType %q = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestDTOJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dto  any
		want string
	}{
		{
			name: "search result minimal",
			dto:  SearchResult{Title: "Cowboy Bebop", URL: "https://example.com/a", SourceID: "animego"},
			want: `{"title":"Cowboy Bebop","url":"https://example.com/a","source_id":"animego"}`,
		},
		{
			name: "search result full",
			dto: SearchResult{
				Title: "Trigun", URL: "https://example.com/b", SourceID: "anilib", Poster: "https://cdn/p.jpg",
				Meta: map[string]any{"year": "1998", "dubs": []string{"original", "rus"}},
			},
			want: `{"title":"Trigun","url":"https://example.com/b","source_id":"anilib","poster":"https://cdn/p.jpg"` +
				`,"meta":{"dubs":["original","rus"],"year":"1998"}}`,
		},
		{
			name: "episode minimal",
			dto:  Episode{Num: "1", RawID: "ep-1"},
			want: `{"num":"1","raw_id":"ep-1"}`,
		},
		{
			name: "episode full",
			dto: Episode{
				Num: "2", Title: "Red Eye", RawID: "ep-2",
				RawEmbeds: map[string][]string{"Дубляж": {"https://e/1", "https://e/2"}},
			},
			want: `{"num":"2","title":"Red Eye","raw_id":"ep-2","raw_embeds":{"Дубляж":["https://e/1","https://e/2"]}}`,
		},
		{
			name: "video source",
			dto: VideoSource{
				URL: "https://s/hls.m3u8", Quality: "1080",
				Headers:      map[string]string{"Referer": "https://example.com"},
				ExtraMPVOpts: []string{"--referrer=https://example.com"},
				Type:         "m3u8",
			},
			want: `{"url":"https://s/hls.m3u8","quality":"1080","headers":{"Referer":"https://example.com"},` +
				`"extra_mpv_opts":["--referrer=https://example.com"],"type":"m3u8"}`,
		},
		{
			name: "media stream",
			dto: MediaStream{
				DubName: "AniLibria.TV",
				Links: map[string]VideoSource{
					"1080": {URL: "https://s/1080.m3u8", Quality: "1080"},
				},
			},
			want: `{"dub_name":"AniLibria.TV","links":{"1080":{"url":"https://s/1080.m3u8","quality":"1080"}}}`,
		},
		{
			name: "dub option",
			dto:  DubOption{ID: "rus", Name: "Оригинал+суб"},
			want: `{"id":"rus","name":"Оригинал+суб"}`,
		},
		{
			name: "dub option with language",
			dto:  DubOption{ID: "sub", Name: "Original", Language: "ja"},
			want: `{"id":"sub","name":"Original","language":"ja"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tt.dto)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("marshal mismatch:\n got: %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// TestDTORoundTrip: marshal -> unmarshal -> re-marshal must be stable for
// every DTO (covers unmarshal tag symmetry and optional-field handling).
func TestDTORoundTrip(t *testing.T) {
	t.Parallel()

	search := SearchResult{
		Title: "Bebop", URL: "https://x/1", SourceID: "animego", Poster: "https://p/1",
		Meta: map[string]any{"k": "v"},
	}
	episode := Episode{
		Num: "OVA", Title: "special", RawID: "ova-1",
		RawEmbeds: map[string][]string{"jap": {"https://e/1"}},
	}
	video := VideoSource{
		URL: "https://x/2", Quality: "720",
		Headers: map[string]string{"Referer": "https://r"}, ExtraMPVOpts: []string{"--a"}, Type: "mp4",
	}
	stream := MediaStream{
		DubName: "Studio",
		Links:   map[string]VideoSource{"720": {URL: "https://x/3", Quality: "720", Type: "mp4"}},
	}

	assertStableJSON(t, search)
	assertStableJSON(t, episode)
	assertStableJSON(t, video)
	assertStableJSON(t, stream)
	assertStableJSON(t, DubOption{ID: "i", Name: "n"})
	assertStableJSON(t, DubOption{ID: "i", Name: "n", Language: "ru"})
	assertStableJSON(t, SearchResult{Title: "t", URL: "u", SourceID: "s"}) // empty optional fields
	assertStableJSON(t, Episode{Num: "1", RawID: "r"})                     // empty optional fields
	assertStableJSON(t, MediaStream{DubName: "d"})                         // empty optional fields
}

func assertStableJSON[T any](t *testing.T, v T) {
	t.Helper()

	first, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%T marshal: %v", v, err)
	}
	var dupe T
	if err := json.Unmarshal(first, &dupe); err != nil {
		t.Fatalf("%T unmarshal: %v", v, err)
	}
	second, err := json.Marshal(dupe)
	if err != nil {
		t.Fatalf("%T re-marshal: %v", v, err)
	}
	if string(first) != string(second) {
		t.Fatalf("%T round-trip mismatch:\n first: %s\nsecond: %s", v, first, second)
	}
}

// Compile-time proof that the consumer-side Provider contract is satisfiable.
type fakeProvider struct{}

func (fakeProvider) ID() string             { return "fake" }
func (fakeProvider) Name() string           { return "Fake" }
func (fakeProvider) BaseURL() string        { return "https://fake.example" }
func (fakeProvider) SourceType() SourceType { return SourceTypeBoth }

func (fakeProvider) Search(context.Context, string) ([]SearchResult, error) {
	return nil, ErrNotFound
}

func (fakeProvider) GetEpisodes(context.Context, string) ([]Episode, error) {
	return nil, ErrNotFound
}

func (fakeProvider) ResolveStream(context.Context, Episode, string) (MediaStream, error) {
	return MediaStream{}, ErrNotFound
}

var _ Provider = fakeProvider{}

func TestProviderInterfaceSatisfied(t *testing.T) {
	t.Parallel()

	var p Provider = fakeProvider{}
	if p.ID() != "fake" || p.SourceType() != SourceTypeBoth {
		t.Fatal("fake provider methods mismatch")
	}
}

func TestSentinelErrors(t *testing.T) {
	t.Parallel()

	all := []error{
		ErrGeoBlocked, ErrProvider403, ErrProviderTimeout, ErrExtractFailed,
		ErrAllCandidatesFailed, ErrNotFound, ErrInvalidInput,
	}
	seen := make(map[string]struct{}, len(all))
	for _, err := range all {
		if err == nil {
			t.Fatalf("sentinel %v is nil", err)
		}
		if _, dup := seen[err.Error()]; dup {
			t.Fatalf("duplicate sentinel message: %q", err.Error())
		}
		seen[err.Error()] = struct{}{}
	}
}

func TestProviderErrorIs(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrGeoBlocked, ErrProvider403, ErrProviderTimeout, ErrExtractFailed,
		ErrAllCandidatesFailed, ErrNotFound, ErrInvalidInput,
	}
	for _, sentinel := range sentinels {
		t.Run(sentinel.Error(), func(t *testing.T) {
			t.Parallel()

			pe := &ProviderError{Provider: "animego", Op: "search", StatusCode: 403, Err: sentinel}
			if !errors.Is(pe, sentinel) {
				t.Fatalf("errors.Is(ProviderError, %v) = false", sentinel)
			}

			// Must survive intermediate %w wrapping.
			wrapped := fmt.Errorf("fan-out failed: %w", fmt.Errorf("inner: %w", pe))
			if !errors.Is(wrapped, sentinel) {
				t.Fatalf("errors.Is(wrapped, %v) = false", sentinel)
			}
			if !errors.Is(wrapped, pe) {
				t.Fatal("errors.Is(wrapped, *ProviderError) = false")
			}
		})
	}
}

func TestProviderErrorMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *ProviderError
		want []string // substrings that must all appear
		bad  string   // substring that must NOT appear
	}{
		{
			name: "with status",
			err:  &ProviderError{Provider: "kodik", Op: "search", StatusCode: 403, Err: ErrProvider403},
			want: []string{"kodik", "search", "403", ErrProvider403.Error()},
		},
		{
			name: "without status",
			err:  &ProviderError{Provider: "anilib", Op: "resolve_stream", Err: ErrExtractFailed},
			want: []string{"anilib", "resolve_stream", ErrExtractFailed.Error()},
			bad:  "403",
		},
		{
			name: "nil inner",
			err:  &ProviderError{Provider: "dreamcast", Op: "get_episodes"},
			want: []string{"dreamcast", "get_episodes"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			msg := tt.err.Error()
			for _, sub := range tt.want {
				if !strings.Contains(msg, sub) {
					t.Fatalf("message %q missing substring %q", msg, sub)
				}
			}
			if tt.bad != "" && strings.Contains(msg, tt.bad) {
				t.Fatalf("message %q must not contain %q", msg, tt.bad)
			}
		})
	}
}

func TestWrapProvider(t *testing.T) {
	t.Parallel()

	pe := WrapProvider("animepahe", "search", 429, ErrGeoBlocked)
	if pe.Provider != "animepahe" || pe.Op != "search" || pe.StatusCode != 429 {
		t.Fatalf("WrapProvider fields mismatch: %+v", pe)
	}
	if !errors.Is(pe, ErrGeoBlocked) {
		t.Fatal("WrapProvider result not errors.Is-compatible with inner sentinel")
	}

	// nil inner error must not panic on Error() or Unwrap().
	peNil := WrapProvider("sameband", "search", 0, nil)
	_ = peNil.Error()
	if unwrapped := peNil.Unwrap(); unwrapped != nil {
		t.Fatalf("Unwrap of nil-inner ProviderError = %v, want nil", unwrapped)
	}
}
