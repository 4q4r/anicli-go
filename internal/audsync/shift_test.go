package audsync

// PR165b T5 tests: ShiftWAVFile (byte-exact, RIFF-safe) and FramesFor.

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestShiftWAVFileTrim exercises the byte-exact shift over the
// hand-built fixture.
func TestShiftWAVFileTrim(t *testing.T) {
	path, _ := wavFixture(t, true)
	src := readFixture(t, path)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(t.TempDir(), "trim.wav")
	if err := ShiftWAVFile(path, dstPath, 10); err != nil {
		t.Fatalf("ShiftWAVFile: %v", err)
	}
	dst := readFixture(t, dstPath)
	if dst.DataSize != src.DataSize-40 {
		t.Fatalf("DataSize = %d, want %d", dst.DataSize, src.DataSize-40)
	}
	if dst.Frames != src.Frames-10 {
		t.Fatalf("Frames = %d, want %d", dst.Frames, src.Frames-10)
	}
	if dst.DataOff != src.DataOff {
		t.Fatalf("DataOff = %d, want %d (header shape preserved)", dst.DataOff, src.DataOff)
	}
	out, err := os.ReadFile(dstPath) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	gotPayload := out[dst.DataOff : dst.DataOff+dst.DataSize]
	wantPayload := raw[src.DataOff+40 : src.DataOff+src.DataSize]
	if !bytes.Equal(gotPayload, wantPayload) {
		t.Fatal("trimmed payload is not the source from frame 10")
	}
	// The RIFF size was recalculated: file size − 8.
	if got := binary.LittleEndian.Uint32(out[4:8]); int64(got) != int64(len(out))-8 {
		t.Fatalf("RIFF size = %d, want %d", got, len(out)-8)
	}
}

func TestShiftWAVFilePad(t *testing.T) {
	path, _ := wavFixture(t, false)
	src := readFixture(t, path)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(t.TempDir(), "pad.wav")
	if err := ShiftWAVFile(path, dstPath, -10); err != nil {
		t.Fatalf("ShiftWAVFile: %v", err)
	}
	dst := readFixture(t, dstPath)
	if dst.DataSize != src.DataSize+40 {
		t.Fatalf("DataSize = %d, want %d", dst.DataSize, src.DataSize+40)
	}
	out, err := os.ReadFile(dstPath) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	head := out[dst.DataOff : dst.DataOff+40]
	if !bytes.Equal(head, make([]byte, 40)) {
		t.Fatal("padded head is not silence")
	}
	if !bytes.Equal(out[dst.DataOff+40:dst.DataOff+dst.DataSize], raw[src.DataOff:src.DataOff+src.DataSize]) {
		t.Fatal("payload after the pad is not 1:1")
	}
}

func TestShiftWAVFileZeroAndNegativeGuards(t *testing.T) {
	path, frames := wavFixture(t, false)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	// frames = 0 → content 1:1.
	dstPath := filepath.Join(t.TempDir(), "same.wav")
	if err := ShiftWAVFile(path, dstPath, 0); err != nil {
		t.Fatalf("ShiftWAVFile(0): %v", err)
	}
	out, err := os.ReadFile(dstPath) //nolint:gosec // G304: the test reads its own temp fixture
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatal("zero shift must copy 1:1")
	}
	// Trim beyond the payload → typed error, never a silently empty file.
	if err := ShiftWAVFile(path, filepath.Join(t.TempDir(), "over.wav"), int64(frames)+1); err == nil {
		t.Fatal("over-trim must error")
	}
	// Missing source → error.
	if err := ShiftWAVFile(filepath.Join(t.TempDir(), "absent.wav"), filepath.Join(t.TempDir(), "x.wav"), 1); err == nil {
		t.Fatal("missing source must error")
	}
}

func TestFramesFor(t *testing.T) {
	cases := []struct {
		seconds float64
		want    int64
	}{
		{17.042, 818016},
		{-0.875, -42000},
		{0, 0},
		{1.0 / 3.0, 16000}, // 48000/3 rounds
	}
	for _, tc := range cases {
		if got := FramesFor(tc.seconds, 48000); got != tc.want {
			t.Fatalf("FramesFor(%v, 48000) = %d, want %d", tc.seconds, got, tc.want)
		}
	}
	if FramesFor(0.5, 8000) != 4000 {
		t.Fatal("rate must be honored")
	}
}
