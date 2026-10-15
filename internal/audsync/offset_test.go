package audsync

// PR173 v2 tests: the Analyzer's exec orchestration against a
// scripted fake (never a real ffmpeg here — the real binaries are the
// `live` tag's business). The fake serves ffprobe -show_streams
// JSON, per-track ASS extraction replies, 8 kHz dual-mono renders of
// analysis-rate timelines keyed by URL (+ optional -map), and honest
// 6:1 decimations of LOCAL .wav files (the closed loop's re-decode).

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// speech8k builds an analysis-rate dual-mono timeline: BROADBAND
// noise bursts (LCG white noise, 1 s on / 2 s off) under a syllable-
// rate 0.25 s amplitude modulation. Broadband is load-bearing (the
// v1 live fixture's lesson); the syllable AM gives consecutive
// envelope bins genuinely different values (≈5-bin wavelength), so
// the correlation's discrete argmax lands on the nearest bin to the
// true lag the way real speech envelopes do — a slow AM would leave
// adjacent bins ≈identical and let the sub-bin window admixture tip
// the peak a bin off.
func speech8k(seconds float64) []float64 {
	n := int(seconds * SampleRate)
	x := make([]float64, n)
	state := int64(246813579)
	for i := range x {
		state = (state*1103515245 + 12345) % (1 << 31)
		if i%(3*SampleRate) < SampleRate {
			am := 0.55 + 0.45*math.Sin(2*math.Pi*float64(i)/(0.25*SampleRate))
			x[i] = (float64(state)/math.Pow(2, 31) - 0.5) * 2 * 9000 * am
		}
	}
	return x
}

// delayed8k shifts a timeline by seconds (positive — delayed, the
// head goes silent; negative — advanced, the tail drops).
func delayed8k(src []float64, seconds float64) []float64 {
	shift := int(math.Round(seconds * SampleRate))
	out := make([]float64, len(src))
	if shift >= 0 {
		copy(out[shift:], src[:len(src)-shift])
	} else {
		copy(out, src[-shift:])
	}
	return out
}

// richASS renders a subtitle track with n usable dialogues (the
// hierarchy's MinSubIntervals floor must clear).
func richASS(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "Dialogue: 0,0:%02d:%02d.00,0:%02d:%02d.90,Default,,0,0,0,,Реплика номер %d\n",
			(i*3)/60, (i*3)%60, ((i*3)+1)/60, ((i*3)+1)%60, i)
	}
	return b.String()
}

// writeSynthWAV renders a 48 kHz stereo s16le WAV whose audio is the
// 6×-held content timeline (decimation recovers it exactly).
func writeSynthWAV(t *testing.T, path string, content []float64) {
	t.Helper()
	dataSize := len(content) * 6 * 4
	buf := make([]byte, 44, 44+dataSize)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataSize)) //nolint:gosec // G115: fixture sizes are far under 2^32
	copy(buf[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)      //nolint:gosec // G115: fixture constant
	binary.LittleEndian.PutUint16(buf[20:], 1)       // PCM
	binary.LittleEndian.PutUint16(buf[22:], 2)       // stereo
	binary.LittleEndian.PutUint32(buf[24:], 48000)   //nolint:gosec // G115: fixture constant
	binary.LittleEndian.PutUint32(buf[28:], 48000*4) //nolint:gosec // G115: fixture constant
	binary.LittleEndian.PutUint16(buf[32:], 4)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataSize)) //nolint:gosec // G115: fixture sizes are far under 2^32
	frame := make([]byte, 4)
	for _, v := range content {
		for range 6 {
			binary.LittleEndian.PutUint16(frame[0:], uint16(int16(clampS16(v)))) //nolint:gosec // G115: the s16 reinterpretation under test
			binary.LittleEndian.PutUint16(frame[2:], uint16(int16(clampS16(v)))) //nolint:gosec // G115: same
			buf = append(buf, frame...)
		}
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil { //nolint:gosec // G306: the test writes its own temp wav
		t.Fatal(err)
	}
}

