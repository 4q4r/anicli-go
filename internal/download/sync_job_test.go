package download

// PR165b T10 tests: RunSyncJob — the two-stage assembly pipeline
// (Stage A video solo → Stage B per-donor WAV→byte-shift→m4a →
// Stage C mkvmerge → Stage D verification), runner-reused through
// the Downloader's seams. Hermetic: every leg is scripted.

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// syncJobSeams scripts the Downloader's legs by argv shape and
// records every call.
type syncJobSeams struct {
	runCalls    [][]string // ffmpeg legs via d.run (silencedetect)
	outCalls    [][]string // ffmpeg legs via d.runOut (video solo, WAV, m4a, RMS)
	mkvCalls    [][]string // mkvmerge
	rmsOut      []byte     // the RMS probe answer
	leadStderr  string     // the silencedetect answer
	verifyFail  bool       // RMS answers silence
	soloCreated []string   // files the fakes created per leg
}

func (s *syncJobSeams) wire(d *Downloader) {
	d.run = func(ctx context.Context, bin string, args []string) (string, error) {
		s.runCalls = append(s.runCalls, args)
		return s.leadStderr, nil
	}
	d.runOut = func(ctx context.Context, bin string, args []string) ([]byte, string, error) {
		s.outCalls = append(s.outCalls, args)
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "silencedetect"):
			return nil, s.leadStderr, nil
		case strings.Contains(joined, "-ss 45.000"):
			if s.verifyFail {
				return nil, "", nil // digital silence
			}
			return s.rmsOut, "", nil
		default:
			// A fixture-creating leg: the LAST argv element is the
			// output path. The WAV decode leg must produce a VALID
			// RIFF file (Stage B shifts its payload byte-exact), big
			// enough for the ±17 s trims; everything else gets a
			// plain fixture file.
			out := args[len(args)-1]
			var werr error
			if strings.Contains(joined, "-f wav") {
				werr = writeMiniWAV(out, 900000)
			} else {
				werr = os.WriteFile(out, []byte("fixture"), 0o600) //nolint:gosec // G306: the fake writes its own temp fixture
			}
			if werr != nil {
				return nil, "", werr
			}
			s.soloCreated = append(s.soloCreated, out)
			return []byte("fixture-stdout"), "", nil
		}
	}
	d.runMKV = func(ctx context.Context, bin string, args []string) (int, string, error) {
		s.mkvCalls = append(s.mkvCalls, args)
		for i, a := range args {
			if a == "-o" && i+1 < len(args) {
				if err := os.WriteFile(args[i+1], []byte("mkv"), 0o600); err != nil { //nolint:gosec // G306: the fake writes its own temp fixture
					return 2, "write failed", nil
				}
			}
		}
		return 0, "", nil
	}
}

// writeMiniWAV writes a valid classic-44 RIFF: PCM s16le stereo at
// 48 kHz, `frames` frames of alternating sample data.
func writeMiniWAV(path string, frames int) error {
	payload := make([]byte, frames*4)
	for i := range frames {
		v := uint16(i % 3000) //nolint:gosec // G115: fixture sample data, far under 2^16
		payload[i*4], payload[i*4+2] = byte(v), byte(v>>8)
		payload[i*4+1], payload[i*4+3] = byte(v>>8), 0
	}
	body := make([]byte, 0, 44+len(payload))
	body = append(body, "RIFF"...)
	body = binary.LittleEndian.AppendUint32(body, uint32(36+len(payload))) //nolint:gosec // G115: fixture sizes are far under 2^32
	body = append(body, "WAVEfmt "...)
	body = binary.LittleEndian.AppendUint32(body, 16)
	body = binary.LittleEndian.AppendUint16(body, 1)     // PCM
	body = binary.LittleEndian.AppendUint16(body, 2)     // stereo
	body = binary.LittleEndian.AppendUint32(body, 48000) // rate
	body = binary.LittleEndian.AppendUint32(body, 48000*4)
	body = binary.LittleEndian.AppendUint16(body, 4)  // block align
	body = binary.LittleEndian.AppendUint16(body, 16) // bits
	body = append(body, "data"...)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(payload))) //nolint:gosec // G115: same fixture bound
	body = append(body, payload...)
	return os.WriteFile(path, body, 0o600) //nolint:gosec // G306: the fake writes its own temp fixture
}

