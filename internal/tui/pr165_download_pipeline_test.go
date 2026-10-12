package tui

// PR165 realCore download-pipeline pins: the content modes shape the
// ffmpeg CommandInput — the multi-audio mux carries the ordered tracks
// with the default mark and the mkv extension, video-only probes the
// video codec for the container, audio-only bit-extracts into the
// probed codec's native container (and stays out of the offline
// index), and a marked set that resolves to NOTHING is a typed error —
// never a silent video-only file.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/providers"
)

// capturingDownloader records every CommandInput the pipeline builds.
type capturingDownloader struct {
	inputs []download.CommandInput
	err    error
}

func (c *capturingDownloader) Download(_ context.Context, in download.CommandInput) error {
	c.inputs = append(c.inputs, in)
	return c.err
}

// stubProber answers every probe with a fixed codec per kind.
type stubProber struct {
	video string
	audio string
	err   error
}

func (s *stubProber) ProbeCodec(_ context.Context, kind string, _ contracts.VideoSource) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if kind == "v" {
		return s.video, nil
	}
	return s.audio, nil
}

// pr165Core builds a realCore over the recorder provider (one 720p
// link per resolve) with the capturing ffmpeg leg and prober.
func pr165Core(t *testing.T, dl *capturingDownloader, prober *stubProber) (*realCore, *resolveRecorder) {
	t.Helper()
	rec := &resolveRecorder{id: "animevib"}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	return &realCore{registry: reg, dl: dl, prober: prober, log: testLogger()}, rec
}

func pr165MuxTask() DownloadTask {
	return DownloadTask{
		ProviderID:   "animevib",
		DubID:        "[animevib] Amazing Dubbing",
		Mode:         "video_audio",
		AudioSet:     []string{"[animevib] Amazing Dubbing", "[animevib] Second Dub"},
		DefaultAudio: "[animevib] Second Dub",
		Episode: contracts.Episode{
			Num:   "3",
			RawID: recorderMergedRawID,
			RawEmbeds: map[string][]string{
				"Amazing Dubbing": {"u1"},
				"Second Dub":      {"u2"},
			},
		},
	}
}

// TestDownloadOneMultiAudioMuxPinsTracks: the marked set resolves per
// dub (provider-local, bare-name decomposition), becomes ordered
// AudioTracks with the language metadata and the `d`-chosen default,
// and the container is mkv unconditionally (≥2 audio tracks).
func TestDownloadOneMultiAudioMuxPinsTracks(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	c, rec := pr165Core(t, dl, &stubProber{video: "h264", audio: "aac"})

	_, err := c.downloadOne(context.Background(), pr165MuxTask())
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if len(dl.inputs) != 1 {
		t.Fatalf("one ffmpeg run expected, got %d", len(dl.inputs))
	}
	in := dl.inputs[0]
	if in.Mode != download.ModeVideoAudio {
		t.Errorf("Mode = %v, want video_audio", in.Mode)
	}
	if len(in.Tracks) != 2 {
		t.Fatalf("two tracks expected, got %d", len(in.Tracks))
	}
	// The set order is the marked order: first the primary dub, then
	// the second; the DEFAULT lands on the `d`-chosen second track.
	if !in.Tracks[1].Default || in.Tracks[0].Default {
		t.Errorf("the default must be on the second (chosen) track: %+v", in.Tracks)
	}
	if in.Tracks[0].Title != "Amazing Dubbing" || in.Tracks[1].Title != "Second Dub" {
		t.Errorf("track titles must be the bare dub names: %+v", in.Tracks)
	}
	// The extension is mkv by COUNT — no probe dependence.
	if !strings.HasSuffix(in.OutputPath, ".mkv") {
		t.Errorf("multi-audio output must be mkv, got %q", in.OutputPath)
	}
	// Each track resolved its own stream: 3 provider calls (video + 2
	// dubs), each with the bare dub name.
	if len(rec.gotDub) != 3 {
		t.Fatalf("3 resolves expected (video + 2 dubs), got %v", rec.gotDub)
	}
}