// decimateWAV reads a local WAV and decodes it the way the fake's
// ffmpeg leg would: every 6th frame of the 48 kHz payload (48k → 8k),
// rendered as s16le stereo bytes.
func decimateWAV(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the fake decodes the caller's temp wav
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	layout, err := readWAVLayout(f)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, layout.DataSize)
	if _, err := f.ReadAt(payload, layout.DataOff); err != nil {
		return nil, err
	}
	var out []byte
	for i := 0; i+3 < len(payload); i += 4 {
		if (i/4)%6 != 0 {
			continue
		}
		out = append(out, payload[i:i+4]...)
	}
	return out, nil
}

// fakeExec scripts the v2 exec surface. Timelines are keyed by
// "URL" (bare decodes) or "URL|0:a:N" (per-track reference decodes).
type fakeExec struct {
	calls       [][]string
	streamsJSON string
	assByTrack  map[int]string
	timelines   map[string][]float64
	sleepURL    string
	slowURL     string
	slowDur     time.Duration
}

func (f *fakeExec) exec(ctx context.Context, bin string, args []string) ([]byte, string, error) {
	argv := append([]string{bin}, args...)
	f.calls = append(f.calls, argv)
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "-show_streams"):
		if f.streamsJSON == "" {
			return nil, "", errors.New("probe failed")
		}
		return []byte(f.streamsJSON), "", nil
	case strings.Contains(joined, "-f ass pipe:"):
		for i, a := range args {
			if a == "-map" && strings.HasPrefix(args[i+1], "0:s:") {
				var idx int
				_, _ = fmt.Sscanf(args[i+1], "0:s:%d", &idx)
				if ass, ok := f.assByTrack[idx]; ok {
					return []byte(ass), "", nil
				}
				return nil, "", nil // an absent track: empty stdout (the `?` semantics)
			}
		}
		return nil, "", nil
	case strings.Contains(joined, "-f s16le pipe:"):
		url := urlOf(args)
		if url == f.sleepURL {
			<-ctx.Done()
			return nil, "", ctx.Err()
		}
		if url == f.slowURL {
			select {
			case <-time.After(f.slowDur):
			case <-ctx.Done():
				return nil, "", ctx.Err()
			}
		}
		if strings.HasSuffix(url, ".wav") {
			out, err := decimateWAV(url)
			if err != nil {
				return nil, "", err
			}
			return out, "", nil
		}
		key := url
		if m := mapOf(args); m != "" {
			key = url + "|" + m
		}
		tl, ok := f.timelines[key]
		if !ok {
			tl = f.timelines[url]
		}
		return dualMonoS16(tl), "", nil
	}
	return nil, "unscripted argv: " + joined, errors.New("unscripted argv")
}

