package tui

// PR165b T10 tests: the two-stage downloadOne — the analysis fork,
// the assembly hand-off, the degradation matrix (§1.1) and the report
// rendering. Fakes everywhere: no real ffmpeg/mkvmerge here (the
// `live` proofs carry the real binaries).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/audsync"
	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/download"
	"github.com/an0nx/anicli-go/internal/i18n"
	"github.com/an0nx/anicli-go/internal/providers"
)

// fakeSyncAnalyzer scripts Reference and per-ANALYSIS-CALL offsets
// (the pipeline legs share one resolved URL, so the scripting is
// queue-based by call order).
type fakeSyncAnalyzer struct {
	refErr       error
	offsetsQueue []audsync.SyncOffset
	errsQueue    []error
	references   []string // the reference sources requested, in order
	analyzed     []string // the donor sources analyzed, in order
}

func (f *fakeSyncAnalyzer) Reference(_ context.Context, video contracts.VideoSource) (audsync.Reference, error) {
	f.references = append(f.references, video.URL)
	if f.refErr != nil {
		return audsync.Reference{}, f.refErr
	}
	return audsync.Reference{FPS: 24}, nil
}

func (f *fakeSyncAnalyzer) Offset(_ context.Context, _ audsync.Reference, dub contracts.VideoSource) (audsync.SyncOffset, error) {
	f.analyzed = append(f.analyzed, dub.URL)
	if len(f.errsQueue) > 0 {
		err := f.errsQueue[0]
		f.errsQueue = f.errsQueue[1:]
		return audsync.SyncOffset{}, err
	}
	if len(f.offsetsQueue) > 0 {
		off := f.offsetsQueue[0]
		f.offsetsQueue = f.offsetsQueue[1:]
		return off, nil
	}
	return audsync.SyncOffset{}, nil // measured clean zero
}

// fakeSyncAssembler captures the assembly jobs and emits the
// progress tick the real pipeline would.
type fakeSyncAssembler struct {
	jobs []download.SyncJob
	err  error
}

func (f *fakeSyncAssembler) RunSyncJob(_ context.Context, job download.SyncJob, progress func(float64)) error {
	f.jobs = append(f.jobs, job)
	if progress != nil {
		progress(100)
	}
	return f.err
}