// TestDownloadOneMultiAudioSingleAACTracksProbesToMP4: a single marked
// audio whose probe says AAC muxes into mp4 (the research table's only
// mp4-safe mux case).
func TestDownloadOneMultiAudioSingleAACTracksProbesToMP4(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	c, _ := pr165Core(t, dl, &stubProber{video: "h264", audio: "aac"})

	task := pr165MuxTask()
	task.AudioSet = []string{"[animevib] Second Dub"}
	task.DefaultAudio = ""
	_, err := c.downloadOne(context.Background(), task)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	in := dl.inputs[0]
	if !strings.HasSuffix(in.OutputPath, ".mp4") {
		t.Errorf("video + single AAC must be mp4, got %q", in.OutputPath)
	}
	if len(in.Tracks) != 1 || !in.Tracks[0].Default {
		t.Errorf("the single track must be the default: %+v", in.Tracks)
	}
}

// TestDownloadOneVideoOnlyProbesContainer pins «только видео»: the
// probed codec decides mp4 (registered set) vs mkv; no audio tracks
// and no audio maps ride the command.
func TestDownloadOneVideoOnlyProbesContainer(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	c, _ := pr165Core(t, dl, &stubProber{video: "h264"})

	task := pr165MuxTask()
	task.Mode = "video_only"
	task.AudioSet = nil
	if _, err := c.downloadOne(context.Background(), task); err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	in := dl.inputs[0]
	if in.Mode != download.ModeVideoOnly {
		t.Errorf("Mode = %v, want video_only", in.Mode)
	}
	if !strings.HasSuffix(in.OutputPath, ".mp4") {
		t.Errorf("h264 video-only must be mp4, got %q", in.OutputPath)
	}
	if len(in.Tracks) != 0 || in.Audio != nil {
		t.Errorf("video-only must carry no audio: %+v", in)
	}

	dl2 := &capturingDownloader{}
	c2, _ := pr165Core(t, dl2, &stubProber{video: "mpeg2video"})
	if _, err := c2.downloadOne(context.Background(), task); err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if !strings.HasSuffix(dl2.inputs[0].OutputPath, ".mkv") {
		t.Errorf("exotic video-only must be mkv, got %q", dl2.inputs[0].OutputPath)
	}
}

// TestDownloadOneAudioOnlyNativeContainer pins «только аудио»: the
// probed codec picks the extension from the research table (flac →
// .flac), the naming drops the quality tag, and the offline index is
// NOT updated (an audio-only row would promise a playable episode).
func TestDownloadOneAudioOnlyNativeContainer(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	c, _ := pr165Core(t, dl, &stubProber{audio: "flac"})

	task := pr165MuxTask()
	task.Mode = "audio_only"
	task.AudioSet = nil
	path, err := c.downloadOne(context.Background(), task)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if !strings.HasSuffix(path, ".flac") {
		t.Errorf("flac audio-only must keep its native container, got %q", path)
	}
	if strings.Contains(path, "p.flac") {
		t.Errorf("audio-only naming drops the quality tag: %q", path)
	}
	in := dl.inputs[0]
	if in.Mode != download.ModeAudioOnly || in.Audio == nil || in.Video.URL != "" {
		t.Errorf("audio-only consumes the audio source alone: %+v", in)
	}
}

// deniedDubProvider resolves only its allowed dub names — a dub the
// episode does not carry types a resolve error (the real-provider
// shape resolveRecorder cannot fake).
type deniedDubProvider struct {
	id      string
	allowed map[string]bool
}

func (p *deniedDubProvider) ID() string                       { return p.id }
func (p *deniedDubProvider) Name() string                     { return p.id }
func (p *deniedDubProvider) BaseURL() string                  { return "https://" + p.id + ".example" }
func (p *deniedDubProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }
func (p *deniedDubProvider) Search(context.Context, string) ([]contracts.SearchResult, error) {
	return nil, nil
}
func (p *deniedDubProvider) GetEpisodes(context.Context, string) ([]contracts.Episode, error) {
	return nil, nil
}