func urlOf(args []string) string {
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func mapOf(args []string) string {
	for i, a := range args {
		if a == "-map" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// dualMonoS16 renders an analysis-rate timeline as s16le stereo
// (L = R = v: mid = v, side = 0 — the vocal isolation passes the
// content through).
func dualMonoS16(tl []float64) []byte {
	out := make([]byte, 0, len(tl)*4)
	var frame [4]byte
	for _, v := range tl {
		binary.LittleEndian.PutUint16(frame[0:], uint16(int16(clampS16(v)))) //nolint:gosec // G115: the s16 reinterpretation under test
		binary.LittleEndian.PutUint16(frame[2:], uint16(int16(clampS16(v)))) //nolint:gosec // G115: same
		out = append(out, frame[:]...)
	}
	return out
}

// streamsJSON builds an -show_streams reply: one video, then the
// (type, lang) pairs in container order.
func streamsJSON(tracks ...[2]string) string {
	var b strings.Builder
	b.WriteString(`{"streams": [{"index": 0, "codec_type": "video"}`)
	for i, tr := range tracks {
		fmt.Fprintf(&b, `, {"index": %d, "codec_type": %q, "tags": {"language": %q}}`, i+1, tr[0], tr[1])
	}
	b.WriteString("]}")
	return b.String()
}

func newFakeAnalyzer(fe *fakeExec) *Analyzer {
	return New(Options{Exec: fe.exec, Budget: 5 * time.Second, DecodeBudget: 5 * time.Second})
}

// --- the reference hierarchy -------------------------------------------------

func TestReferenceSubsWinHierarchy(t *testing.T) {
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"subtitle", "rus"}, [2]string{"audio", "jpn"}),
		assByTrack:  map[int]string{0: richASS(15)},
		timelines:   map[string][]float64{},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if ref.Kind != RefSubs {
		t.Fatalf("kind = %v, want RefSubs (subtitles outrank any audio)", ref.Kind)
	}
	if envelopeActivity(ref.Envelope) == 0 {
		t.Fatal("sub reference envelope is empty")
	}
	if ref.MinLag != SubLagMin || ref.MaxLag != SubLagMax {
		t.Fatalf("band = [%v, %v], want the subtitle band", ref.MinLag, ref.MaxLag)
	}
	// Single-subtitle container: only s:0 fires (the hierarchy skips
	// absent tracks instead of spawning a doomed ffmpeg).
	if joined := assMaps(fe.calls); len(joined) != 1 || joined[0] != "0:s:0?" {
		t.Fatalf("ass extraction = %v, want [0:s:0?]", joined)
	}
}

// TestReferenceTwoSubsKeepsMostIntervals: the script's candidate
// order (1, 0) with both tracks present, and the most-intervals
// track wins even though it is not first.
func TestReferenceTwoSubsKeepsMostIntervals(t *testing.T) {
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"subtitle", "rus"}, [2]string{"subtitle", "rus"}, [2]string{"audio", "jpn"}),
		assByTrack: map[int]string{
			0: richASS(4),  // sparse (signs-only class) — 4 usable
			1: richASS(15), // the dialogue track — wins
		},
		timelines: map[string][]float64{},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if ref.Kind != RefSubs || envelopeActivity(ref.Envelope) == 0 {
		t.Fatalf("kind = %v, want RefSubs from the richer track", ref.Kind)
	}
	if joined := assMaps(fe.calls); len(joined) != 2 || joined[0] != "0:s:1?" || joined[1] != "0:s:0?" {
		t.Fatalf("ass extraction order = %v, want [0:s:1? 0:s:0?]", joined)
	}
}

// assMaps collects the -map 0:s:N? args in call order.
func assMaps(calls [][]string) []string {
	joined := make([]string, 0, 2)
	for _, call := range calls {
		for i, arg := range call {
			if arg == "-map" && strings.HasPrefix(call[i+1], "0:s:") {
				joined = append(joined, call[i+1])
			}
		}
	}
	return joined
}

func TestReferenceJpnAudioWhenNoSubs(t *testing.T) {
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "rus"}, [2]string{"audio", "jpn"}),
		timelines:   map[string][]float64{"https://v/video.m3u8|0:a:1": speech8k(300)},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if ref.Kind != RefAudio {
		t.Fatalf("kind = %v, want RefAudio", ref.Kind)
	}
	if ref.MinLag != AudioLagMin || ref.MaxLag != AudioLagMax {
		t.Fatalf("band = [%v, %v], want the audio band", ref.MinLag, ref.MaxLag)
	}
	// The jpn track is the container's second audio → -map 0:a:1.
	decoded := ""
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "-f s16le pipe:") {
			decoded = mapOf(call)
		}
	}
	if decoded != "0:a:1" {
		t.Fatalf("reference decode map = %q, want 0:a:1", decoded)
	}
}

func TestReferencePrefersLangDifferringFromDonor(t *testing.T) {
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "rus"}, [2]string{"audio", "eng"}),
		timelines: map[string][]float64{
			"https://v/video.m3u8|0:a:0": speech8k(300),
			"https://v/video.m3u8|0:a:1": speech8k(300),
		},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	_ = ref
	decoded := ""
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "-f s16le pipe:") {
			decoded = mapOf(call)
		}
	}
	if decoded != "0:a:1" {
		t.Fatalf("reference decode map = %q, want 0:a:1 (eng differs from the rus donor)", decoded)
	}
	// The mirror case: an eng donor takes the rus track.
	fe2 := &fakeExec{
		streamsJSON: fe.streamsJSON,
		timelines:   fe.timelines,
	}
	a2 := newFakeAnalyzer(fe2)
	if _, err := a2.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "eng"); err != nil {
		t.Fatalf("Reference(mirror): %v", err)
	}
	for _, call := range fe2.calls {
		if strings.Contains(strings.Join(call, " "), "-f s16le pipe:") {
			if m := mapOf(call); m != "0:a:0" {
				t.Fatalf("mirror decode map = %q, want 0:a:0 (rus differs from the eng donor)", m)
			}
		}
	}
}

