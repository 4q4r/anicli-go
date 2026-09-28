package tui

// PR113 «Продолжить просмотр»: the pure label/target computation of
// the root menu's continue row. The row jumps to the NEXT unwatched
// episode of the most recently watched anime (owner ruling 2: «да, но
// надо чтобы он не мешал другим пунктам меню и напоминал тайтл и
// серию»).

import (
	"math"
	"strconv"
	"strings"

	"github.com/an0nx/anicli-go/internal/i18n"
	"github.com/an0nx/anicli-go/internal/storage"
)

// maxContinueEpisode bounds the arithmetic successor so a hostile or
// corrupted episode label ("1e300") cannot overflow the int64
// conversion below — such labels simply stay.
const maxContinueEpisode = float64(math.MaxInt32)

// ContinueTarget computes the episode the «Продолжить» row jumps to:
// N+1 — the next unwatched episode after the saved CurrentEpisode —
// except:
//   - an episode left incomplete (0 < progress < total seconds) stays
//     on N: mpv's watch-later file restores the playback position
//     where it stopped (--save-position-on-quit, PR113 #1);
//   - a known episode total already exhausted stays on N (there is
//     no next episode to offer);
//   - a non-integer episode label ("OVA", "5.5") has no arithmetic
//     successor and stays.
func ContinueTarget(rec storage.AnimeProgress) string {
	cur := strings.TrimSpace(rec.CurrentEpisode)
	if cur == "" {
		return ""
	}
	if rec.ProgressSeconds > 0 && rec.TotalSeconds > 0 &&
		rec.ProgressSeconds < rec.TotalSeconds {
		return cur
	}
	n, err := strconv.ParseFloat(cur, 64)
	if err != nil || n != math.Trunc(n) || n < 0 || n > maxContinueEpisode {
		return cur
	}
	next := int64(n) + 1
	if rec.TotalEpisodes > 0 && next > int64(rec.TotalEpisodes) {
		return cur
	}
	return strconv.FormatInt(next, 10)
}

// ContinueLabel renders the row text: the localized «Продолжить» form
// with the bound title (the same name the manual history flow resumes
// by) and the target episode; the dim dash form when there is nothing
// to continue.
func ContinueLabel(rec *storage.AnimeProgress) string {
	if rec == nil {
		return i18n.T("menu.continue_empty")
	}
	ep := ContinueTarget(*rec)
	if ep == "" {
		return i18n.T("menu.continue_empty")
	}
	return i18n.T("menu.continue", i18n.Vals{
		"title": derefStr(rec.BoundTitle, rec.Title),
		"ep":    ep,
	})
}