func testSyncJob(t *testing.T) (SyncJob, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "EP_1_DUB_720p.mkv")
	job := SyncJob{
		Video:  contracts.VideoSource{URL: "https://v/video.m3u8"},
		Output: out,
		Title:  "Тайтл — эп 1",
		Donors: []SyncDonor{
			{Dub: "DUB1", Source: contracts.VideoSource{URL: "https://d/dub1.m3u8"}, Lang: "rus", Title: "DUB1", Default: true, Offset: 17.042},
			{Dub: "DUB2", Source: contracts.VideoSource{URL: "https://d/dub2.m3u8"}, Lang: "eng", Title: "DUB2", Offset: 0},
		},
	}
	return job, out
}

func TestRunSyncJobStageSequence(t *testing.T) {
	job, out := testSyncJob(t)
	var seams syncJobSeams
	seams.rmsOut = loudRMSBytes(t)
	d := New(Options{})
	seams.wire(d)

	if err := d.RunSyncJob(context.Background(), job, nil); err != nil {
		t.Fatalf("RunSyncJob: %v", err)
	}

	// Stage sequence: video solo → (WAV → m4a) per donor → mkvmerge.
	wavLegs := make([]string, 0, len(seams.outCalls))
	for _, args := range seams.outCalls {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "-f wav") || strings.Contains(joined, "-c:a aac") || strings.Contains(joined, "-c:v copy") {
			wavLegs = append(wavLegs, joined)
		}
	}
	stages := strings.Join(wavLegs, "\n")
	if !strings.Contains(stages, "-map 0:v:0 -c:v copy") {
		t.Fatalf("stage A (video solo) missing:\n%s", stages)
	}
	if !strings.Contains(stages, "-map 0:a:0 -ar 48000 -ac 2 -c:a pcm_s16le -f wav") {
		t.Fatalf("stage B WAV decode missing:\n%s", stages)
	}
	if strings.Contains(stages, "-ss") {
		t.Fatalf("stage B must decode WHOLE (no -ss):\n%s", stages)
	}
	if !strings.Contains(stages, "-c:a aac -b:a 256k") {
		t.Fatalf("stage B m4a codec missing:\n%s", stages)
	}
	if len(seams.mkvCalls) != 1 || !strings.Contains(strings.Join(seams.mkvCalls[0], " "), "--no-audio") {
		t.Fatalf("stage C mkvmerge missing: %v", seams.mkvCalls)
	}

	// The mkvmerge input carries BOTH donors; the first is default.
	mkvJoined := strings.Join(seams.mkvCalls[0], "|")
	if !strings.Contains(mkvJoined, "--default-track-flag|0:yes") || !strings.Contains(mkvJoined, "--default-track-flag|0:no") {
		t.Fatalf("donor dispositions wrong: %s", mkvJoined)
	}

	// The final file landed at the job's output (rename from .part).
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("final file: %v", err)
	}
	// Every intermediate is cleaned: no *.tmp* files anywhere.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(out), "*tmp*"))
	if len(matches) != 0 {
		t.Fatalf("temp files survived: %v", matches)
	}
}

func TestRunSyncJobVerifyFailRemovesOutput(t *testing.T) {
	job, out := testSyncJob(t)
	var seams syncJobSeams
	seams.verifyFail = true // digital silence at 45 s
	d := New(Options{})
	seams.wire(d)

	err := d.RunSyncJob(context.Background(), job, nil)
	var silent *ErrSilentOutput
	if !errors.As(err, &silent) {
		t.Fatalf("want ErrSilentOutput, got %v", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Fatal("the failed output must be removed")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(out), "*tmp*"))
	if len(matches) != 0 {
		t.Fatalf("temp files survived the failure: %v", matches)
	}
}