// pr165bSyncCore builds a two-provider core (animevib video, anilib
// dubs) with the sync seams injected and the download root pinned to
// a temp dir (the legacy index path writes there — never the user's
// real data dir).
func pr165bSyncCore(t *testing.T, dl *capturingDownloader, analyzer audioSyncAnalyzer, assembler syncAssembler, mkvmergePresent bool) (*realCore, *resolveRecorder) {
	t.Helper()
	rec := &resolveRecorder{id: "animevib"}
	dubRec := &resolveRecorder{id: "anilib"}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(rec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(dubRec); err != nil {
		t.Fatalf("register dubs: %v", err)
	}
	return &realCore{
		registry:        reg,
		dl:              dl,
		prober:          &stubProber{video: "h264", audio: "aac"},
		log:             testLogger(),
		analyzer:        analyzer,
		assembler:       assembler,
		mkvmergePresent: func() bool { return mkvmergePresent },
		settings:        config.Settings{Download: config.Download{Dir: t.TempDir()}},
	}, rec
}

// pr165bSyncTask: the video from animevib, the marked dubs from
// anilib (cross-provider — the analysis gate), sync ON.
func pr165bSyncTask() DownloadTask {
	return DownloadTask{
		ProviderID: "animevib",
		DubID:      "[animevib] Amazing Dubbing",
		Mode:       "video_audio",
		AudioSync:  true,
		AudioSet:   []string{"[anilib] DUB1", "[anilib] DUB2"},
		Episode: contracts.Episode{
			Num:   "3",
			RawID: recorderMergedRawID,
			RawEmbeds: map[string][]string{
				"DUB1": {"u1"},
				"DUB2": {"u2"},
			},
		},
	}
}

func TestSyncAssemblyRunsOnOffset(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{offsetsQueue: []audsync.SyncOffset{
		{Seconds: 17.042, Confidence: 5, WindowsAgree: 3},
		{Seconds: 0}, // DUB2 measured clean zero → frames=0, no note
	}}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	path, _, notes, err := c.downloadOne(context.Background(), pr165bSyncTask(), nil)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	// The assembly branch: the single-pass mux NEVER ran.
	if len(dl.inputs) != 0 {
		t.Fatalf("assembly episode must not single-pass mux, got %v", dl.inputs)
	}
	if len(assembler.jobs) != 1 {
		t.Fatalf("one assembly job expected, got %d", len(assembler.jobs))
	}
	job := assembler.jobs[0]
	// Sync outputs are ALWAYS .mkv.
	if !strings.HasSuffix(job.Output, ".mkv") {
		t.Fatalf("sync output = %q, want .mkv", job.Output)
	}
	// Both donors assemble; the measured one carries its offset, the
	// clean-zero one carries 0 (frames=0 — still one mkvmerge pass).
	if len(job.Donors) != 2 {
		t.Fatalf("donors = %d, want 2", len(job.Donors))
	}
	if job.Donors[0].Offset != 17.042 || job.Donors[1].Offset != 0 {
		t.Fatalf("offsets = %v/%v, want 17.042/0", job.Donors[0].Offset, job.Donors[1].Offset)
	}
	if !job.Donors[0].Default {
		t.Fatalf("the first donor must carry the default disposition: %+v", job.Donors)
	}
	// Notes: applied for the measured dub; the clean zero renders no
	// note («уже в синхроне» is not a note).
	if len(notes) != 1 || notes[0].Kind != SyncApplied || notes[0].Seconds != 17.042 {
		t.Fatalf("notes = %+v, want one applied", notes)
	}
	_ = path
}

// TestSyncNoOffsetsFallsBackToSinglePass: every donor measures a
// clean zero — today's multi-audio mux runs (argv identical), the
// assembly never starts, and «уже в синхроне» renders no note.
func TestSyncNoOffsetsFallsBackToSinglePass(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{} // clean zeros
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	path, _, notes, err := c.downloadOne(context.Background(), pr165bSyncTask(), nil)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if len(assembler.jobs) != 0 {
		t.Fatalf("no assembly expected on clean zeros, got %v", assembler.jobs)
	}
	if len(dl.inputs) != 1 {
		t.Fatalf("the single-pass mux must run, got %d", len(dl.inputs))
	}
	if len(notes) != 0 {
		t.Fatalf("clean zeros render no notes, got %v", notes)
	}
	// Today's container rule for a 2-track mux: mkv by count.
	if !strings.HasSuffix(path, ".mkv") {
		t.Fatalf("the degraded 2-track mux keeps today's container: %q", path)
	}
}

// TestSyncUncertainNoteOnAnalysisError: the analysis errors for every
// donor — today's path + an uncertain note per dub.
func TestSyncUncertainNoteOnAnalysisError(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{errsQueue: []error{
		errors.New("provider down"), errors.New("provider down"),
	}}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	_, _, notes, err := c.downloadOne(context.Background(), pr165bSyncTask(), nil)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if len(assembler.jobs) != 0 {
		t.Fatalf("no assembly on failed analysis, got %v", assembler.jobs)
	}
	if len(dl.inputs) != 1 {
		t.Fatalf("the single-pass mux must run, got %d", len(dl.inputs))
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %+v, want one uncertain per dub", notes)
	}
	for _, n := range notes {
		if n.Kind != SyncUncertain {
			t.Fatalf("note %+v, want uncertain", n)
		}
	}
}

// TestSyncUnavailableWhenMkvmergeAbsent: the refused install
// degrades to today's path with the honest unavailable note (the
// PR148 doctrine — never an abort).
func TestSyncUnavailableWhenMkvmergeAbsent(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{offsetsQueue: []audsync.SyncOffset{
		{Seconds: 17.042, Confidence: 5, WindowsAgree: 3},
	}}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, false)

	_, _, notes, err := c.downloadOne(context.Background(), pr165bSyncTask(), nil)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if len(assembler.jobs) != 0 {
		t.Fatalf("no assembly without mkvmerge, got %v", assembler.jobs)
	}
	if len(dl.inputs) != 1 {
		t.Fatalf("the single-pass mux must run, got %d", len(dl.inputs))
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %+v, want unavailable per dub", notes)
	}
	for _, n := range notes {
		if n.Kind != SyncUnavailable {
			t.Fatalf("note %+v, want unavailable", n)
		}
	}
}

// TestSyncFatalOnAssemblyError: the assembly/verification contract —
// a Stage failure fails the EPISODE (the .part never ships).
func TestSyncFatalOnAssemblyError(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{offsetsQueue: []audsync.SyncOffset{
		{Seconds: 17.042, Confidence: 5, WindowsAgree: 3},
	}}
	assembler := &fakeSyncAssembler{err: errors.New("mkvmerge exploded")}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	_, _, notes, err := c.downloadOne(context.Background(), pr165bSyncTask(), nil)
	if err == nil || !strings.Contains(err.Error(), "mkvmerge exploded") {
		t.Fatalf("assembly failure must fail the episode, got %v", err)
	}
	if len(dl.inputs) != 0 {
		t.Fatalf("a failed assembly must not fall through to a silent mux: %v", dl.inputs)
	}
	if len(notes) == 0 || notes[0].Kind != SyncApplied {
		t.Fatalf("the applied note still rides the failure: %+v", notes)
	}
}

