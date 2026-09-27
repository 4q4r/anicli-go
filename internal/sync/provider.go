// Package sync is the dual-provider progress-sync layer (PR112): a
// Provider abstraction over the tracker pushes, the two
// implementations (Shikimori — the existing python-parity push, and
// MyAnimeList — via the card-derived id mapping) and the dispatcher
// that fans one episode-progress event out to every enabled tracker
// (owner ruling: Shikimori and MyAnimeList may be authenticated at the
// same time and sync in parallel; sync UX is identical whichever one
// the user signed in with).
//
// The package owns the shared resolution (local row lookup, the
// no-rollback guard, the planned→watching play-time normalization) so
// both providers observe exactly the same inputs, and reports typed
// per-provider verdicts the TUI renders through i18n.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/mal"
	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// ErrNotOnMAL is the typed mapping miss: the title carries no
// MyAnimeList id (not on MAL, or the card exposes no link). The
// dispatcher downgrades it to a skip verdict, never a failure —
// watching a Shikimori-only title must not read as an error.
var ErrNotOnMAL = errors.New("mal: title has no myanimelist id")

// Note is the typed skip reason of a verdict.
type Note int

const (
	// NoteNone reports "not a skip": synced, or failed with Err.
	NoteNone Note = iota
	// NoteAuthRequired marks an enabled provider without credentials —
	// the push was never attempted.
	NoteAuthRequired
	// NoteNotOnMAL marks a title with no MAL mapping — nothing was
	// pushed.
	NoteNotOnMAL
)

// Verdict is one provider's outcome of a dispatch round.
type Verdict struct {
	// Key is the provider identity ("shikimori" / "myanimelist") — the
	// i18n prefix the TUI renders the line from.
	Key string
	// Synced reports a successful push.
	Synced bool
	// Note is the typed skip reason (NoteNone on success/failure).
	Note Note
	// Err is the push failure (nil on success and on typed skips).
	Err error
}

// Report is the settled outcome of one dispatch round.
type Report struct {
	// Participated reports whether any tracker is enabled; false keeps
	// the legacy single typed skip note at the call site.
	Participated bool
	// NoRollback reports the shared counter guard: the local progress
	// is already ahead, nothing was pushed to anyone. Prior carries
	// the stored counter for the verdict line.
	NoRollback bool
	// Prior is the local episode counter when NoRollback fired.
	Prior int
	// Verdicts are the per-provider outcomes, dispatcher order.
	Verdicts []Verdict
}

// PushRequest is the resolved per-event push context shared by every
// provider: one local row lookup, one guard, one status decision.
type PushRequest struct {
	// ShikimoriID is the canonical title binding.
	ShikimoriID int64
	// Episode is the episode counter to push.
	Episode int
	// Status is the play-resolved rate status ("watching" unless the
	// row carried another canonical status).
	Status string
	// RateID is the stored Shikimori rate id (0 = create).
	RateID int64
	// AnimeID is the local row id (for rate-id persistence).
	AnimeID int64
}

// Provider is one tracker's episode-progress push surface.
type Provider interface {
	// Key is the provider identity ("shikimori" / "myanimelist").
	Key() string
	// Participates reports whether the tracker is switched on in
	// settings — a disabled tracker is silent (no verdict line).
	Participates() bool
	// Ready reports whether usable credentials exist.
	Ready() bool
	// SyncEpisodeProgress pushes the episode counter.
	SyncEpisodeProgress(ctx context.Context, req PushRequest) error
}

// History is the local-progress seam the dispatcher and the Shikimori
// implementation need (satisfied by the TUI HistoryService).
type History interface {
	// GetByShikimoriID loads the record bound to a shikimori anime,
	// nil when none is bound.
	GetByShikimoriID(ctx context.Context, shikimoriID int64) (*storage.AnimeProgress, error)
	// SetRateID persists a newly created rate id.
	SetRateID(ctx context.Context, animeID, rateID int64) error
}

// Dispatcher fans one episode-progress event out to every enabled
// tracker. Construct with NewDispatcher; safe for concurrent use.
type Dispatcher struct {
	providers []Provider
	history   History
	log       *slog.Logger
}

// NewDispatcher builds the dispatcher over the tracker roster. history
// may be nil (the guards run without the local row: pushes proceed);
// logger may be nil (slog.Default).
func NewDispatcher(history History, logger *slog.Logger, providers ...Provider) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{providers: providers, history: history, log: logger}
}

