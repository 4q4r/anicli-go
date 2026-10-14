package audsync

// The verdict pipeline over already-decoded windows (pure — no exec):
// per window an RMS silence gate, then the cross-correlation with a
// prominence gate; the surviving offsets must agree within
// ConsensusTolerance (≥2 of them); the verdict is their median snapped
// to the video's frame grid. Anything weaker is ErrNoConfidence — a
// download without a confident offset proceeds unsynchronized.

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// Reference is one episode's decoded reference: the viable windows of
// the video's own track (index-aligned to WindowSchedule — a nil entry
// keeps the pairing when a window could not be decoded) and the video's
// frame rate for the snap.
type Reference struct {
	Windows [][]float64
	FPS     float64
}

// Reference decodes the video's own-track windows (once per episode)
// and probes its frame rate. The episode's duration gates the whole
// analysis: an unreadable duration is ErrNoReference; an episode
// shorter than MinEpisodeDur yields an EMPTY reference (no windows),
// which every Offset rejects with ErrNoConfidence — the caller just
// skips the analysis. All-silent windows are ErrNoReference.
func (a *Analyzer) Reference(ctx context.Context, video contracts.VideoSource) (Reference, error) {
	dur, err := a.probeDuration(ctx, video)
	if err != nil {
		return Reference{}, fmt.Errorf("%w (video duration: %w)", ErrNoReference, err)
	}
	schedule := WindowSchedule(dur)
	if schedule == nil {
		return Reference{}, nil // too short: an honest empty reference
	}
	fps := DefaultFPS
	if f, err := a.probeFPS(ctx, video); err == nil {
		fps = f // an unreadable fps is DefaultFPS, never an error
	}

	windows := make([][]float64, len(schedule))
	decoded, audible := 0, 0
	for i, w := range schedule {
		data, _, err := a.exec(ctx, a.ffmpeg, decodeArgs(video, w.RefStart, ReferenceDur))
		if err != nil {
			continue // a dead window is not evidence; keep the pairing
		}
		samples := decodePCM(data)
		decoded++
		if rms(samples) >= MinRMS {
			audible++
		}
		windows[i] = samples
	}
	if decoded == 0 || audible == 0 {
		return Reference{}, ErrNoReference
	}
	return Reference{Windows: windows, FPS: fps}, nil
}

// Offset renders one donor's verdict against a Reference within the
// analysis budget (the timeout kills ffmpeg mid-window). Every failure
// mode wraps ErrNoConfidence — the analysis NEVER fails a download.
func (a *Analyzer) Offset(ctx context.Context, ref Reference, dub contracts.VideoSource) (SyncOffset, error) {
	if len(ref.Windows) == 0 {
		return SyncOffset{}, ErrNoConfidence
	}
	ctx, cancel := context.WithTimeout(ctx, a.budget)
	defer cancel()

	dur, err := a.probeDuration(ctx, dub)
	if err != nil {
		return SyncOffset{}, fmt.Errorf("%w (donor duration: %w)", ErrNoConfidence, err)
	}
	schedule := WindowSchedule(dur)
	if schedule == nil {
		return SyncOffset{}, fmt.Errorf("%w (donor too short: %.0fs)", ErrNoConfidence, dur)
	}

	searchWins := make([][]float64, len(schedule))
	for i, w := range schedule {
		data, _, err := a.exec(ctx, a.ffmpeg, decodeArgs(dub, w.SearchStart, SearchDur))
		if err != nil {
			continue // a dead window is not evidence; keep the pairing
		}
		searchWins[i] = decodePCM(data)
	}
	off, err := analyzePair(ref.Windows, searchWins, ref.FPS)
	if err != nil {
		return SyncOffset{}, err
	}
	return off, nil
}

