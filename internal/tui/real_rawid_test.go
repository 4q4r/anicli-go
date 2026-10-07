package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// The merged-session RawID seam (issue #157): the session merge
// composes "prov1:id1|prov2:id2" — prefixed even for a single source
// (MergeEpisodeLists) — and the python resolve loop strips the called
// provider's own part before provider.resolve_stream
// (cli/stream_resolver.py extract_best_source). The Go port's
// decomposition was missing, so the lua scripts received
// "animevib:{...}" and their json.decode crashed on the first byte
// 'a' — the live owner-reported resolve_stream failure. These tests
// pin the decomposition at the two production boundaries that hand a
// possibly-merged episode to a concrete provider.

// resolveRecorder fakes a registry provider and records the RawID
// every ResolveStream call receives — and the dubID too (the
// merged-track-key strip pin, fix-round 2 of #158).
type resolveRecorder struct {
	id     string
	empty  bool
	gotRaw []string
	gotDub []string
}

func (p *resolveRecorder) ID() string                       { return p.id }
func (p *resolveRecorder) Name() string                     { return p.id }
func (p *resolveRecorder) BaseURL() string                  { return "https://" + p.id + ".example" }
func (p *resolveRecorder) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }
func (p *resolveRecorder) Search(context.Context, string) ([]contracts.SearchResult, error) {
	return nil, nil
}
func (p *resolveRecorder) GetEpisodes(context.Context, string) ([]contracts.Episode, error) {
	return nil, nil
}

func (p *resolveRecorder) ResolveStream(_ context.Context, episode contracts.Episode, dubID string) (contracts.MediaStream, error) {
	p.gotRaw = append(p.gotRaw, episode.RawID)
	p.gotDub = append(p.gotDub, dubID)
	if p.empty {
		return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{}}, nil
	}
	return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{
		"720": {URL: "https://cdn.example/720.m3u8", Quality: "720"},
	}}, nil
}

const recorderMergedRawID = `animevib:{"n":"1","u":"https://www.animevib.ru/1.html"}`
const recorderBareRawID = `{"n":"1","u":"https://www.animevib.ru/1.html"}`

// TestRealEpisodeResolveStreamDecomposesMergedRawID pins the playback
// seam: realEpisode.ResolveStream — the service every TUI resolve
// fans out through (the stream picker and the separate-audio leg) —
// must hand the provider the bare provider-local raw id, never the
// merged bytes.
func TestRealEpisodeResolveStreamDecomposesMergedRawID(t *testing.T) {
	t.Parallel()

	rec := &resolveRecorder{id: "animevib"}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &realEpisode{registry: reg}

	if _, err := svc.ResolveStream(context.Background(), "animevib",
		contracts.Episode{Num: "1", RawID: recorderMergedRawID, RawEmbeds: map[string][]string{}}, "JAM"); err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(rec.gotRaw) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(rec.gotRaw))
	}
	if got := rec.gotRaw[0]; got != recorderBareRawID {
		t.Fatalf("provider got RawID %q, want the bare state json", got)
	}
}

// TestRealEpisodeResolveStreamStripsTrackTagFromDubID pins the dub
// half of the python extract_best_source decomposition (fix-round 2
// of #158): cli/stream_resolver.py:174 strips the merged-session
// track tag — provider_dub_name = re.sub(r"^\[.*?\]\s*", "", dub_key)
// — before provider.resolve_stream. The Go port skipped it: the
// remembered-dub scoped resolve handed "[animevib] Amazing Dubbing"
// to the lua script, whose bare-name comparison (the kodik
// data-title) could never match — the typed wall on EVERY episode,
// the live owner report. The provider boundary consumes the bare dub
// name its own episodes listing merges under.
func TestRealEpisodeResolveStreamStripsTrackTagFromDubID(t *testing.T) {
	t.Parallel()

	rec := &resolveRecorder{id: "animevib"}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	svc := &realEpisode{registry: reg}

	if _, err := svc.ResolveStream(context.Background(), "animevib",
		contracts.Episode{Num: "3", RawID: recorderMergedRawID, RawEmbeds: map[string][]string{}},
		"[animevib] Amazing Dubbing"); err != nil {
		t.Fatalf("ResolveStream: %v", err)
	}
	if len(rec.gotDub) != 1 || rec.gotDub[0] != "Amazing Dubbing" {
		t.Fatalf("provider got dubID %q, want the bare %q", rec.gotDub, "Amazing Dubbing")
	}
}

// TestDownloadOneStripsTrackTagFromDubID pins the download seam's dub
// strip: the task DubID is the merged track key (stampResolvedDub),
// and downloadOne resolves through the registry DIRECTLY — the same
// python-parity strip must apply before the provider call. A bare
// dubID passes through unchanged (the direct-callers contract).
func TestDownloadOneStripsTrackTagFromDubID(t *testing.T) {
	t.Parallel()

	rec := &resolveRecorder{id: "animevib", empty: true}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	c := &realCore{registry: reg}

	_, err := c.downloadOne(context.Background(), DownloadTask{
		ProviderID: "animevib",
		DubID:      "[animevib] Amazing Dubbing",
		Episode: contracts.Episode{
			Num:       "3",
			RawID:     recorderMergedRawID,
			RawEmbeds: map[string][]string{},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Fatalf("err = %v, want the empty-links quality verdict", err)
	}
	if len(rec.gotDub) != 1 || rec.gotDub[0] != "Amazing Dubbing" {
		t.Fatalf("provider got dubID %q, want the bare %q", rec.gotDub, "Amazing Dubbing")
	}
}

// TestDownloadOneDecomposesMergedRawID pins the download seam: the
// download task carries the merged session episode, and downloadOne
// resolves through the registry DIRECTLY (no EpisodeService hop) —
// the same decomposition must apply before the provider call. The
// recorder's empty link map fails the run right after the resolve
// (the honest quality verdict), which is exactly the seam under test.
func TestDownloadOneDecomposesMergedRawID(t *testing.T) {
	t.Parallel()

	rec := &resolveRecorder{id: "animevib", empty: true}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	c := &realCore{registry: reg}

	_, err := c.downloadOne(context.Background(), DownloadTask{
		ProviderID: "animevib",
		DubID:      "JAM",
		Episode: contracts.Episode{
			Num:       "1",
			RawID:     recorderMergedRawID,
			RawEmbeds: map[string][]string{},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Fatalf("err = %v, want the empty-links quality verdict", err)
	}
	if len(rec.gotRaw) != 1 {
		t.Fatalf("resolve calls = %d, want 1", len(rec.gotRaw))
	}
	if got := rec.gotRaw[0]; got != recorderBareRawID {
		t.Fatalf("provider got RawID %q, want the bare state json", got)
	}
}