// SyncEpisodeProgress resolves the shared push context once (local row,
// no-rollback guard, play-time status normalization) and pushes to every
// enabled tracker, returning one verdict per participating provider.
//
// Verdicts:
//   - no tracker enabled   -> zero Report (the caller keeps its legacy
//     typed skip note);
//   - episode behind local -> Participated + NoRollback, no verdicts,
//     no provider calls;
//   - an enabled-but-unauthenticated tracker yields a NoteAuthRequired
//     verdict without a push attempt;
//   - ErrNotOnMAL downgrades to NoteNotOnMAL (typed skip, not failure);
//     everything else surfaces as the verdict's Err.
func (d *Dispatcher) SyncEpisodeProgress(ctx context.Context, shikimoriID int64, episode int) Report {
	var active []Provider
	for _, p := range d.providers {
		if p != nil && p.Participates() {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		d.log.Info("sync: no tracker enabled; progress push skipped", "episode", episode)
		return Report{}
	}

	req := PushRequest{ShikimoriID: shikimoriID, Episode: episode, Status: "watching"}
	if d.history != nil {
		rec, err := d.history.GetByShikimoriID(ctx, shikimoriID)
		if err != nil {
			// A lookup hiccup must not block the push: the stored
			// rate id would have avoided a duplicate rate, so worst
			// case the provider creates one (the id persists on the
			// next round) — python behaves the same way.
			d.log.Warn("sync: local row lookup failed", "error", err)
		}
		if rec != nil {
			req.AnimeID = rec.ID
			if rec.ShikimoriRateID != nil {
				req.RateID = *rec.ShikimoriRateID
			}
			if st := shikimori.CanonicalStatus(rec.ShikimoriStatus); st != "" {
				if st == "plan_to_watch" {
					st = "watching" // python: planned → watching on play
				}
				req.Status = st
			}
			if prior := parseEpisodeCounter(rec.CurrentEpisode); prior > episode {
				d.log.Info("sync: episode behind local progress; push skipped",
					"episode", episode, "prior", prior)
				return Report{Participated: true, NoRollback: true, Prior: prior}
			}
		}
	}

	verdicts := make([]Verdict, 0, len(active))
	for _, p := range active {
		if !p.Ready() {
			verdicts = append(verdicts, Verdict{Key: p.Key(), Note: NoteAuthRequired})
			continue
		}
		err := p.SyncEpisodeProgress(ctx, req)
		switch {
		case err == nil:
			verdicts = append(verdicts, Verdict{Key: p.Key(), Synced: true})
		case errors.Is(err, ErrNotOnMAL):
			verdicts = append(verdicts, Verdict{Key: p.Key(), Note: NoteNotOnMAL})
		default:
			verdicts = append(verdicts, Verdict{Key: p.Key(), Err: err})
		}
	}
	return Report{Participated: true, Verdicts: verdicts}
}

// ShikimoriPusher is the Shikimori tracker surface the implementation
// pushes through (satisfied by the TUI ShikimoriService).
type ShikimoriPusher interface {
	// Enabled reports whether the integration is active.
	Enabled() bool
	// Mode reports the auth mode diagnostic ("none"/"cookie"/"bearer").
	Mode() string
	// UpdateEpisodes pushes {episodes, status}, creating the rate when
	// rateID is 0 and returning the rate id.
	UpdateEpisodes(ctx context.Context, shikimoriID, rateID int64, episodes int, status string) (int64, error)
}

// ShikimoriSync is the Shikimori Provider: the python-parity
// immediate push (episodes=N, ensured status) with the created-rate-id
// persistence (the write survives caller cancellation).
type ShikimoriSync struct {
	Shiki   ShikimoriPusher
	History History
	Log     *slog.Logger
}

// Key implements Provider.
func (p *ShikimoriSync) Key() string { return "shikimori" }

// Participates implements Provider.
func (p *ShikimoriSync) Participates() bool { return p.Shiki != nil && p.Shiki.Enabled() }

// Ready implements Provider: cookie and bearer modes carry
// credentials; "none"/"disabled" do not.
func (p *ShikimoriSync) Ready() bool {
	return p.Shiki != nil && p.Shiki.Mode() != "none" && p.Shiki.Mode() != "disabled"
}

// SyncEpisodeProgress implements Provider.
func (p *ShikimoriSync) SyncEpisodeProgress(ctx context.Context, req PushRequest) error {
	newRate, err := p.Shiki.UpdateEpisodes(ctx, req.ShikimoriID, req.RateID, req.Episode, req.Status)
	if err != nil {
		return fmt.Errorf("shikimori push: %w", err)
	}
	// A created rate id persists so the next push PATCHes instead of
	// duplicating (the id write survives caller cancellation).
	if req.RateID == 0 && newRate != 0 && req.AnimeID != 0 && p.History != nil {
		if err := p.History.SetRateID(context.WithoutCancel(ctx), req.AnimeID, newRate); err != nil {
			p.logger().Warn("shikimori sync: persist rate id failed", "anime", req.AnimeID, "error", err)
		}
	}
	return nil
}

func (p *ShikimoriSync) logger() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// MALPusher is the MyAnimeList surface the implementation pushes
// through (satisfied by *mal.Client).
type MALPusher interface {
	// Enabled reports whether the integration is active.
	Enabled() bool
	// Authenticated reports whether a bearer token exists.
	Authenticated() bool
	// UpdateMyListStatus PUTs the list status (creates when absent).
	UpdateMyListStatus(ctx context.Context, animeID int64, in mal.ListInput) error
}

// MALIDResolver resolves the shikimori→MAL id mapping: cache first,
// then the Shikimori card (persisting the fresh mapping).
type MALIDResolver interface {
	ResolveMALID(ctx context.Context, shikimoriID int64) (int64, error)
}

// MALSync is the MyAnimeList Provider: resolve the MAL id for the
// shikimori-bound title, then PUT the mapped status + counter.
type MALSync struct {
	MAL      MALPusher
	Resolver MALIDResolver
	Log      *slog.Logger
}

// Key implements Provider.
func (p *MALSync) Key() string { return "myanimelist" }

// Participates implements Provider.
func (p *MALSync) Participates() bool { return p.MAL != nil && p.MAL.Enabled() }

// Ready implements Provider.
func (p *MALSync) Ready() bool { return p.MAL != nil && p.MAL.Authenticated() }

// SyncEpisodeProgress implements Provider. A mapping miss is the
// typed ErrNotOnMAL (the dispatcher downgrades it to a skip verdict).
func (p *MALSync) SyncEpisodeProgress(ctx context.Context, req PushRequest) error {
	malID, err := p.Resolver.ResolveMALID(ctx, req.ShikimoriID)
	if err != nil {
		return err
	}
	status, rewatching := mal.StatusToMAL(req.Status)
	if status == "" {
		status = "watching" // unknown stored status: the play default
	}
	in := mal.ListInput{Status: status, NumWatchedEpisodes: req.Episode, IsRewatching: rewatching}
	if err := p.MAL.UpdateMyListStatus(ctx, malID, in); err != nil {
		return fmt.Errorf("mal push: %w", err)
	}
	p.logger().Debug("mal sync: progress pushed",
		"shikimori_id", req.ShikimoriID, "mal_id", malID,
		"episode", req.Episode, "status", status, "rewatching", rewatching)
	return nil
}

func (p *MALSync) logger() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// CardSource fetches the Shikimori anime card (satisfied by the TUI
// ShikimoriService.GetAnime).
type CardSource interface {
	GetAnime(ctx context.Context, shikimoriID int64) (*shikimori.Anime, error)
}

// IDMapStore is the durable mapping cache (satisfied by
// storage.MALMapRepo).
type IDMapStore interface {
	Get(ctx context.Context, shikimoriID int64) (int64, bool, error)
	Set(ctx context.Context, shikimoriID, malID int64) error
}

// CardMALIDResolver is the default MALIDResolver: the cached mapping
// first, then the Shikimori card's MAL carriers (mal_id field, links
// array), persisting fresh resolutions.
type CardMALIDResolver struct {
	Cards CardSource
	Store IDMapStore
	Log   *slog.Logger
}

// ResolveMALID implements MALIDResolver. Verdicts:
//   - cache hit -> the cached id, no network;
//   - card fetched with a mapping -> the id, persisted (persist errors
//     warn: the push still proceeds);
//   - card without any mapping -> ErrNotOnMAL (typed skip);
//   - card fetch failure -> the transport error (a transient failure
//     must not poison the cache as "not on MAL").
func (r *CardMALIDResolver) ResolveMALID(ctx context.Context, shikimoriID int64) (int64, error) {
	log := r.logger()
	if r.Store != nil {
		id, ok, err := r.Store.Get(ctx, shikimoriID)
		if err != nil {
			log.Warn("mal map: cache read failed", "shikimori_id", shikimoriID, "error", err)
		} else if ok {
			return id, nil
		}
	}
	if r.Cards == nil {
		return 0, fmt.Errorf("%w: no card source wired", ErrNotOnMAL)
	}
	card, err := r.Cards.GetAnime(ctx, shikimoriID)
	if err != nil {
		return 0, fmt.Errorf("mal map: fetch card %d: %w", shikimoriID, err)
	}
	id := card.MALID()
	if id == 0 {
		return 0, ErrNotOnMAL
	}
	if r.Store != nil {
		if err := r.Store.Set(context.WithoutCancel(ctx), shikimoriID, id); err != nil {
			log.Warn("mal map: cache write failed", "shikimori_id", shikimoriID, "mal_id", id, "error", err)
		}
	}
	return id, nil
}

func (r *CardMALIDResolver) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

// parseEpisodeCounter reads a row's episode counter (0 on junk —
// python's int() try/except parity).
func parseEpisodeCounter(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}