// TestRunSyncJobZeroOffsetStillAssembles pins the Stage B
// unification: a donor without confidence still decodes → shifts 0
// frames → m4a, so ONE mkvmerge pass carries every track.
func TestRunSyncJobZeroOffsetStillAssembles(t *testing.T) {
	job, out := testSyncJob(t)
	job.Donors = job.Donors[:1]
	job.Donors[0].Offset = 0
	var seams syncJobSeams
	seams.rmsOut = loudRMSBytes(t)
	d := New(Options{})
	seams.wire(d)

	if err := d.RunSyncJob(context.Background(), job, nil); err != nil {
		t.Fatalf("RunSyncJob: %v", err)
	}
	wavs := 0
	for _, args := range seams.outCalls {
		if strings.Contains(strings.Join(args, " "), "-f wav") {
			wavs++
		}
	}
	if wavs != 1 {
		t.Fatalf("wav decodes = %d, want 1 (the zero-offset donor still assembles)", wavs)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("final file: %v", err)
	}
}

func TestRunSyncJobProgressTicks(t *testing.T) {
	job, _ := testSyncJob(t)
	var seams syncJobSeams
	seams.rmsOut = loudRMSBytes(t)
	d := New(Options{})
	seams.wire(d)

	var ticks []float64
	if err := d.RunSyncJob(context.Background(), job, func(p float64) { ticks = append(ticks, p) }); err != nil {
		t.Fatalf("RunSyncJob: %v", err)
	}
	for _, want := range []float64{10, 30, 90, 100} {
		found := false
		for _, p := range ticks {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("progress ticks %v missing %v", ticks, want)
		}
	}
}

// TestRunSyncJobLeadExpectation: the verification's expected lead is
// the FIRST offset donor's |offset| when negative (pad) — a +17 trim
// expects ≈0 and a −17 pad expects ≈17. Scripted via the silencedetect
// answer: a +offset job with a 17-second lead FAILS (trim geometry).
func TestRunSyncJobLeadExpectation(t *testing.T) {
	job, _ := testSyncJob(t)
	var seams syncJobSeams
	seams.rmsOut = loudRMSBytes(t)
	seams.leadStderr = "[silencedetect] silence_start: 0\n[silencedetect] silence_end: 17 | silence_duration: 17\n"
	d := New(Options{})
	seams.wire(d)

	err := d.RunSyncJob(context.Background(), job, nil)
	var silent *ErrSilentOutput
	if !errors.As(err, &silent) || silent.Check != "lead" {
		t.Fatalf("trim job with a 17-s lead must fail on lead, got %v", err)
	}

	// The pad twin: a negative offset expects the lead.
	job, _ = testSyncJob(t)
	job.Donors[0].Offset = -17
	seams.leadStderr = "[silencedetect] silence_start: 0\n[silencedetect] silence_end: 17.4 | silence_duration: 17.4\n"
	if err := d.RunSyncJob(context.Background(), job, nil); err != nil {
		t.Fatalf("pad job with a matching lead must pass: %v", err)
	}
}

func loudRMSBytes(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 5*8000*2)
	for i := range len(b) / 2 {
		b[i*2], b[i*2+1] = 120, 0
	}
	return b
}

// TestSyncExpectedLeadFirstDonorOnly (CodeRabbit #4): the expected
// lead derives ONLY from donors[0] — that is the track
// VerifyAssembled measures (0:a:0). A later donor's pad must not set
// the expectation: donors[0] unshifted + donors[1] padded −10 s has a
// ≈0 file lead, and expecting 10 s deleted VALID episodes.
func TestSyncExpectedLeadFirstDonorOnly(t *testing.T) {
	cases := []struct {
		name   string
		donors []SyncDonor
		want   time.Duration
	}{
		{"empty", nil, 0},
		{"first_zero_second_padded", []SyncDonor{{Offset: 0}, {Offset: -10}}, 0},
		{"first_padded", []SyncDonor{{Offset: -10}}, 10 * time.Second},
		{"first_trim", []SyncDonor{{Offset: 17.042}, {Offset: -10}}, 0},
		{"all_zero", []SyncDonor{{Offset: 0}, {Offset: 0}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncExpectedLead(tc.donors); got != tc.want {
				t.Fatalf("syncExpectedLead = %v, want %v", got, tc.want)
			}
		})
	}
}
