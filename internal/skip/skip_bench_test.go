package skip

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/an0nx/anicli-go/internal/netclient"
)

// PR81 benchmarks for the skip hot paths: the aniskip payload decode
// (every episode start consults it) and the FFmetadata chapter-file
// build (every offline/buffered watch embeds it).

// benchSkipHTTP answers every GET with a valid aniskip v2 envelope.
type benchSkipHTTP struct{ body []byte }

func (f *benchSkipHTTP) Get(_ context.Context, _ string, _ map[string]string) (*netclient.Response, error) {
	return &netclient.Response{StatusCode: 200, Status: "200 OK", Body: f.body}, nil
}

func (f *benchSkipHTTP) PostJSON(_ context.Context, _ string, _ any, _ map[string]string) (*netclient.Response, error) {
	return &netclient.Response{StatusCode: 200, Status: "200 OK", Body: []byte(`{}`)}, nil
}

func benchAniskipBody(b *testing.B) []byte {
	b.Helper()
	body, err := json.Marshal(map[string]any{
		"found": true,
		"results": []map[string]any{
			{"skip_type": "op", "interval": map[string]any{"start_time": 0.0, "end_time": 90.5}, "episode_length": 1440.0},
			{"skip_type": "ed", "interval": map[string]any{"start_time": 1320.25, "end_time": 1440.0}, "episode_length": 1440.0},
		},
	})
	if err != nil {
		b.Fatalf("marshal aniskip body: %v", err)
	}
	return body
}

var benchSinkIntervals []Interval

// BenchmarkAniskipDecode measures the full GetSkipTimes decode: URL
// build, envelope unmarshal and interval conversion over the fake
// transport (the per-episode-start cost when the API answers).
func BenchmarkAniskipDecode(b *testing.B) {
	b.ReportAllocs()
	client := NewAniSkipClient(&benchSkipHTTP{body: benchAniskipBody(b)}, AniSkipOptions{BaseURL: "https://aniskip.example"})
	ctx := context.Background()
	for b.Loop() {
		intervals, err := client.GetSkipTimes(ctx, 21, 7)
		if err != nil {
			b.Fatalf("get skip times: %v", err)
		}
		benchSinkIntervals = intervals
	}
}

// benchChapters is a realistic chapter set: OP, ED and one intermission
// rounded to 24 episodes' worth of variety.
func benchChapters() []Interval {
	out := make([]Interval, 0, 24)
	for i := range 24 {
		base := float64(i * 60)
		out = append(out,
			Interval{SkipType: "op", StartTime: base, EndTime: base + 90, EpisodeLength: 1440},
			Interval{SkipType: "ed", StartTime: base + 1320, EndTime: base + 1440, EpisodeLength: 1440},
		)
	}
	return out
}

var benchSinkMetadata string

// BenchmarkGenerateFFMetadata builds the ffmpeg chapter file for a
// 48-interval chapter set (the per-episode offline-write cost).
func BenchmarkGenerateFFMetadata(b *testing.B) {
	b.ReportAllocs()
	chapters := benchChapters()
	for b.Loop() {
		benchSinkMetadata = GenerateFFMetadata(chapters)
		if benchSinkMetadata == "" {
			b.Fatal("empty metadata")
		}
	}
}