func TestReferenceSkipsSilentCandidates(t *testing.T) {
	// jpn decodes to silence → the next non-silent candidate wins.
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}, [2]string{"audio", "rus"}),
		timelines:   map[string][]float64{"https://v/video.m3u8|0:a:1": speech8k(300)},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if ref.Kind != RefAudio || envelopeActivity(ref.Envelope) == 0 {
		t.Fatalf("verdict = kind %v activity %d; want the non-silent rus track", ref.Kind, envelopeActivity(ref.Envelope))
	}
}

func TestReferenceErrWhenNothingUsable(t *testing.T) {
	// A video-only stream: no subs, no audio.
	fe := &fakeExec{streamsJSON: streamsJSON()}
	a := newFakeAnalyzer(fe)
	_, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if !errors.Is(err, ErrNoReference) {
		t.Fatalf("want ErrNoReference, got %v", err)
	}
	// Every audio candidate silent → the same verdict.
	fe2 := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		timelines:   map[string][]float64{},
	}
	a2 := newFakeAnalyzer(fe2)
	if _, err := a2.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus"); !errors.Is(err, ErrNoReference) {
		t.Fatalf("want ErrNoReference for all-silent candidates, got %v", err)
	}
	// A failed stream probe wraps ErrNoReference (never fails a download).
	fe3 := &fakeExec{}
	a3 := newFakeAnalyzer(fe3)
	if _, err := a3.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus"); !errors.Is(err, ErrNoReference) {
		t.Fatalf("want ErrNoReference on probe failure, got %v", err)
	}
}

func TestReferenceSparseSubsFallThroughToAudio(t *testing.T) {
	// A signs-only ASS (4 intervals < MinSubIntervals) is not a usable
	// reference — the jpn track takes over.
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"subtitle", "rus"}, [2]string{"audio", "jpn"}),
		assByTrack:  map[int]string{0: richASS(4)},
		timelines:   map[string][]float64{"https://v/video.m3u8|0:a:0": speech8k(300)},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if ref.Kind != RefAudio {
		t.Fatalf("kind = %v, want RefAudio (sparse subs fall through)", ref.Kind)
	}
}

// --- the donor offset ---------------------------------------------------------

func TestOffsetV2VerdictExact(t *testing.T) {
	refTL := speech8k(300)
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		timelines: map[string][]float64{
			"https://v/video.m3u8|0:a:0": refTL,
			"https://d/dub.m3u8":         delayed8k(refTL, 2),
		},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	off, err := a.Offset(context.Background(), ref, contracts.VideoSource{URL: "https://d/dub.m3u8"})
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off.Seconds != 2.0 {
		t.Fatalf("offset = %v, want exactly 2.0", off.Seconds)
	}
}

func TestOffsetV2SilentDonorIsNoConfidence(t *testing.T) {
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		timelines: map[string][]float64{
			"https://v/video.m3u8|0:a:0": speech8k(300),
		},
	}
	a := newFakeAnalyzer(fe)
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	_, err = a.Offset(context.Background(), ref, contracts.VideoSource{URL: "https://d/dub.m3u8"})
	if !errors.Is(err, ErrNoConfidence) {
		t.Fatalf("want ErrNoConfidence for a silent donor, got %v", err)
	}
}

// TestOffsetBudgetKillsDecode: a hung donor decode dies within the
// DecodeBudget as a typed ErrNoConfidence wrap.
func TestOffsetBudgetKillsDecode(t *testing.T) {
	const dubURL = "https://d/dub.m3u8"
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		timelines:   map[string][]float64{"https://v/video.m3u8|0:a:0": speech8k(300)},
		sleepURL:    dubURL,
	}
	a := New(Options{Exec: fe.exec, Budget: 200 * time.Millisecond, DecodeBudget: 200 * time.Millisecond})
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.Offset(context.Background(), ref, contracts.VideoSource{URL: dubURL})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoConfidence) {
			t.Fatalf("want ErrNoConfidence wrap, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Offset ignored the decode budget")
	}
}

