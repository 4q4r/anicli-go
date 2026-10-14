package audsync

// PR165b T4 tests: the two DECODE argv shapes (8 kHz mono analysis vs
// 48 kHz stereo WAV pipeline — never mixed), the m4a codec argv, the
// s16le conversion, and the ffprobe parsers.

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestDecodeArgs(t *testing.T) {
	src := contracts.VideoSource{URL: "https://x/v.m3u8", Headers: map[string]string{"Referer": "https://r/"}}
	want := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-headers", "Referer: https://r/\r\n", "-ss", "90.000", "-t", "180.000",
		"-i", "https://x/v.m3u8", "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:"}
	if got := decodeArgs(src, 90, 180); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestDecodeArgsNoHeaders(t *testing.T) {
	src := contracts.VideoSource{URL: "https://x/v.m3u8"}
	got := decodeArgs(src, 1.5, 2.25)
	want := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", "1.500", "-t", "2.250",
		"-i", "https://x/v.m3u8", "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

// TestDecodeWAVArgsForcedPipeline pins the owner's forced pipeline
// decode: 48 kHz stereo s16le WAV, first audio track, and NO -ss/-t —
// the donor decodes WHOLE (the PCM byte shift is decode-order based,
// immune to a nonlinear HLS timeline).
func TestDecodeWAVArgsForcedPipeline(t *testing.T) {
	src := contracts.VideoSource{URL: "https://x/dub.m3u8"}
	got := decodeWAVArgs(src, "/tmp/out.wav")
	want := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", "https://x/dub.m3u8", "-map", "0:a:0",
		"-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le", "-f", "wav", "/tmp/out.wav"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	for _, banned := range []string{"-ss", "-t"} {
		for _, a := range got {
			if a == banned {
				t.Fatalf("pipeline argv carries %s: %q", banned, got)
			}
		}
	}
}

func TestEncodeM4AArgs(t *testing.T) {
	got := encodeM4AArgs("/tmp/in.wav", "/tmp/out.m4a")
	want := []string{"-y", "-hide_banner", "-loglevel", "error",
		"-i", "/tmp/in.wav", "-c:a", "aac", "-b:a", "256k", "/tmp/out.m4a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

// TestDecodePCMConvertsS16LE pins the little-endian signed conversion.
// The float values keep the s16 integer scale (RMS gates compare
// against MinRMS in s16 units; the correlation is scale-invariant).
func TestDecodePCMConvertsS16LE(t *testing.T) {
	pcm := []byte{
		0x00, 0x80, // -32768
		0xff, 0x7f, // 32767
		0x01, 0x80, // -32767
		0x00, 0x01, // 256
	}
	got := decodePCM(pcm)
	want := []float64{-32768, 32767, -32767, 256}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(decodePCM(nil)) != 0 {
		t.Fatal("empty pcm must decode to an empty slice")
	}
	// Odd trailing byte is ignored, never a panic.
	if len(decodePCM([]byte{1, 2, 3})) != 1 {
		t.Fatal("odd pcm must ignore the trailing byte")
	}
}

func TestS16BytesRoundTrip(t *testing.T) {
	vals := []float64{1000, -1000, 32767, -32768, 0}
	b := s16Bytes(vals)
	if len(b) != len(vals)*2 {
		t.Fatalf("len = %d, want %d", len(b), len(vals)*2)
	}
	for i, v := range vals {
		if got := float64(int16(binary.LittleEndian.Uint16(b[i*2:]))); got != v { //nolint:gosec // G115: the s16 reinterpretation under test
			t.Fatalf("[%d] = %v, want %v", i, got, v)
		}
	}
}

func TestParseDurationAndFPS(t *testing.T) {
	d, err := parseDuration([]byte(`{"format": {"duration": "1440.5"}}`))
	if err != nil || math.Abs(d-1440.5) > 1e-9 {
		t.Fatalf("duration = %v, %v", d, err)
	}
	if _, err := parseDuration([]byte(`{"format": {}}`)); err == nil {
		t.Fatal("missing duration must error")
	}
	if _, err := parseDuration([]byte(`garbage`)); err == nil {
		t.Fatal("malformed json must error")
	}

	fps, err := parseFPS([]byte(`{"streams": [{"r_frame_rate": "24000/1001"}]}`))
	if err != nil || math.Abs(fps-24000.0/1001.0) > 1e-9 {
		t.Fatalf("fps = %v, %v", fps, err)
	}
	if fps, err := parseFPS([]byte(`{"streams": []}`)); err != nil || fps != DefaultFPS {
		t.Fatalf("missing fps = %v, %v; want DefaultFPS, nil", fps, err)
	}
	if fps, err := parseFPS([]byte(`garbage`)); err != nil || fps != DefaultFPS {
		t.Fatalf("garbage fps = %v, %v; want DefaultFPS, nil", fps, err)
	}
	if fps, err := parseFPS([]byte(`{"streams": [{"r_frame_rate": "0/0"}]}`)); err != nil || fps != DefaultFPS {
		t.Fatalf("zero-denominator fps = %v, %v; want DefaultFPS, nil", fps, err)
	}
}
