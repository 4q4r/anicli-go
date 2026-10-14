package audsync

// PR165b T5 tests: the RIFF chunk-walking WAV parser and the
// byte-exact PCM shift. Fixtures are HAND-BUILT RIFF byte streams —
// no ffmpeg in the hermetic suite (the real ffmpeg header shape,
// LIST/INFO before data, is the `live` test's business).

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// wavFixture builds a RIFF/WAVE file: fmt (PCM, 2ch, 48000 Hz, s16le,
// blockAlign 4) + [optional LIST/INFO 33-byte payload (odd → pad 1),
// the owner's 78-byte-header case] + data (payload = frame bytes,
// frame i = {i, -i-1} s16 samples). Returns the path and the expected
// frame count.
func wavFixture(t *testing.T, withListChunk bool) (string, int) {
	t.Helper()
	const frames = 100
	payload := make([]byte, 0, frames*4)
	for i := range frames {
		payload = binary.LittleEndian.AppendUint16(payload, uint16(i))    //nolint:gosec // G115: fixture frame data, i < 100
		payload = binary.LittleEndian.AppendUint16(payload, uint16(-i-1)) //nolint:gosec // G115: fixture frame data, two's-complement wrap intended
	}

	fmtChunk := make([]byte, 0, 8+16)
	fmtChunk = append(fmtChunk, "fmt "...)
	fmtChunk = binary.LittleEndian.AppendUint32(fmtChunk, 16)
	fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, 1) // PCM
	fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, 2) // stereo
	fmtChunk = binary.LittleEndian.AppendUint32(fmtChunk, 48000)
	fmtChunk = binary.LittleEndian.AppendUint32(fmtChunk, 48000*4)
	fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, 4)  // blockAlign
	fmtChunk = binary.LittleEndian.AppendUint16(fmtChunk, 16) // bits

	var body []byte
	body = append(body, "WAVE"...)
	body = append(body, fmtChunk...)
	if withListChunk {
		// 25-byte payload (odd → one pad byte): the header before the
		// PCM is 12+24+8+25+1+8 = 78 bytes — the owner's case.
		info := append([]byte("INFOISFT"), binary.LittleEndian.AppendUint32(nil, 13)...)
		info = append(info, []byte("Lavf59.27.100")...) // 13 bytes
		if len(info) != 25 {
			t.Fatalf("fixture LIST payload = %d, want 25", len(info))
		}
		body = append(body, "LIST"...)
		body = binary.LittleEndian.AppendUint32(body, uint32(len(info))) //nolint:gosec // G115: fixture size, far under 2^32
		body = append(body, info...)
		body = append(body, 0) // pad byte
	}
	body = append(body, "data"...)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(payload))) //nolint:gosec // G115: fixture size, far under 2^32
	body = append(body, payload...)

	out := make([]byte, 0, 8+len(body))
	out = append(out, "RIFF"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body))) //nolint:gosec // G115: fixture size, far under 2^32
	out = append(out, body...)

	path := filepath.Join(t.TempDir(), "fix.wav")
	if err := os.WriteFile(path, out, 0o600); err != nil { //nolint:gosec // G306: 0600, the test writes its own temp fixture
		t.Fatal(err)
	}
	return path, frames
}

func readFixture(t *testing.T, path string) wavLayout {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	l, err := readWAVLayout(f)
	if err != nil {
		t.Fatalf("readWAVLayout: %v", err)
	}
	return l
}

func TestReadWAVLayoutClassic44(t *testing.T) {
	path, frames := wavFixture(t, false)
	l := readFixture(t, path)
	if l.DataOff != 44 {
		t.Fatalf("DataOff = %d, want 44", l.DataOff)
	}
	if l.DataSize != int64(frames*4) {
		t.Fatalf("DataSize = %d, want %d", l.DataSize, frames*4)
	}
	if l.Frames != int64(frames) {
		t.Fatalf("Frames = %d, want %d", l.Frames, frames)
	}
	if l.BlockAlign != 4 {
		t.Fatalf("BlockAlign = %d, want 4", l.BlockAlign)
	}
}

func TestReadWAVLayoutWithListChunk(t *testing.T) {
	path, frames := wavFixture(t, true)
	l := readFixture(t, path)
	if l.DataOff != 78 {
		t.Fatalf("DataOff = %d, want 78 (LIST/INFO before data must not break the walk)", l.DataOff)
	}
	if l.Frames != int64(frames) {
		t.Fatalf("Frames = %d, want %d", l.Frames, frames)
	}
}

func TestReadWAVLayoutRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, blob []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, blob, 0o600); err != nil { //nolint:gosec // G306: 0600, the test writes its own temp fixture
			t.Fatal(err)
		}
		return path
	}
	buildWAV := func(mutate func(body []byte) []byte) []byte {
		path, _ := wavFixture(t, false)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp fixture
		if err != nil {
			t.Fatal(err)
		}
		body := raw[8:]
		body = mutate(body)
		out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...) //nolint:gosec // G115: fixture size, far under 2^32
		return append(out, body...)
	}
	// body layout: "WAVE"(0..4) | fmt chunk 8+16 (4..28) | "data"+size
	// (28..36) | payload.
	cases := []struct {
		name string
		blob []byte
	}{
		{"not_riff", []byte("RIFXxxxxxxxx")},
		{"not_wave", append([]byte("RIFF"), append(binary.LittleEndian.AppendUint32(nil, 4), "AVI "...)...)},
		{"no_data", []byte(buildWAV(func(body []byte) []byte {
			// drop the data chunk entirely
			return body[:8+24]
		}))},
		{"fmt_float", []byte(buildWAV(func(body []byte) []byte {
			// audio format 3 (IEEE float), fmt payload offset 0
			binary.LittleEndian.PutUint16(body[12:14], 3)
			return body
		}))},
		{"bits_not_16", []byte(buildWAV(func(body []byte) []byte {
			// bits-per-sample at fmt payload offset 14
			binary.LittleEndian.PutUint16(body[26:28], 24)
			return body
		}))},
		{"data_size_overrun", []byte(buildWAV(func(body []byte) []byte {
			// the data chunk claims far more than the file carries
			binary.LittleEndian.PutUint32(body[32:36], 1<<30) //nolint:gosec // G115: the fixture DELIBERATELY overruns to test the guard
			return body
		}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.Open(write(tc.name+".wav", tc.blob)) //nolint:gosec // G304: the test reads its own temp fixture
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			if _, err := readWAVLayout(f); err == nil {
				t.Fatalf("%s: want a typed error", tc.name)
			}
		})
	}
	// An empty file errors too.
	f, err := os.Open(write("empty.wav", nil)) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := readWAVLayout(f); err == nil {
		t.Fatal("empty file must error")
	}
}
