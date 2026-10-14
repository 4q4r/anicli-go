package audsync

// The RIFF-safe WAV reader — a Go-port of the wave module's chunk
// semantics. ffmpeg writes a LIST/INFO chunk before data (the header
// is 78 bytes on the owner's build), so HARDCODED 44 is forbidden:
// chunks are walked (id, LE size, payload with a pad byte when the
// size is odd), fmt is validated as PCM s16le, and the FIRST data
// chunk wins.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// wavLayout is where the PCM payload lives inside one WAV file.
type wavLayout struct {
	// DataOff is the absolute offset of the data chunk's payload.
	DataOff int64
	// DataSize is the payload size declared by the chunk header.
	DataSize int64
	// Frames is DataSize / BlockAlign.
	Frames int64
	// BlockAlign is the byte size of one frame (channels × 2).
	BlockAlign int16
}

// readWAVLayout walks the RIFF chunks of f. Errors: not-RIFF, not-WAVE,
// a non-PCM or non-s16le fmt, missing fmt/data, and sizes that overrun
// the file — every malformed shape is a typed error, never a guess.
func readWAVLayout(f *os.File) (wavLayout, error) {
	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return wavLayout{}, fmt.Errorf("audsync: wav: %w", errTruncated)
	}
	if string(header[0:4]) != "RIFF" {
		return wavLayout{}, errNotRIFF
	}
	if string(header[8:12]) != "WAVE" {
		return wavLayout{}, errNotWAVE
	}

	layout := wavLayout{}
	sawFmt := false
	for {
		var chunk [8]byte
		n, err := io.ReadFull(f, chunk[:])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break // the walk only ends early on a truncated trailing chunk
		}
		if err != nil {
			return wavLayout{}, fmt.Errorf("audsync: wav: %w", err)
		}
		if n == 0 {
			break
		}
		id, size := string(chunk[0:4]), int64(binary.LittleEndian.Uint32(chunk[4:8]))
		payloadOff, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return wavLayout{}, fmt.Errorf("audsync: wav: %w", err)
		}
		switch id {
		case "fmt ":
			sawFmt = true
			if size < 16 {
				return wavLayout{}, errors.New("audsync: wav: fmt chunk too short")
			}
			var fmtBody [16]byte
			if _, err := io.ReadFull(f, fmtBody[:]); err != nil {
				return wavLayout{}, fmt.Errorf("audsync: wav: %w", errTruncated)
			}
			format := binary.LittleEndian.Uint16(fmtBody[0:2])
			channels := binary.LittleEndian.Uint16(fmtBody[2:4])
			bits := binary.LittleEndian.Uint16(fmtBody[14:16])
			if format != 1 {
				return wavLayout{}, fmt.Errorf("audsync: wav: %w (format %d)", errNotPCM, format)
			}
			if bits != 16 {
				return wavLayout{}, fmt.Errorf("audsync: wav: %w (%d-bit)", errNotPCM, bits)
			}
			if channels == 0 {
				return wavLayout{}, errors.New("audsync: wav: zero channels")
			}
			layout.BlockAlign = int16(channels * 2) //nolint:gosec // G115: channels×2 ≤ 131070 per the spec's block-align field
		case "data":
			if !sawFmt {
				return wavLayout{}, errors.New("audsync: wav: data chunk before fmt")
			}
			fileSize, err := f.Seek(0, io.SeekEnd)
			if err != nil {
				return wavLayout{}, fmt.Errorf("audsync: wav: %w", err)
			}
			if payloadOff+size > fileSize {
				return wavLayout{}, fmt.Errorf("audsync: wav: %w (data %d+%d > %d)", errBadSize, payloadOff, size, fileSize)
			}
			layout.DataOff = payloadOff
			layout.DataSize = size
			layout.Frames = size / int64(layout.BlockAlign)
			return layout, nil // the FIRST data chunk wins
		}
		// Skip the payload plus the pad byte every odd-sized chunk
		// carries (RIFF spec §"Chunk Padding").
		next := payloadOff + size + size%2
		if _, err := f.Seek(next, io.SeekStart); err != nil {
			return wavLayout{}, fmt.Errorf("audsync: wav: %w", err)
		}
	}
	if !sawFmt {
		return wavLayout{}, errors.New("audsync: wav: no fmt chunk")
	}
	return wavLayout{}, errors.New("audsync: wav: no data chunk")
}

var (
	errNotRIFF    = errors.New("not a RIFF container")
	errNotWAVE    = errors.New("not a WAVE form")
	errNotPCM     = errors.New("audio is not PCM s16le")
	errBadSize    = errors.New("chunk size overruns the file")
	errTruncated  = errors.New("truncated file")
	errOutOfRange = errors.New("trim exceeds the payload")
)
