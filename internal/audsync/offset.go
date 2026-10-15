package audsync

// The donor leg (the script's measure_lag applied per dub): the whole
// donor decodes once at 8 kHz stereo, the vocal envelope gates it,
// and the band-limited full correlation against the episode's
// reference renders the lag. Every failure wraps ErrNoConfidence —
// the analysis NEVER fails a download.

import (
	"context"
	"fmt"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// Offset measures one donor against the episode reference. The
// whole-source decode is the slow leg and runs on its own
// DecodeBudget; the correlation is in-process FFT math (single-digit
// milliseconds at the fixed canvas) and needs no budget of its own.
// lag > 0 — the donor lags the reference (trim its head); lag < 0 —
// it leads (pad).
func (a *Analyzer) Offset(ctx context.Context, ref Reference, dub contracts.VideoSource) (SyncOffset, error) {
	decodeCtx, cancelDecode := context.WithTimeout(ctx, a.decodeBudget)
	defer cancelDecode()

	env, err := a.decodeEnvelope(decodeCtx, dub, -1)
	if err != nil {
		return SyncOffset{}, fmt.Errorf("%w (donor decode: %w)", ErrNoConfidence, err)
	}
	if envelopeActivity(env) < MinEnvBins {
		return SyncOffset{}, fmt.Errorf("%w (donor envelope is silent)", ErrNoConfidence)
	}
	lag, err := measureLag(env, ref.Envelope, ref.MinLag, ref.MaxLag)
	if err != nil {
		return SyncOffset{}, fmt.Errorf("%w (correlation: %w)", ErrNoConfidence, err)
	}
	return SyncOffset{Seconds: lag}, nil
}