// probeDuration / probeFPS run the ffprobe legs through the seam.
func (a *Analyzer) probeDuration(ctx context.Context, src contracts.VideoSource) (float64, error) {
	out, _, err := a.exec(ctx, a.ffprobe, durationArgs(src))
	if err != nil {
		return 0, err
	}
	return parseDuration(out)
}

func (a *Analyzer) probeFPS(ctx context.Context, src contracts.VideoSource) (float64, error) {
	out, _, err := a.exec(ctx, a.ffprobe, fpsArgs(src))
	if err != nil {
		return 0, err
	}
	return parseFPS(out)
}

// analyzePair renders one (video, donor) verdict from the decoded
// window samples. refWins[i] and searchWins[i] carry s16-scale float
// samples of the i-th scheduled window (60 s reference, 180 s search).
// fps is the video's frame rate (≤ 0 → DefaultFPS) for the final snap.
func analyzePair(refWins, searchWins [][]float64, fps float64) (SyncOffset, error) {
	if fps <= 0 {
		fps = DefaultFPS
	}
	type vote struct {
		off  float64
		prom float64
	}
	var votes []vote
	for i := 0; i < len(refWins) && i < len(searchWins); i++ {
		ref, srch := refWins[i], searchWins[i]
		if rms(ref) < MinRMS || rms(srch) < MinRMS {
			continue // a silent window is not evidence (nor a vote)
		}
		delay, prom, err := CrossCorrelate(ref, srch)
		if err != nil || prom < MinProminence {
			continue
		}
		off := (float64(delay) - float64(len(ref))) / SampleRate
		votes = append(votes, vote{off: off, prom: prom})
	}
	if len(votes) < 2 {
		return SyncOffset{}, ErrNoConfidence
	}

	// Consensus: the largest cluster of votes within the tolerance
	// band (sort by offset, take the densest sliding window). A
	// minority cluster or a fully split vote set is no confidence.
	sort.Slice(votes, func(a, b int) bool { return votes[a].off < votes[b].off })
	bestStart, bestCount := 0, 1
	for i := range votes {
		count := 1
		for j := i + 1; j < len(votes) && votes[j].off-votes[i].off <= ConsensusTolerance; j++ {
			count++
		}
		if count > bestCount {
			bestStart, bestCount = i, count
		}
	}
	if bestCount < 2 {
		return SyncOffset{}, ErrNoConfidence
	}
	agree := votes[bestStart : bestStart+bestCount]

	// Median of the agreeing offsets (even counts average the middle
	// pair), then the frame-grid snap.
	offs := make([]float64, len(agree))
	minProm := agree[0].prom
	for i, v := range agree {
		offs[i] = v.off
		if v.prom < minProm {
			minProm = v.prom
		}
	}
	sort.Float64s(offs)
	var median float64
	if len(offs)%2 == 1 {
		median = offs[len(offs)/2]
	} else {
		median = (offs[len(offs)/2-1] + offs[len(offs)/2]) / 2
	}
	conf := minProm / MinProminence
	if conf > ConfidenceCap {
		conf = ConfidenceCap
	}
	return SyncOffset{
		Seconds:      snapToFrame(median, fps),
		Confidence:   conf,
		WindowsAgree: len(agree),
	}, nil
}

// snapToFrame pulls the offset onto the frame grid when it sits within
// SnapTolerance of one (the AAC-priming and demux jitter band); an
// off-grid offset is kept verbatim — a wrong snap is worse than none.
func snapToFrame(off, fps float64) float64 {
	if fps <= 0 {
		fps = DefaultFPS
	}
	snapped := math.Round(off*fps) / fps
	if math.Abs(off-snapped) <= SnapTolerance {
		return snapped
	}
	return off
}

// rms is the root-mean-square of s16-scale float samples.
func rms(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range x {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

// RMSOfPCM is the s16le PCM root-mean-square in s16 units — the
// assembled-file verification floor (Task 8) reads it. An empty or
// odd-length buffer is 0 (never a panic); odd trailing bytes are
// ignored.
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
