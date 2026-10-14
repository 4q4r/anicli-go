package audsync

// PR165b T4 tests: the Analyzer's exec orchestration — the reference
// chain and the per-donor offset, against a scripted fake exec (never
// a real ffmpeg here; the real binaries are the `live` tag's business).

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// fakeExec scripts stdout by argv shape: duration probes, fps probes
// and PCM decodes. The PCM decode renders one shared synthetic
// timeline at the requested -ss/-t span, shifted by urlShift[URL]
// seconds (positive = the source lags the timeline).
type fakeExec struct {
	calls       [][]string
	timeline    []float64 // s16-scale float samples
	urlShift    map[string]float64
	durationErr bool    // duration probes fail
	sleepURL    string  // decodes of this URL sleep until ctx.Done
	fps         float64 // reply of the fps probe (NaN → malformed reply)
}

func (f *fakeExec) exec(ctx context.Context, bin string, args []string) ([]byte, string, error) {
	argv := append([]string{bin}, args...)
	f.calls = append(f.calls, argv)
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "format=duration"):
		if f.durationErr {
			return nil, "", errors.New("probe failed")
		}
		return []byte(`{"format": {"duration": "1440"}}`), "", nil
	case strings.Contains(joined, "r_frame_rate"):
		if math.IsNaN(f.fps) {
			return []byte(`garbage`), "", nil
		}
		return []byte(`{"streams": [{"r_frame_rate": "24/1"}]}`), "", nil
	case strings.Contains(joined, "-f s16le pipe:"):
		if urlOf(args) == f.sleepURL {
			<-ctx.Done()
			return nil, "", ctx.Err()
		}
		start, dur := spanOf(args)
		return s16Bytes(f.slice(urlOf(args), start, dur)), "", nil
	}
	return nil, "unscripted argv: " + joined, errors.New("unscripted argv")
}

// slice cuts the timeline at [start−shift, start−shift+dur) seconds of
// the given source, zero-filling outside the timeline.
func (f *fakeExec) slice(url string, start, dur float64) []float64 {
	n := int(dur * testRate)
	out := make([]float64, n)
	shift := f.urlShift[url]
	s0 := int(math.Round((start - shift) * testRate))
	for i := range out {
		j := s0 + i
		if j >= 0 && j < len(f.timeline) {
			out[i] = f.timeline[j]
		}
	}
	return out
}

// spanOf extracts the (-ss, -t) pair from a decode argv; urlOf picks
// the input URL (the argv's last element).
func spanOf(args []string) (start, dur float64) {
	for i, a := range args {
		if a == "-ss" && i+1 < len(args) {
			start, _ = strconv.ParseFloat(args[i+1], 64)
		}
		if a == "-t" && i+1 < len(args) {
			dur, _ = strconv.ParseFloat(args[i+1], 64)
		}
	}
	return start, dur
}

func urlOf(args []string) string {
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestReferenceDecodesThreeWindowsAndFPS(t *testing.T) {
	fe := &fakeExec{timeline: wobble(1500, 7), urlShift: map[string]float64{}}
	a := New(Options{Exec: fe.exec})
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"})
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	if len(ref.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(ref.Windows))
	}
	for _, w := range ref.Windows {
		if len(w) != int(ReferenceDur*testRate) {
			t.Fatalf("window len = %d, want %d", len(w), int(ReferenceDur*testRate))
		}
	}
	if math.Abs(ref.FPS-24) > 1e-9 {
		t.Fatalf("fps = %v, want 24", ref.FPS)
	}
	// The decode calls carried the scheduled reference starts.
	starts := map[string]bool{}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "-f s16le pipe:") {
			s, _ := spanOf(call)
			starts[strconv.FormatFloat(s, 'f', -1, 64)] = true
		}
	}
	for _, want := range []string{"90", "720", "960"} {
		if !starts[want] {
			t.Fatalf("decode starts %v missing %v", starts, want)
		}
	}
}

func TestReferenceErrOnSilence(t *testing.T) {
	fe := &fakeExec{timeline: zeros(1500 * testRate), urlShift: map[string]float64{}}
	a := New(Options{Exec: fe.exec})
	_, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"})
	if !errors.Is(err, ErrNoReference) {
		t.Fatalf("want ErrNoReference, got %v", err)
	}
}

func TestReferenceErrOnBadDuration(t *testing.T) {
	fe := &fakeExec{timeline: wobble(1500, 7), urlShift: map[string]float64{}, durationErr: true}
	a := New(Options{Exec: fe.exec})
	_, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"})
	if !errors.Is(err, ErrNoReference) {
		t.Fatalf("want ErrNoReference wrap, got %v", err)
	}
}

// TestOffsetVerdictThroughFakeExec: the donor lags the shared timeline
// by +2 s — the full orchestration (duration → schedule → 3 search
// decodes → verdict) must land +2, with agreeing windows.
func TestOffsetVerdictThroughFakeExec(t *testing.T) {
	fe := &fakeExec{timeline: wobble(1500, 7), urlShift: map[string]float64{
		"https://d/dub.m3u8": 2,
	}}
	a := New(Options{Exec: fe.exec})
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"})
	if err != nil {
		t.Fatalf("Reference: %v", err)
	}
	off, err := a.Offset(context.Background(), ref, contracts.VideoSource{URL: "https://d/dub.m3u8"})
	if err != nil {
		t.Fatalf("Offset: %v", err)
	}
	if math.Abs(off.Seconds-2) > 0.05 {
		t.Fatalf("offset = %v, want ≈2", off.Seconds)
	}
	if off.WindowsAgree < 2 || off.Confidence < 1 {
		t.Fatalf("verdict = %+v", off)
	}
}

func TestOffsetBudgetKillsDecode(t *testing.T) {
	const dubURL = "https://d/dub.m3u8"
	fe := &fakeExec{timeline: wobble(1500, 7), urlShift: map[string]float64{}, sleepURL: dubURL}
	a := New(Options{Exec: fe.exec, Budget: 200 * time.Millisecond})
	ref, err := a.Reference(context.Background(), contracts.VideoSource{URL: "https://v/video.m3u8"})
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
		t.Fatal("Offset ignored the analysis budget")
	}
}

// TestAnalyzerDefaults pins the Options normalization: empty binaries
// default to ffmpeg/ffprobe (the sibling rule), empty budget to
// DefaultBudget, nil exec to the real one.
func TestAnalyzerDefaults(t *testing.T) {
	a := New(Options{})
	if a.ffmpeg != "ffmpeg" || a.ffprobe != "ffprobe" || a.budget != DefaultBudget {
		t.Fatalf("defaults: %+v", a)
	}
	if a.exec == nil {
		t.Fatal("nil Exec must install realExec")
	}
	custom := New(Options{FFmpeg: "/opt/av/ffmpeg"})
	if custom.ffprobe != "/opt/av/ffprobe" {
		t.Fatalf("sibling ffprobe = %q", custom.ffprobe)
	}
}
