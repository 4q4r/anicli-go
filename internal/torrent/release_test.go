package torrent

import (
	"reflect"
	"testing"
)

// TestParseReleaseName pins the quality/release-name parsing against
// REAL release names (no ML, no fuzzy matching — regex-only,
// fail-soft: missing fields are just absent labels).
func TestParseReleaseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want Quality
	}{
		{
			name: "fansub brackets SxxEyy",
			in:   "[GroupName] Title S01E01 [1080p][HEVC].mkv",
			want: Quality{
				Group:      "GroupName",
				Resolution: "1080",
				VideoCodec: "HEVC",
				Episodes:   []int{1},
			},
		},
		{
			name: "dash episode with WxH resolution",
			in:   "Title - 01 (BDRip 1920x1080 x264 FLAC)",
			want: Quality{
				Resolution: "1080",
				Source:     "BDRip",
				VideoCodec: "x264",
				Audio:      "FLAC",
				Episodes:   []int{1},
			},
		},
		{
			name: "web-dl 4K HDR",
			in:   "Title (2024) WEB-DL 2160p HDR",
			want: Quality{
				Resolution: "2160",
				Source:     "WEB-DL",
			},
		},
		{
			name: "anilibria ru-dub marker",
			in:   "AniLibria.TV × Title (2024) BDRip 1080p",
			want: Quality{
				Dub:        "AniLibria",
				Resolution: "1080",
				Source:     "BDRip",
			},
		},
		{
			name: "russian batch range with из",
			in:   "Тайтл (2024) BDRip 1080p | 01-12 из 12",
			want: Quality{
				Resolution: "1080",
				Source:     "BDRip",
				Episodes:   []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			},
		},
		{
			name: "subplease batch",
			in:   "[SubsPlease] (Batch) Title (01-12) (1080p) [AAC]",
			want: Quality{
				Group:      "SubsPlease",
				Resolution: "1080",
				Audio:      "AAC",
				Episodes:   []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			},
		},
		{
			name: "per-file episode in a batch",
			in:   "Title - 03.mkv",
			want: Quality{Episodes: []int{3}},
		},
		{
			name: "bracket episode",
			in:   "Title [07] [720p].mp4",
			want: Quality{Resolution: "720", Episodes: []int{7}},
		},
		{
			name: "ep-of-count single episode",
			in:   "Тайтл (2024) BDRip 1080p | 03 из 12",
			want: Quality{
				Resolution: "1080",
				Source:     "BDRip",
				Episodes:   []int{3},
			},
		},
		{
			name: "lowercase tokens",
			in:   "title s02e15 720p webrip x265 opus",
			want: Quality{
				Resolution: "720",
				Source:     "WEBRip",
				VideoCodec: "x265",
				Audio:      "Opus",
				Episodes:   []int{15},
			},
		},
		{
			name: "trailing group tag",
			in:   "Title - 05 [HorribleSubs].mkv",
			want: Quality{
				Group:    "HorribleSubs",
				Episodes: []int{5},
			},
		},
		{
			name: "av1 and ac3",
			in:   "Title (2024) 2160p AV1 AC3 3840x2160",
			want: Quality{
				Resolution: "2160",
				VideoCodec: "AV1",
				Audio:      "AC3",
			},
		},
		{
			name: "h265 alias",
			in:   "Title 1080p H.265 10bit",
			want: Quality{
				Resolution: "1080",
				VideoCodec: "x265",
			},
		},
		{
			name: "levsha dub",
			in:   "Тайтл ТВ-1 01-12 [ТВ] [Levsha] [BDRip 1920x1080 x265]",
			want: Quality{
				Dub:        "Левша",
				Source:     "BDRip",
				Resolution: "1080",
				VideoCodec: "x265",
				Episodes:   []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
			},
		},
		{
			name: "rus dub marker",
			in:   "Title S01 (RUS) BluRay 1080p",
			want: Quality{
				Dub:        "RUS",
				Resolution: "1080",
				Source:     "BluRay",
			},
		},
		{
			name: "nothing found fails soft",
			in:   "Просто Тайтл",
			want: Quality{},
		},
		{
			name: "jam dub trailing tag is also the group",
			in:   "Title - 01 (BDRip 1080p) [JAM]",
			want: Quality{
				Group:      "JAM",
				Dub:        "JAM",
				Resolution: "1080",
				Source:     "BDRip",
				Episodes:   []int{1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ParseQuality(tt.in)
			if got.Group != tt.want.Group {
				t.Errorf("Group = %q, want %q", got.Group, tt.want.Group)
			}
			if got.Resolution != tt.want.Resolution {
				t.Errorf("Resolution = %q, want %q", got.Resolution, tt.want.Resolution)
			}
			if got.Source != tt.want.Source {
				t.Errorf("Source = %q, want %q", got.Source, tt.want.Source)
			}
			if got.VideoCodec != tt.want.VideoCodec {
				t.Errorf("VideoCodec = %q, want %q", got.VideoCodec, tt.want.VideoCodec)
			}
			if got.Audio != tt.want.Audio {
				t.Errorf("Audio = %q, want %q", got.Audio, tt.want.Audio)
			}
			if got.Dub != tt.want.Dub {
				t.Errorf("Dub = %q, want %q", got.Dub, tt.want.Dub)
			}
			if !reflect.DeepEqual(got.Episodes, tt.want.Episodes) {
				t.Errorf("Episodes = %v, want %v", got.Episodes, tt.want.Episodes)
			}
		})
	}
}

func TestQualityBadge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   Quality
		want string
	}{
		{Quality{Resolution: "1080"}, "1080p"},
		{Quality{Resolution: "2160"}, "2160p"},
		{Quality{}, "?"},
	}
	for _, tt := range tests {
		if got := tt.in.Badge(); got != tt.want {
			t.Errorf("Quality%+v.Badge() = %q, want %q", tt.in, got, tt.want)
		}
	}
}