// TestReferenceDecodeBudgetKillsDecode: a hung reference decode dies
// within the DecodeBudget as a typed ErrNoReference wrap.
func TestReferenceDecodeBudgetKillsDecode(t *testing.T) {
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		sleepURL:    "https://v/video.m3u8",
	}
	a := New(Options{Exec: fe.exec, DecodeBudget: 200 * time.Millisecond})
	done := make(chan error, 1)
	go func() {
		_, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoReference) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want ErrNoReference + deadline wrap, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reference ignored the decode budget")
	}
}

// TestOffsetDecodeBudgetLetsSlowDecodeSucceed: the donor decode is
// the slow leg — beyond the per-pair Budget, within the DecodeBudget —
// and the verdict stays exact.
func TestOffsetDecodeBudgetLetsSlowDecodeSucceed(t *testing.T) {
	const dubURL = "https://d/dub.m3u8"
	refTL := speech8k(300)
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		timelines: map[string][]float64{
			"https://v/video.m3u8|0:a:0": refTL,
			dubURL:                       delayed8k(refTL, 2),
		},
		slowURL: dubURL,
		slowDur: 500 * time.Millisecond,
	}
	a := New(Options{Exec: fe.exec, Budget: 200 * time.Millisecond, DecodeBudget: 5 * time.Second})
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	off, err := a.Offset(context.Background(), ref, contracts.VideoSource{URL: dubURL})
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if off.Seconds != 2.0 {
		t.Fatalf("offset = %v, want exactly 2.0 (identical to the undelayed pair)", off.Seconds)
	}
}

// --- the closed loop ------------------------------------------------------------

// closedLoopFixture wires a fake world where the reference is an
// audio track and the donor is a LOCAL pipeline WAV carrying the same
// content delayed by lagSeconds.
func closedLoopFixture(t *testing.T, lagSeconds float64) (*Analyzer, *fakeExec, Reference, string, string) {
	t.Helper()
	dir := t.TempDir()
	content := speech8k(60)
	refURL := "https://v/video.m3u8"
	fe := &fakeExec{
		streamsJSON: streamsJSON([2]string{"audio", "jpn"}),
		timelines:   map[string][]float64{refURL + "|0:a:0": content},
	}
	a := New(Options{Exec: fe.exec, Budget: 5 * time.Second, DecodeBudget: 5 * time.Second})
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: refURL}, "rus")
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	raw := filepath.Join(dir, "donor.wav")
	writeSynthWAV(t, raw, delayed8k(content, lagSeconds))
	shifted := filepath.Join(dir, "donor.shifted.wav")
	return a, fe, ref, raw, shifted
}

// The hermetic closed-loop tests pin the BRANCH CONTRACT: the first
// shift is always lag + priming; the re-measured residual decides one
// correction pass; the applied value and the final file geometry
// follow. WHICH bin the residual lands on for the synthetic fixture
// is a sub-bin resampler-fidelity question the fake cannot answer
// honestly (the real-ffmpeg `live` proof owns it) — so the tests read
// the fixture's deterministic residual first and assert the loop's
// arithmetic against it.