// TestSyncSyncOffGatesEverything: AudioSync=false — no analysis, no
// assembly, no notes, today's mux.
func TestSyncSyncOffGatesEverything(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	task := pr165bSyncTask()
	task.AudioSync = false
	if _, _, notes, err := c.downloadOne(context.Background(), task, nil); err != nil || len(notes) != 0 {
		t.Fatalf("sync-off notes/err = %v/%v", notes, err)
	}
	if len(analyzer.references) != 0 {
		t.Fatal("sync-off must not analyze")
	}
	if len(dl.inputs) != 1 {
		t.Fatalf("the single-pass mux must run, got %d", len(dl.inputs))
	}
}

// TestSyncLegacyPathIndexes: the legacy separate-audio episode goes
// through the assembly too, and the synchronized .mkv keeps the
// offline-index contract (the legacy shape indexes as today).
func TestSyncLegacyPathIndexes(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{offsetsQueue: []audsync.SyncOffset{
		{Seconds: -0.875, Confidence: 5, WindowsAgree: 3},
	}}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	task := DownloadTask{
		ProviderID: "animevib",
		// The DubID composition the separate-audio parser reads:
		// the AUDIO dub rides BEFORE the \x1f+ separator (the parser's
		// before-part convention), the video dub after.
		DubID:     "[anilib] DUB1\x1f+[animevib] Amazing Dubbing",
		Mode:      "video_audio",
		AudioSync: true,
		Episode: contracts.Episode{
			Num:   "3",
			RawID: recorderMergedRawID,
			RawEmbeds: map[string][]string{
				"Amazing Dubbing": {"u1"},
			},
		},
	}
	path, _, notes, err := c.downloadOne(context.Background(), task, nil)
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if len(assembler.jobs) != 1 {
		t.Fatalf("the legacy pair must assemble, got %v", assembler.jobs)
	}
	if len(assembler.jobs[0].Donors) != 1 || assembler.jobs[0].Donors[0].Offset != -0.875 {
		t.Fatalf("legacy donor = %+v", assembler.jobs[0].Donors)
	}
	if !strings.HasSuffix(path, ".mkv") {
		t.Fatalf("sync output = %q, want .mkv", path)
	}
	if len(notes) != 1 || notes[0].Kind != SyncApplied || notes[0].Seconds != -0.875 {
		t.Fatalf("notes = %+v, want applied −0.875", notes)
	}
}

// TestSyncNotesRender pins the report suffix: applied offsets render
// signed 3-decimal, uncertain and unavailable their typed forms, and
// the settle row carries the suffix.
func TestSyncNotesRender(t *testing.T) {
	t.Cleanup(func() { _ = i18n.Init("en") })
	if err := i18n.Init("ru"); err != nil {
		t.Fatalf("Init(ru): %v", err)
	}
	suffix := renderSyncSuffix([]SyncNote{
		{Dub: "DUB", Seconds: 17.042, Kind: SyncApplied},
		{Dub: "DUB2", Kind: SyncUncertain},
		{Dub: "DUB3", Kind: SyncUnavailable},
	})
	for _, want := range []string{"синхронизация:", "DUB +17.042с", "DUB2: нет уверенности", "DUB3: синхронизация недоступна"} {
		if !strings.Contains(suffix, want) {
			t.Fatalf("suffix %q missing %q", suffix, want)
		}
	}
	if renderSyncSuffix(nil) != "" {
		t.Fatal("no notes → no suffix")
	}

	msg := downloadSettledMsg{
		count: 1, total: 1,
		report: []downloadEpisodeReport{{
			Episode: "2", Dub: "[animevib] Amazing Dubbing", Path: "/dl/ep2.mkv",
			SyncNotes: []SyncNote{{Dub: "DUB", Seconds: 17.042, Kind: SyncApplied}},
		}},
	}
	status := renderDownloadSettle(msg)
	if !strings.Contains(status, "синхронизация: DUB +17.042с") {
		t.Fatalf("the settle row must carry the sync suffix:\n%s", status)
	}
}

// TestSyncProgressTicksReachTheManager: the background runner passes
// the manager's progress callback into the assembly (10/30/90/100)
// instead of dropping it.
func TestSyncProgressTicksReachTheManager(t *testing.T) {
	t.Parallel()
	dl := &capturingDownloader{}
	analyzer := &fakeSyncAnalyzer{offsetsQueue: []audsync.SyncOffset{
		{Seconds: 17.042, Confidence: 5, WindowsAgree: 3},
	}}
	assembler := &fakeSyncAssembler{}
	c, _ := pr165bSyncCore(t, dl, analyzer, assembler, true)

	var ticks []float64
	_, _, _, err := c.downloadOne(context.Background(), pr165bSyncTask(), func(p float64) {
		ticks = append(ticks, p)
	})
	if err != nil {
		t.Fatalf("downloadOne: %v", err)
	}
	if len(ticks) == 0 || ticks[len(ticks)-1] != 100 {
		t.Fatalf("progress ticks %v, want the assembly ladder ending at 100", ticks)
	}
}
