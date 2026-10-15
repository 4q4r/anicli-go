package audsync

// The closed loop (the script's main flow — the owner measured a
// 0.00 ms residual with EXACTLY this): apply lag + AAC_PRIMING to the
// decoded pipeline WAV, re-decode the shifted result (a cheap local
// decode), re-measure in a ±1 s band, and if the residual plus the
// priming lands outside ±15 ms, correct the target once and re-shift
// from the ORIGINAL WAV. One pass, no loop. The m4a encode then puts
// the priming back — the residual vanishes in the muxed product.

import (
	"context"
	"fmt"
	"math"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// ClosedLoop runs the per-donor application on an already-decoded
// pipeline WAV: the initial shift, the residual re-measurement and
// the single correction pass, leaving shiftedWAV as the final
// product. measuredLag is the analysis phase's verdict for this
// donor; the returned value is the APPLIED shift (final lag +
// AACPrimingSec) — the frames the pipeline actually moved, ready for
// FramesFor and the lead-geometry verification.
func (a *Analyzer) ClosedLoop(ctx context.Context, ref Reference, rawWAV, shiftedWAV string, measuredLag float64) (float64, error) {
	target := measuredLag
	if err := shiftFromRaw(rawWAV, shiftedWAV, target); err != nil {
		return 0, err
	}

	residual, err := a.measureShifted(ctx, ref, shiftedWAV)
	if err != nil {
		return 0, err
	}
	if math.Abs(residual+AACPrimingSec) > ResidualTolerance {
		target += residual + AACPrimingSec
		if err := shiftFromRaw(rawWAV, shiftedWAV, target); err != nil {
			return 0, err
		}
	}
	return target + AACPrimingSec, nil
}

// shiftFromRaw applies one shift from the ORIGINAL pipeline WAV (the
// script's apply_pcm_shift_safe from tmp_raw — never a chained
// shift), in pipeline frames.
func shiftFromRaw(rawWAV, shiftedWAV string, target float64) error {
	if err := ShiftWAVFile(rawWAV, shiftedWAV, FramesFor(target+AACPrimingSec, PipelineRate)); err != nil {
		return fmt.Errorf("audsync: closed loop shift: %w", err)
	}
	return nil
}

// measureShifted re-decodes the shifted WAV (a local file — a plain
// ffmpeg decode) and measures its content lag in the ±1 s band.
func (a *Analyzer) measureShifted(ctx context.Context, ref Reference, shiftedWAV string) (float64, error) {
	decodeCtx, cancel := context.WithTimeout(ctx, a.decodeBudget)
	defer cancel()
	env, err := a.decodeEnvelope(decodeCtx, contracts.VideoSource{URL: shiftedWAV}, -1)
	if err != nil {
		return 0, fmt.Errorf("audsync: closed loop re-decode: %w", err)
	}
	if envelopeActivity(env) < MinEnvBins {
		return 0, fmt.Errorf("audsync: closed loop: %w (shifted envelope is silent)", ErrNoConfidence)
	}
	lag, err := measureLag(env, ref.Envelope, -ResidualLagWindow, ResidualLagWindow)
	if err != nil {
		return 0, fmt.Errorf("audsync: closed loop correlation: %w", err)
	}
	return lag, nil
}