// TestClosedLoopBranchContract: for both donor polarities the loop's
// outcome follows the residual branch exactly — no correction inside
// the tolerance, one lag+priming correction outside, and the final
// file geometry matches the applied value.
func TestClosedLoopBranchContract(t *testing.T) {
	for _, lag := range []float64{3.0, -0.5} {
		t.Run(fmt.Sprintf("lag%+.1f", lag), func(t *testing.T) {
			a, _, ref, raw, shifted := closedLoopFixture(t, lag)
			ctx := context.Background()
			// The residual the loop WILL measure: same shift, same
			// decode, same correlation — deterministic.
			if err := ShiftWAVFile(raw, shifted, FramesFor(lag+AACPrimingSec, PipelineRate)); err != nil {
				t.Fatalf("pre-shift: %v", err)
			}
			res, err := a.measureShifted(ctx, ref, shifted)
			if err != nil {
				t.Fatalf("measureShifted: %v", err)
			}
			applied, err := a.ClosedLoop(ctx, ref, raw, shifted, lag)
			if err != nil {
				t.Fatalf("ClosedLoop: %v", err)
			}
			want := lag + AACPrimingSec
			if math.Abs(res+AACPrimingSec) > ResidualTolerance {
				want = lag + (res + AACPrimingSec) + AACPrimingSec
			}
			if applied != want {
				t.Fatalf("applied = %v, want %v (residual %v, tolerance %v)", applied, want, res, ResidualTolerance)
			}
			// The final file geometry follows the applied shift.
			layout := mustLayout(t, shifted)
			rawLayout := mustLayout(t, raw)
			delta := rawLayout.Frames - layout.Frames
			if wantFrames := FramesFor(applied, PipelineRate); delta != wantFrames {
				t.Fatalf("payload delta = %d frames, want %d (applied %v)", delta, wantFrames, applied)
			}
		})
	}
}

// TestClosedLoopReshiftsFromRaw pins the «re-shift from the ORIGINAL
// WAV» rule: after the correction the payload delta equals the FINAL
// target's frames — a chained shift would double the trim.
func TestClosedLoopReshiftsFromRaw(t *testing.T) {
	a, _, ref, raw, shifted := closedLoopFixture(t, 3)
	ctx := context.Background()
	if err := ShiftWAVFile(raw, shifted, FramesFor(3.0+AACPrimingSec, PipelineRate)); err != nil {
		t.Fatal(err)
	}
	res, err := a.measureShifted(ctx, ref, shifted)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := a.ClosedLoop(ctx, ref, raw, shifted, 3.0)
	if err != nil {
		t.Fatalf("ClosedLoop: %v", err)
	}
	finalTarget := applied - AACPrimingSec
	if math.Abs(res+AACPrimingSec) <= ResidualTolerance {
		t.Fatalf("fixture residual %v landed in tolerance — this pin needs the out-of-tolerance branch", res)
	}
	if want := 3.0 + (res + AACPrimingSec); math.Abs(finalTarget-want) > 1e-9 {
		t.Fatalf("final target = %v, want measured %v + one priming correction → %v", finalTarget, res, want)
	}
	layout := mustLayout(t, shifted)
	rawLayout := mustLayout(t, raw)
	if delta := rawLayout.Frames - layout.Frames; delta != FramesFor(applied, PipelineRate) {
		t.Fatalf("payload delta = %d, want exactly the final applied %v frames", delta, applied)
	}
}

// TestClosedLoopSilentShiftedIsNoConfidence: a donor that decodes
// silent fails the loop loudly — never a silent success.
func TestClosedLoopSilentShiftedIsNoConfidence(t *testing.T) {
	a, _, ref, _, shifted := closedLoopFixture(t, 3)
	silent := filepath.Join(t.TempDir(), "silent.wav")
	writeSynthWAV(t, silent, make([]float64, 48000))
	if _, err := a.ClosedLoop(context.Background(), ref, silent, shifted, 0); !errors.Is(err, ErrNoConfidence) {
		t.Fatalf("want ErrNoConfidence for a silent donor, got %v", err)
	}
}

// TestPrimingFrames pins test 4 of the brief: shift = lag +
// 1024/48000 converts to the exact frame count.
func TestPrimingFrames(t *testing.T) {
	if got := FramesFor(3.0+AACPrimingSec, PipelineRate); got != 145024 {
		t.Fatalf("frames(3.0+priming) = %d, want 145024", got)
	}
	if got := FramesFor(-0.5+AACPrimingSec, PipelineRate); got != -22976 {
		t.Fatalf("frames(−0.5+priming) = %d, want −22976", got)
	}
}

func mustLayout(t *testing.T, path string) wavLayout {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the test reads its own temp wav
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	layout, err := readWAVLayout(f)
	if err != nil {
		t.Fatal(err)
	}
	return layout
}