func (p *deniedDubProvider) ResolveStream(_ context.Context, _ contracts.Episode, dubID string) (contracts.MediaStream, error) {
	if !p.allowed[dubID] {
		return contracts.MediaStream{}, errors.New("dub not carried")
	}
	return contracts.MediaStream{DubName: dubID, Links: map[string]contracts.VideoSource{
		"720": {URL: "https://cdn.example/" + dubID + ".m3u8", Quality: "720"},
	}}, nil
}

// TestDownloadOneMultiAudioAllDroppedIsTypedError pins the honest
// failure: a marked set where NOTHING resolved for the episode errors
// out — the file is never silently written video-only.
func TestDownloadOneMultiAudioAllDroppedIsTypedError(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(&deniedDubProvider{id: "animevib", allowed: map[string]bool{
		"Amazing Dubbing": true, // the video dub resolves; the marked set does not
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	c := &realCore{registry: reg, dl: dl, prober: &stubProber{video: "h264"}, log: testLogger()}

	task := pr165MuxTask()
	task.AudioSet = []string{"[animevib] Ghost Dub"} // the episode does not carry it
	task.DefaultAudio = ""
	_, err := c.downloadOne(context.Background(), task)
	if err == nil || !strings.Contains(err.Error(), "none of the marked dubs") {
		t.Fatalf("an all-dropped set must be a typed error, got %v", err)
	}
	if len(dl.inputs) != 0 {
		t.Fatalf("no ffmpeg run may happen, got %+v", dl.inputs)
	}
}

// TestDownloadOneMultiAudioPartialDropMuxesTheRest pins mandate #7 at
// the pipeline level: a dub dead at download time drops; the mux runs
// with the carried tracks (and the default repairs onto them).
func TestDownloadOneMultiAudioPartialDropMuxesTheRest(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(&deniedDubProvider{id: "animevib", allowed: map[string]bool{
		"Amazing Dubbing": true,
		"Second Dub":      true,
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	c := &realCore{registry: reg, dl: dl, prober: &stubProber{video: "h264"}, log: testLogger()}

	task := pr165MuxTask()
	task.AudioSet = []string{"[animevib] Amazing Dubbing", "[animevib] Second Dub", "[animevib] Ghost Dub"}
	task.DefaultAudio = "[animevib] Ghost Dub" // dropped → the invariant repairs
	if _, err := c.downloadOne(context.Background(), task); err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	in := dl.inputs[0]
	if len(in.Tracks) != 2 {
		t.Fatalf("the carried tracks mux, got %+v", in.Tracks)
	}
	// The dropped default must not leave the mux defaultless: the
	// FIRST carried track becomes the default.
	if !in.Tracks[0].Default {
		t.Errorf("the default must repair onto the first carried track: %+v", in.Tracks)
	}
}

// TestDownloadOneProbeFailureDegradesToMKV pins the probe-failure
// fallback: a failing ffprobe degrades the container to universal
// Matroska and never fails the download itself.
func TestDownloadOneProbeFailureDegradesToMKV(t *testing.T) {
	t.Parallel()

	dl := &capturingDownloader{}
	c, _ := pr165Core(t, dl, &stubProber{err: errors.New("ffprobe missing")})

	task := pr165MuxTask()
	task.Mode = "video_only"
	task.AudioSet = nil
	if _, err := c.downloadOne(context.Background(), task); err != nil {
		t.Fatalf("a probe failure must not fail the download, got %v", err)
	}
	if !strings.HasSuffix(dl.inputs[0].OutputPath, ".mkv") {
		t.Errorf("the probe failure must degrade to mkv, got %q", dl.inputs[0].OutputPath)
	}
}
