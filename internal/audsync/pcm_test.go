package audsync

// PR173 v2 tests: the PIPELINE argv shapes (the 48 kHz stereo WAV
// decode and the m4a codec pass — unchanged from PR165b) and the s16
// conversion helpers. The analysis argv goldens live with their
// owners: envDecodeArgs and assExtractArgs in reference_test.go
// (well — offset_test.go's fake asserts them live), the envelope
// math in envelope_test.go.

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// TestDecodeWAVArgsForcedPipeline pins the owner's forced pipeline
// decode: 48 kHz stereo s16le WAV, first audio track, and NO -ss/-t —
// the donor decodes WHOLE (the PCM byte shift is decode-order based,
// immune to a nonlinear HLS timeline).
func TestDecodeWAVArgsForcedPipeline(t *testing.T) {
	src := contracts.VideoSource{URL: "https://x/dub.m3u8"}
	got := DecodeWAVArgs(src, "/tmp/out.wav")
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
	got := EncodeM4AArgs("/tmp/in.wav", "/tmp/out.m4a")
	want := []string{"-y", "-hide_banner", "-loglevel", "error",
		"-i", "/tmp/in.wav", "-c:a", "aac", "-b:a", "256k", "/tmp/out.m4a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
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
