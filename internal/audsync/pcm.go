package audsync

// The PIPELINE argv shapes — the owner's forced decode/encode pair,
// unchanged from PR165b:
//   - DecodeWAVArgs: the whole donor's first audio track, 48 kHz
//     stereo s16le WAV on disk — no -ss/-t, because the byte shift
//     operates on decode order and must stay immune to a nonlinear
//     HLS timeline;
//   - EncodeM4AArgs: the shifted-WAV → isolated m4a codec pass.
//
// The ANALYSIS argv shapes live with the v2 algorithm: envDecodeArgs
// and assExtractArgs (reference.go).

import (
	"encoding/binary"
	"math"
	"sort"
	"strings"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// headerBlob renders per-input HTTP headers as ONE -headers pair
// (ffmpeg honors only the last): keys sorted, CR/LF stripped from
// keys/values, CRLF-terminated blob — the same contract as the
// download package's builder.
func headerBlob(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	blob := ""
	for _, k := range keys {
		blob += stripCRLF(k) + ": " + stripCRLF(headers[k]) + "\r\n"
	}
	return []string{"-headers", blob}
}

// stripCRLF removes CR and LF bytes from one header key/value.
func stripCRLF(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return crlfReplacer.Replace(s)
}

var crlfReplacer = strings.NewReplacer("\r", "", "\n", "")

// DecodeWAVArgs renders the PIPELINE decode (owner-forced 48 kHz
// stereo WAV; the whole donor — never a window).
func DecodeWAVArgs(src contracts.VideoSource, outWAV string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, headerBlob(src.Headers)...)
	args = append(args, "-i", src.URL, "-map", "0:a:0",
		"-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le", "-f", "wav", outWAV)
	return args
}

// EncodeM4AArgs renders the shifted-WAV → m4a codec pass.
func EncodeM4AArgs(inWAV, outM4A string) []string {
	return []string{"-y", "-hide_banner", "-loglevel", "error",
		"-i", inWAV, "-c:a", "aac", "-b:a", "256k", outM4A}
}

// s16Bytes is the inverse of decodePCM (tests and the closed-loop live
// proofs use it to synthesize PCM).
func s16Bytes(samples []float64) []byte {
	out := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(clampS16(v)))) //nolint:gosec // G115: same s16 reinterpretation, round-tripped
	}
	return out
}

// clampS16 keeps synthetic values inside the s16 range.
func clampS16(v float64) float64 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return v
	}
}

// RMSOfPCM is the s16le PCM root-mean-square in s16 units — the
// assembled-file verification floor (the download package's
// VerifyAssembled reads it). An empty or odd-length buffer is 0 (never
// a panic); odd trailing bytes are ignored.
func RMSOfPCM(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	sum := 0.0
	for i := range n {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) //nolint:gosec // G115: PCM s16le IS the two's-complement reinterpretation of the uint16 word
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}
