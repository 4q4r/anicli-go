package shikimori

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/an0nx/anicli-go/internal/storage"
)

// Syncer implements the immediate-push progress-sync policy
// (FEATURE_INVENTORY F ruling, 2026-06-28): after playback/download
// progress, push to Shikimori AT THE TIME and set the dirty flag ONLY on
// failure or a disabled/unauthenticated integration. The flag is the
// deferred-sync handoff: a later successful push clears it.
//
// Ported from anicli-py anicli/cli/download.py _sync_download_progress:
// local progress is always saved first, then the push runs, and the
// dirty flag follows the push verdict.
type Syncer struct {
	client *Client
	repo   *storage.ProgressRepo
	log    *slog.Logger
}

// NewSyncer builds the sync helper. logger may be nil (slog.Default).
func NewSyncer(client *Client, repo *storage.ProgressRepo, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{client: client, repo: repo, log: logger}
}

// SyncEpisodeProgress records episode as the new local progress of the
// row and immediately pushes {episodes, status: watching} to Shikimori.
//
// Verdicts:
//   - no shikimori binding on the row -> local update only, no dirty
//     flag, nil error (python: `if not s_id: return`);
//   - push succeeds -> new rate ids are persisted, dirty cleared, nil;
//   - push fails (network, auth, disabled) -> dirty set, error returned
//     (the caller decides how loudly to surface it; strict_sync_mode is
//     a CLI concern, not a client one).
func (s *Syncer) SyncEpisodeProgress(ctx context.Context, progress storage.AnimeProgress, episode int) error {
	// Local update first, dirty untouched (mark_dirty=False path).
	if err := s.repo.UpdatePlayback(ctx, progress.ID, strconv.Itoa(episode),
		derefString(progress.VideoDub), derefString(progress.AudioDub),
		progress.ProgressSeconds, progress.TotalSeconds); err != nil {
		return fmt.Errorf("shikimori sync: local progress update %d: %w", progress.ID, err)
	}

	if progress.ShikimoriID == nil {
		return nil
	}

	rateID, err := s.push(ctx, progress, episode)
	if err != nil {
		// Deferred-sync handoff: retry on the next sync pass. The flag
		// write must outlive the push failure — including the caller's
		// cancellation killing the push — so it runs detached from ctx.
		if markErr := s.repo.MarkDirty(context.WithoutCancel(ctx), progress.ID); markErr != nil {
			s.log.Warn("shikimori sync: mark dirty failed",
				"anime", progress.ID, "error", markErr)
		}
		return err
	}

	return s.persistSuccess(ctx, progress, rateID)
}

// persistSuccess records a successful push: the new rate id and the
// cleared dirty flag. Both writes are detached from the caller's
// context (F39): a cancellation arriving in the window after the
// server already recorded the push must not lose the rate id locally —
// the next sync would otherwise create a duplicate rate.
func (s *Syncer) persistSuccess(ctx context.Context, progress storage.AnimeProgress, rateID int64) error {
	if rateID != 0 && (progress.ShikimoriRateID == nil || *progress.ShikimoriRateID != rateID) {
		if err := s.repo.SetRateID(context.WithoutCancel(ctx), progress.ID, rateID); err != nil {
			return fmt.Errorf("shikimori sync: persist rate id %d: %w", rateID, err)
		}
	}
	if err := s.repo.ClearDirty(context.WithoutCancel(ctx), progress.ID); err != nil {
		return fmt.Errorf("shikimori sync: clear dirty %d: %w", progress.ID, err)
	}
	return nil
}

// push runs the immediate push: PATCH the existing rate or create one
// (python update_rate dispatch via _rate_exists, no brute-force).
func (s *Syncer) push(ctx context.Context, progress storage.AnimeProgress, episode int) (int64, error) {
	input := RateInput{Episodes: &episode, Status: "watching"}

	if progress.ShikimoriRateID != nil {
		return s.client.UpdateRate(ctx, *progress.ShikimoriRateID, input)
	}
	return s.client.CreateRate(ctx, *progress.ShikimoriID, input)
}

// derefString renders a nullable string.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
