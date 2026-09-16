package shikimori

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

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

// SyncResult reports the verdict of the startup two-way sync (PR27).
type SyncResult struct {
	// Updated counts existing records refreshed from remote.
	Updated int
	// Created counts new records created from remote.
	Created int
	// Pushed counts dirty records successfully pushed to remote.
	Pushed int
	// Conflicts counts records where both sides changed: the remote
	// list state (status/score/rewatches) won locally, the local
	// episode progress won and is replayed in the push phase — so a
	// conflict also lands in Pushed once its replay succeeds.
	Conflicts int
}

// SyncProgress reports the live state of a running sync (rendered on
// the SyncScreen spinner line).
type SyncProgress struct {
	Phase   string // "rates" | "pull" | "new" | "push"
	Message string // human-readable, e.g. "Загрузка метаданных: 50/200"
	Done    int
	Total   int
}

// SyncFull runs the two-way startup sync (PR27), ported from
// anicli-py anicli/cli/shikimori_sync.py:30-102 plus the deferred
// dirty-flag replay: pull remote rates into bound local rows, create
// shikimori-sourced placeholders for remote-only anime (metadata in
// 50-id chunks via GetAnimesInfo), then replay every dirty row.
// progress (may be nil) is invoked on every phase transition and
// periodically within phases so the caller can render a live status
// line.
//
// Verdicts:
//   - integration disabled or empty remote list -> zeroed result, nil;
//   - rates fetch failure -> zeroed result + error (the sync screen
//     renders its warning from it);
//   - a failed dirty replay keeps the row dirty for the next startup
//     and surfaces as the returned error alongside the partial result.
func (s *Syncer) SyncFull(ctx context.Context, progress func(SyncProgress)) (*SyncResult, error) {
	report := func(p SyncProgress) {
		if progress != nil {
			progress(p)
		}
	}
	result := &SyncResult{}

	s.log.Info("shikimori sync: starting", "repo_nil", s.repo == nil)
	report(SyncProgress{Phase: "rates", Message: "Загрузка списка Shikimori…"})
	rates, err := s.client.GetUserRates(ctx)
	if err != nil {
		if errors.Is(err, ErrDisabled) {
			return result, nil // python: `if not cfg.enabled: return`
		}
		return result, fmt.Errorf("shikimori sync: user rates: %w", err)
	}
	if len(rates) == 0 {
		return result, nil // python: `if not rates: return`
	}
	report(SyncProgress{Phase: "rates", Message: fmt.Sprintf("Список загружен: %d записей", len(rates)), Done: len(rates), Total: len(rates)})

	rateByTarget := make(map[int64]UserRate, len(rates))
	for _, rate := range rates {
		rateByTarget[rate.TargetID] = rate
	}

	report(SyncProgress{Phase: "pull", Message: "Сопоставление локальных записей…", Total: len(rateByTarget)})
	s.log.Info("shikimori sync: remote rates", "count", len(rates))
	locals, err := s.repo.ListAllWithShikimoriID(ctx)
	s.log.Info("shikimori sync: local rows with shikimori_id", "count", len(locals))
	if err != nil {
		return result, fmt.Errorf("shikimori sync: local roster: %w", err)
	}
	for i := range locals {
		rec := locals[i]
		rate, ok := rateByTarget[*rec.ShikimoriID]
		if !ok {
			continue // local-only row: nothing to pull
		}
		delete(rateByTarget, *rec.ShikimoriID)
		if err := s.pullRate(ctx, &rec, rate, result); err != nil {
			return result, err
		}
		if (i+1)%10 == 0 || i == len(locals)-1 {
			report(SyncProgress{Phase: "pull", Message: fmt.Sprintf("Обновление записей: %d/%d", i+1, len(locals)), Done: i + 1, Total: len(locals)})
		}
	}

	if len(rateByTarget) > 0 {
		report(SyncProgress{Phase: "new", Message: fmt.Sprintf("Новые тайтлы: %d — загрузка метаданных…", len(rateByTarget)), Total: len(rateByTarget)})
	}
	if err := s.createMissing(ctx, rateByTarget, result, report); err != nil {
		return result, err
	}

	report(SyncProgress{Phase: "push", Message: "Отправка отложенных изменений…"})
	if err := s.pushDirty(ctx, result, report); err != nil {
		return result, err
	}
	report(SyncProgress{Phase: "done", Message: "Готово"})
	s.log.Info("shikimori sync: full sync complete",
		"updated", result.Updated, "created", result.Created,
		"pushed", result.Pushed, "conflicts", result.Conflicts)
	return result, nil
}

// pullRate refreshes one bound row from its remote rate (python
// shikimori_sync.py:49-61). A dirty row is a both-sides-changed
// conflict: the remote list state (status/score/rewatches) wins, the
// local episode progress wins — the push phase replays it upward.
func (s *Syncer) pullRate(ctx context.Context, rec *storage.AnimeProgress, rate UserRate, result *SyncResult) error {
	conflict := rec.Dirty
	if conflict {
		result.Conflicts++
	}

	status, score, rewatches := rate.Status, rate.Score, rate.Rewatches
	var episodes *string
	if !conflict && rate.Episodes > parseEpisode(rec.CurrentEpisode) {
		ep := strconv.Itoa(rate.Episodes)
		episodes = &ep
	}
	if err := s.repo.UpdateLocalStatus(ctx, rec.ID, &status, &score, &rewatches, episodes); err != nil {
		return fmt.Errorf("shikimori sync: pull %d: %w", rec.ID, err)
	}
	if rate.ID != 0 && (rec.ShikimoriRateID == nil || *rec.ShikimoriRateID != rate.ID) {
		if err := s.repo.SetRateID(ctx, rec.ID, rate.ID); err != nil {
			return fmt.Errorf("shikimori sync: pull rate id %d: %w", rec.ID, err)
		}
	}
	if !conflict {
		result.Updated++
	}
	return nil
}

