package download

// PR165b T8 tests: the assembled-file verification invariant — RMS at
// 45 s above the silence floor (catches the owner's «половина без
// звука») and the lead-silence geometry within ±1 s (trim → ≈0, pad →
// ≈|offset|). Hermetic: both ffmpeg legs run through injected seams.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseLeadSilence(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   time.Duration
	}{
		{
			"silence_from_zero",
			"[silencedetect @ 0x1] silence_start: 0\n" +
				"[silencedetect @ 0x1] silence_end: 8.123 | silence_duration: 8.123\n",
			8123 * time.Millisecond,
		},
		{"silence_not_from_zero", "[silencedetect @ 0x1] silence_start: 4.2\n[silencedetect @ 0x1] silence_end: 9.5 | silence_duration: 5.3\n", 0},
		{"no_silence", "", 0},
		{"garbage", "nonsense lines only\n", 0},
		{"unterminated_zero_silence", "[silencedetect @ 0x1] silence_start: 0\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLeadSilence(tc.stderr)
			dev := got - tc.want
			if dev < 0 {
				dev = -dev
			}
			// The stderr values are decimal floats — a millisecond of
			// conversion jitter is exact enough.
			if dev > time.Millisecond {
				t.Fatalf("parseLeadSilence = %v, want %v", got, tc.want)
			}
		})
	}
}

// verifySeams wires a Downloader with scripted verification legs:
// rmsOut answers the RMS@45s pipe call, leadStderr answers the
// silencedetect call.
func verifySeams(rmsOut []byte, leadStderr string) *Downloader {
	d := New(Options{})
	d.runOut = func(ctx context.Context, bin string, args []string) ([]byte, string, error) {
		return rmsOut, "", nil
	}
	d.run = func(ctx context.Context, bin string, args []string) (string, error) {
		return leadStderr, nil
	}
	return d
}

func soundingPCM(t *testing.T) []byte {
	t.Helper()
	// 5 s of loud 8 kHz mono s16le — well above the MinRMS floor.
	b := make([]byte, 5*8000*2)
	for i := range len(b) / 2 {
		v := 12000
		if (i/800)%2 == 0 {
			v = -12000
		}
		b[i*2] = byte(v)        //nolint:gosec // G115: the s16 two's-complement low byte, fixture data
		b[i*2+1] = byte(v >> 8) //nolint:gosec // G115: the s16 high byte, fixture data
	}
	return b
}

func TestVerifyAssembledTolerance(t *testing.T) {
	ctx := context.Background()
	// RMS pass + lead 0.4 s, expected 0 (trim) → nil.
	if err := verifySeams(soundingPCM(t), "[silencedetect] silence_start: 0\n[silencedetect] silence_end: 0.4 | silence_duration: 0.4\n").
		VerifyAssembled(ctx, "out.mkv", 0); err != nil {
		t.Fatalf("trim geometry: %v", err)
	}
	// RMS pass + lead 8 s, expected 0 → ErrSilentOutput{lead}.
	err := verifySeams(soundingPCM(t), "[silencedetect] silence_start: 0\n[silencedetect] silence_end: 8 | silence_duration: 8\n").
		VerifyAssembled(ctx, "out.mkv", 0)
	var silent *ErrSilentOutput
	if !errors.As(err, &silent) || silent.Check != "lead" {
		t.Fatalf("want ErrSilentOutput{lead}, got %v", err)
	}
	// RMS silence → ErrSilentOutput{rms@45s} regardless of the lead.
	err = verifySeams(nil, "").
		VerifyAssembled(ctx, "out.mkv", 0)
	if !errors.As(err, &silent) || silent.Check != "rms@45s" {
		t.Fatalf("want ErrSilentOutput{rms@45s}, got %v", err)
	}
	// Pad geometry: expected 17 s (offset −17), measured lead 17.5 s → nil.
	if err := verifySeams(soundingPCM(t), "[silencedetect] silence_start: 0\n[silencedetect] silence_end: 17.5 | silence_duration: 17.5\n").
		VerifyAssembled(ctx, "out.mkv", 17*time.Second); err != nil {
		t.Fatalf("pad geometry: %v", err)
	}
	// Just outside the ±1 s band: expected 0, measured 1.2 s → fail.
	err = verifySeams(soundingPCM(t), "[silencedetect] silence_start: 0\n[silencedetect] silence_end: 1.2 | silence_duration: 1.2\n").
		VerifyAssembled(ctx, "out.mkv", 0)
	if !errors.As(err, &silent) || silent.Check != "lead" {
		t.Fatalf("outside the lead band must fail, got %v", err)
	}
	// RMS exactly at the floor passes; below it fails.
	if err := verifySeams(floorPCM(t), "[silencedetect] silence_end: 0.5\n").
		VerifyAssembled(ctx, "out.mkv", 0); err != nil {
		t.Fatalf("at-floor RMS: %v", err)
	}
}

// floorPCM renders PCM whose RMS is exactly the s16 floor (±100).
func floorPCM(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 8000*2)
	for i := range len(b) / 2 {
		b[i*2], b[i*2+1] = 100, 0
	}
	return b
}

func TestErrSilentOutputText(t *testing.T) {
	err := &ErrSilentOutput{Path: "out.mkv", Check: "lead", Details: "measured 8s, expected 0s"}
	for _, want := range []string{"out.mkv", "lead", "measured 8s"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("typed error text %q missing %q", err.Error(), want)
		}
	}
}
