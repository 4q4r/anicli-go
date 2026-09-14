package regression

import (
	"context"
	"errors"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/shikimori"
)

// fixedProvider is the deterministic provider backing the golden API
// captures: canned search/episodes/resolve answers, zero I/O.
type fixedProvider struct{}

const fixedProviderID = "fixed"

func (fixedProvider) ID() string { return fixedProviderID }
func (fixedProvider) Name() string {
	return "Fixed Regression Source"
}
func (fixedProvider) BaseURL() string                  { return "https://fixed.example" }
func (fixedProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }

func (fixedProvider) Search(_ context.Context, query string) ([]contracts.SearchResult, error) {
	return []contracts.SearchResult{
		{
			Title:    "Fixed hit for " + query,
			URL:      "https://fixed.example/anime/1",
			SourceID: fixedProviderID,
			Poster:   "https://fixed.example/poster/1.jpg",
			Meta:     map[string]any{"year": 1998},
		},
		{
			Title:    "Second fixed hit",
			URL:      "https://fixed.example/anime/2",
			SourceID: fixedProviderID,
		},
	}, nil
}

func fixedEpisodes() []contracts.Episode {
	return []contracts.Episode{
		{
			Num:   "1",
			Title: "Asteroid Blues",
			RawID: "ep-1",
			RawEmbeds: map[string][]string{
				"1080": {"https://fixed.example/embed/1/1080"},
				"720":  {"https://fixed.example/embed/1/720"},
			},
		},
		{
			Num:   "2",
			Title: "Stray Dog Strut",
			RawID: "ep-2",
			RawEmbeds: map[string][]string{
				"1080": {"https://fixed.example/embed/2/1080"},
			},
		},
	}
}

func (fixedProvider) GetEpisodes(_ context.Context, _ string) ([]contracts.Episode, error) {
	return fixedEpisodes(), nil
}

func (fixedProvider) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	urls, ok := episode.RawEmbeds[dubID]
	if !ok || len(urls) == 0 {
		return contracts.MediaStream{}, contracts.WrapProvider(fixedProviderID, contracts.OpResolveStream, 0,
			errors.New("dub not present on episode"))
	}
	return contracts.MediaStream{
		DubName: "Fixed Dub " + dubID,
		Links: map[string]contracts.VideoSource{
			dubID: {
				URL:     "https://fixed.example/media/" + episode.Num + "-" + dubID + ".m3u8?expires=1893456000",
				Quality: dubID,
				Type:    "m3u8",
				Headers: map[string]string{"Referer": "https://fixed.example/"},
			},
		},
	}, nil
}

// fixedShiki is the deterministic Shikimori client backing the golden
// API captures. All dates are fixed absolute values in the past so the
// now()-windowed calendar/hero projections are stable by construction.
type fixedShiki struct{}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func int64Ptr(i int64) *int64 { return &i }

func (fixedShiki) Autocomplete(_ context.Context, _ string, _ int) ([]shikimori.AutocompleteItem, error) {
	return []shikimori.AutocompleteItem{
		{
			ShikimoriID: 21,
			TitleRu:     strPtr("Ковбой Бибоп"),
			TitleEn:     strPtr("Cowboy Bebop"),
			PosterURL:   strPtr("/system/animes/original/21.jpg"),
			Type:        strPtr("TV"),
			Year:        intPtr(1998),
			URL:         "https://shikimori.io/animes/21",
		},
	}, nil
}

func (fixedShiki) GetUserRates(_ context.Context) ([]shikimori.UserRate, error) {
	return []shikimori.UserRate{
		{ID: 901, UserID: 1, TargetID: 21, TargetType: "Anime", Score: 9, Status: "watching", Episodes: 12},
		{ID: 902, UserID: 1, TargetID: 22, TargetType: "Anime", Score: 7, Status: "completed", Episodes: 26},
	}, nil
}

func (fixedShiki) GetAnimesInfo(_ context.Context, _ []int64) ([]shikimori.Anime, error) {
	return []shikimori.Anime{
		{
			ID:            21,
			Name:          "Cowboy Bebop",
			Russian:       "Ковбой Бибоп",
			Image:         shikimori.Image{Original: "/system/animes/original/21.jpg", Preview: "/system/animes/preview/21.jpg"},
			Episodes:      26,
			EpisodesAired: 24,
			Status:        "ongoing",
			Kind:          "TV",
			Score:         8.9,
			Description:   "Fixed regression description.",
			Genres:        []shikimori.Genre{{ID: 1, Name: "Action", Russian: "Экшен"}},
			NextEpisode:   13,
			// Fixed past date: always outside the calendar/feed windows.
			NextEpisodeAt: "2020-01-01T00:00:00Z",
			URL:           "/animes/21",
		},
		{
			ID:       22,
			Name:     "Fixed Second Anime",
			Russian:  "Фиксированное второе аниме",
			Image:    shikimori.Image{Original: "/system/animes/original/22.jpg"},
			Episodes: 26,
			Status:   "released",
			Kind:     "TV",
			Score:    7.1,
		},
	}, nil
}

func (fixedShiki) FetchOngoingCandidates(_ context.Context) ([]shikimori.OngoingCandidate, error) {
	return []shikimori.OngoingCandidate{
		{
			ShikimoriID: 21,
			TitleRu:     strPtr("Ковбой Бибоп"),
			TitleEn:     strPtr("Cowboy Bebop"),
			PosterURL:   "/system/animes/original/21.jpg",
			Year:        intPtr(1998),
			SourceURL:   "https://shikimori.io/animes/21",
		},
	}, nil
}

func (fixedShiki) GetAnimeDetails(_ context.Context, id int64) (*shikimori.AnimeDetails, error) {
	if id != 21 {
		return nil, context.DeadlineExceeded
	}
	return &shikimori.AnimeDetails{
		Anime: shikimori.Anime{
			ID:         21,
			Name:       "Cowboy Bebop",
			Russian:    "Ковбой Бибоп",
			Image:      shikimori.Image{Original: "/system/animes/original/21.jpg"},
			Episodes:   26,
			Status:     "ongoing",
			Kind:       "TV",
			Score:      8.9,
			AiredOn:    "1998-04-03",
			ReleasedOn: "1999-04-24",
		},
		Characters: []shikimori.Character{
			{ID: 1, Name: "Spike Spiegel", Russian: "Спайк Шпигель", Image: shikimori.Image{Original: "/system/characters/original/1.jpg"}, Roles: []string{"Main"}},
		},
		Staff: []shikimori.StaffMember{
			{ID: 2, Name: "Shinichiro Watanabe", Image: shikimori.Image{Original: "/system/people/original/2.jpg"}, Roles: []string{"Director"}},
		},
		Related: []shikimori.RelatedEntry{
			{Relation: "side_story", Anime: &shikimori.Anime{ID: 23, Name: "Fixed Side Story", Russian: "Фиксированная побочная история"}},
		},
		Similar: []shikimori.Anime{
			{ID: 24, Name: "Fixed Similar Anime", Russian: "Фиксированное похожее аниме"},
		},
	}, nil
}

func (fixedShiki) Authenticated() bool { return true }