// createMissing fetches metadata for the remote-only anime and inserts
// shikimori-sourced placeholder rows (python shikimori_sync.py:63-101);
// GetAnimesInfo chunks the ids at 50 per request internally.
func (s *Syncer) createMissing(ctx context.Context, rateByTarget map[int64]UserRate, result *SyncResult, report func(SyncProgress)) error {
	if len(rateByTarget) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(rateByTarget))
	for id := range rateByTarget {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	s.log.Info("shikimori sync: fetching metadata", "ids", len(ids))
	infos, err := s.client.GetAnimesInfo(ctx, ids)
	if err != nil {
		return fmt.Errorf("shikimori sync: animes info: %w", err)
	}
	s.log.Info("shikimori sync: metadata received", "infos", len(infos))
	for i, info := range infos {
		if err := s.createRemote(ctx, info, rateByTarget[info.ID]); err != nil {
			return err
		}
		result.Created++
		if (i+1)%10 == 0 || i == len(infos)-1 {
			report(SyncProgress{Phase: "new", Message: fmt.Sprintf("Новые тайтлы: %d/%d", i+1, len(infos)), Done: i + 1, Total: len(infos)})
		}
	}
	return nil
}

// createRemote inserts one placeholder row for a remote-only anime:
// source_id "shikimori", source_url the bare id (the binding search
// rebinds it onto a real provider source later), needs_correction set.
func (s *Syncer) createRemote(ctx context.Context, info Anime, rate UserRate) error {
	title := info.Russian
	if title == "" {
		title = info.Name
	}
	poster := s.client.PosterURL(&info)
	var posterPtr *string
	if poster != "" {
		posterPtr = &poster
	}
	shikiID := info.ID
	rec := storage.AnimeProgress{
		Title:           title,
		Poster:          posterPtr,
		SourceID:        "shikimori",
		SourceURL:       strconv.FormatInt(shikiID, 10),
		CurrentEpisode:  strconv.Itoa(rate.Episodes),
		ShikimoriID:     &shikiID,
		ShikimoriStatus: rate.Status,
		Score:           rate.Score,
		TotalEpisodes:   info.Episodes,
		Rewatches:       rate.Rewatches,
		NeedsCorrection: true,
		UpdatedAt:       time.Now().UTC(),
	}
	if rate.ID != 0 {
		rateID := rate.ID
		rec.ShikimoriRateID = &rateID
	}
	if err := s.repo.Upsert(ctx, &rec); err != nil {
		return fmt.Errorf("shikimori sync: create %d: %w", shikiID, err)
	}
	s.log.Info("shikimori sync: created row",
		"shikimori_id", shikiID, "title", title, "source_url", rec.SourceURL)
	return nil
}

// pushDirty replays the deferred-sync queue: every dirty bound row is
// pushed with its own episode counter and status; the flag clears only
// on success, so a failed replay stays queued for the next startup.
func (s *Syncer) pushDirty(ctx context.Context, result *SyncResult, report func(SyncProgress)) error {
	dirty, err := s.repo.ListDirty(ctx)
	if err != nil {
		return fmt.Errorf("shikimori sync: dirty roster: %w", err)
	}
	if len(dirty) > 0 {
		report(SyncProgress{Phase: "push", Message: fmt.Sprintf("Отправка: %d записей", len(dirty)), Total: len(dirty)})
	}
	var pushErr error
	for i := range dirty {
		rec := dirty[i]
		if rec.ShikimoriID == nil {
			continue // unbound row: nothing to push
		}
		rateID, err := s.pushRate(ctx, rec)
		if err != nil {
			pushErr = errors.Join(pushErr, fmt.Errorf("shikimori sync: replay %d: %w", rec.ID, err))
			continue
		}
		if err := s.persistSuccess(ctx, rec, rateID); err != nil {
			return fmt.Errorf("shikimori sync: replay persist %d: %w", rec.ID, err)
		}
		result.Pushed++
		if (i+1)%5 == 0 || i == len(dirty)-1 {
			report(SyncProgress{Phase: "push", Message: fmt.Sprintf("Отправка: %d/%d", i+1, len(dirty)), Done: i + 1, Total: len(dirty)})
		}
	}
	return pushErr
}

// pushRate replays one row's state to Shikimori: PATCH the stored rate
// or create one when the row carries no rate id.
func (s *Syncer) pushRate(ctx context.Context, rec storage.AnimeProgress) (int64, error) {
	episodes := parseEpisode(rec.CurrentEpisode)
	input := RateInput{Episodes: &episodes, Status: CanonicalStatus(rec.ShikimoriStatus)}
	if rec.ShikimoriRateID != nil {
		return s.client.UpdateRate(ctx, *rec.ShikimoriRateID, input)
	}
	return s.client.CreateRate(ctx, *rec.ShikimoriID, input)
}

// parseEpisode reads a row's episode counter (0 on junk — python's
// int() try/except parity).
func parseEpisode(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}
