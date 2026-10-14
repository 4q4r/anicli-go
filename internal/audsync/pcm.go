package audsync

// The two DECODE argv shapes — never mix them:
//   - decodeArgs: the ANALYSIS decode (research §3.4): a timed window
//     at 8 kHz mono s16le straight to the pipe;
//   - decodeWAVArgs: the PIPELINE decode (the owner's forced shape):
//     the WHOLE donor, first audio track, 48 kHz stereo s16le WAV on
//     disk — no -ss/-t, because the byte shift operates on decode
//     order and must stay immune to a nonlinear HLS timeline.
//
// encodeM4AArgs is the shifted-WAV → isolated m4a codec pass.

import (
	"encoding/binary"
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

// decodeArgs renders the ANALYSIS decode: the WHOLE source's audio to
// the pipe, 8 kHz mono s16le. There is deliberately NO -ss: input
// seeking is unreliable on HLS demuxers (the T6 live proof — a
// discontinuous playlist re-decoded the head for every -ss), while a
// whole decode follows the true decode-order timeline (plan §1.3);
// the windows are then sliced from the samples locally. A full
// episode at 8 kHz mono is ~23 MB for 24 minutes.
func decodeArgs(src contracts.VideoSource) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, headerBlob(src.Headers)...)
	args = append(args, "-i", src.URL, "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "pipe:")
	return args
}

// decodeWAVArgs renders the PIPELINE decode (owner-forced 48 kHz
// stereo WAV; the whole donor — never a window).
func decodeWAVArgs(src contracts.VideoSource, outWAV string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	args = append(args, headerBlob(src.Headers)...)
	args = append(args, "-i", src.URL, "-map", "0:a:0",
		"-ar", "48000", "-ac", "2", "-c:a", "pcm_s16le", "-f", "wav", outWAV)
	return args
}

// encodeM4AArgs renders the shifted-WAV → m4a codec pass.
func encodeM4AArgs(inWAV, outM4A string) []string {
	return []string{"-y", "-hide_banner", "-loglevel", "error",
		"-i", inWAV, "-c:a", "aac", "-b:a", "256k", outM4A}
}

// decodePCM converts s16le bytes to float samples. The values keep the
// s16 integer scale: the RMS gates compare against MinRMS in s16
// units, and the correlation/prominence math is scale-invariant. An
// odd trailing byte is ignored; nil is an empty slice.
func decodePCM(pcm []byte) []float64 {
	n := len(pcm) / 2
	if n == 0 {
		return nil
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) //nolint:gosec // G115: PCM s16le IS the two's-complement reinterpretation of the uint16 word
	}
	return out
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
