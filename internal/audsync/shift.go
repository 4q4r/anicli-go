package audsync

// The byte-exact PCM shift, file to file with FLAT memory: the offset
// is applied to the WAV payload by moving frame boundaries, never by
// re-encoding or re-scaling. Positive frames TRIM the payload head
// (the donor lags the video); negative frames PAD zero frames (the
// donor leads). The destination header is rebuilt with recalculated
// data and RIFF sizes; every other chunk byte is copied verbatim.

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
)

// ShiftWAVFile applies a ±frames shift to src's PCM payload and writes
// a valid RIFF file to dst. frames > 0 — trim that many frames off the
// head; frames < 0 — prepend that many zero frames; 0 — verbatim copy.
// A trim beyond the payload is a typed error, never a silently empty
// file.
func ShiftWAVFile(src, dst string, frames int64) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is the caller-provided temp WAV, the function's contract
	if err != nil {
		return fmt.Errorf("audsync: shift: %w", err)
	}
	defer func() { _ = in.Close() }()
	layout, err := readWAVLayout(in)
	if err != nil {
		return fmt.Errorf("audsync: shift: %w", err)
	}
	if frames > 0 && frames > layout.Frames {
		return fmt.Errorf("audsync: shift: %w (frames %d > %d)", errOutOfRange, frames, layout.Frames)
	}

	inHeader := make([]byte, layout.DataOff)
	if _, err := in.ReadAt(inHeader, 0); err != nil {
		return fmt.Errorf("audsync: shift: %w", err)
	}
	srcPayload := layout.DataSize
	var dstPayload int64
	var payloadSrc func(w io.Writer) error
	switch {
	case frames > 0:
		dstPayload = srcPayload - frames*int64(layout.BlockAlign)
		payloadSrc = func(w io.Writer) error {
			_, err := io.CopyN(w, io.NewSectionReader(in, layout.DataOff+frames*int64(layout.BlockAlign), dstPayload), dstPayload)
			return err
		}
	case frames < 0:
		dstPayload = srcPayload + (-frames)*int64(layout.BlockAlign)
		payloadSrc = func(w io.Writer) error {
			if _, err := w.Write(make([]byte, -frames*int64(layout.BlockAlign))); err != nil {
				return err
			}
			_, err := io.CopyN(w, io.NewSectionReader(in, layout.DataOff, srcPayload), srcPayload)
			return err
		}
	default:
		dstPayload = srcPayload
		payloadSrc = func(w io.Writer) error {
			_, err := io.CopyN(w, io.NewSectionReader(in, layout.DataOff, srcPayload), srcPayload)
			return err
		}
	}

	// The data payload is frame-aligned s16 (even sizes) — the pad
	// byte stays correct by construction, but the code stays honest
	// about it.
	pad := dstPayload % 2
	riffSize := layout.DataOff - 8 + dstPayload + pad

	out, err := os.Create(dst) //nolint:gosec // G304: dst is the caller-provided temp path
	if err != nil {
		return fmt.Errorf("audsync: shift: %w", err)
	}
	defer func() { _ = out.Close() }()

	binary.LittleEndian.PutUint32(inHeader[4:8], uint32(riffSize))                               //nolint:gosec // G115: RIFF sizes are uint32 by spec
	binary.LittleEndian.PutUint32(inHeader[layout.DataOff-4:layout.DataOff], uint32(dstPayload)) //nolint:gosec // G115: same spec bound
	if _, err := out.Write(inHeader); err != nil {
		return fmt.Errorf("audsync: shift: %w", err)
	}
	if err := payloadSrc(out); err != nil {
		return fmt.Errorf("audsync: shift: %w", err)
	}
	if pad == 1 {
		if _, err := out.Write([]byte{0}); err != nil {
			return fmt.Errorf("audsync: shift: %w", err)
		}
	}
	return out.Close()
}

// FramesFor converts seconds to PCM frames at the given rate
// (half-frames round to nearest).
func FramesFor(seconds float64, rate int) int64 {
	return int64(math.Round(seconds * float64(rate)))
}
