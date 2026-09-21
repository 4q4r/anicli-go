package tui

// Tests for the PR39 library background refresh: key «s» on the
// history filter screen (the library screen with per-status counts)
// starts a SILENT background check — no progress surfaces at press
// time, no status text on success — and re-renders the counts only
// when the reloaded data actually changed. Failures surface on the
// status line (loud), never silently.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/shikimori"
	"github.com/an0nx/anicli-go/internal/storage"
)

// fakeSyncFull records SyncFull invocations (the Deps.SyncFull seam).
type fakeSyncFull struct {
	calls int
	ctx   context.Context
	err   error
	// onSync mutates the world (e.g. fakeHistory.items) while the
	// sync runs — before the reload the refresh command performs.
	onSync func()
}

func (f *fakeSyncFull) sync(ctx context.Context, _ func(shikimori.SyncProgress)) (*shikimori.SyncResult, error) {
	f.calls++
	f.ctx = ctx
	if f.onSync != nil {
		f.onSync()
	}
	return &shikimori.SyncResult{}, f.err
}

// TestHistoryFilterKeySStartsBackgroundRefresh: pressing «s» dispatches
// the refresh command without touching the UI or fetching at press
// time; the command runs the sync bounded by a deadline context.
func TestHistoryFilterKeySStartsBackgroundRefresh(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)

	if filter.ID() != historyFilterID {
		t.Fatalf("library screen must keep its identity, got %q", filter.ID())
	}
	before := filter.View().Content
	_, cmd := filter.Update(ctrlR())
	after := filter.View().Content

	if cmd == nil {
		t.Fatal("pressing Ctrl+R must start the background refresh command")
	}
	if before != after {
		t.Fatalf("pressing Ctrl+R must not touch the UI at press time\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if sync.calls != 0 {
		t.Fatalf("the sync must not run at press time, ran %d times", sync.calls)
	}

	msg := cmd()
	if sync.calls != 1 {
		t.Fatalf("executing the command must run the sync once, ran %d", sync.calls)
	}
	if sync.ctx == nil || sync.ctx.Done() == nil {
		t.Fatal("the sync must run under a cancellable context")
	}
	if _, ok := sync.ctx.Deadline(); !ok {
		t.Fatal("the sync context must carry a deadline (bounded background work)")
	}
	if _, ok := msg.(historyRefreshMsg); !ok {
		t.Fatalf("command must settle with historyRefreshMsg, got %#v", msg)
	}
}

// TestHistoryFilterRefreshDedupWhileInFlight: a second Ctrl+R while a
// check is running is a silent no-op; after the check completes the
// key starts a new check.
func TestHistoryFilterRefreshDedupWhileInFlight(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)

	_, first := filter.Update(ctrlR())
	if first == nil {
		t.Fatal("first Ctrl+R must dispatch the refresh")
	}
	if _, second := filter.Update(ctrlR()); second != nil {
		t.Fatal("second s while a check is in flight must be a no-op")
	}
	first() // the check completes; its goroutine re-arms the flag
	_, third := filter.Update(ctrlR())
	if third == nil {
		t.Fatal("s must start a new check once the previous one completed")
	}
	third()
	if sync.calls != 2 {
		t.Fatalf("want exactly 2 syncs (dedup dropped the middle press), got %d", sync.calls)
	}
}

// TestHistoryFilterRefreshAppliesChangedCounts: when the reload brings
// changed data the screen re-renders with the new counts — and with NO
// success status text (zero visual noise on success). The cursor
// position survives the re-render.
func TestHistoryFilterRefreshAppliesChangedCounts(t *testing.T) {
	hist := &fakeHistory{items: historyItems()} // Смотрю [2]
	sync := &fakeSyncFull{}
	sync.onSync = func() {
		hist.items = append(hist.items, storage.AnimeProgress{
			ID: 4, Title: "Стальной алхимик", SourceID: "anilib", SourceURL: "u4",
			ShikimoriStatus: "watching", ShikimoriID: ptrTo(int64(14)),
		})
	}
	deps := &Deps{History: hist, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)
	filter.list.Jump(1) // cursor on the second row

	_, cmd := filter.Update(ctrlR())
	settled, ok := cmd().(historyRefreshMsg)
	if !ok {
		t.Fatal("command must settle with historyRefreshMsg")
	}
	filter.Update(settled) // the program loop delivers the message
	v := filter.View().Content
	if !strings.Contains(v, "Смотрю [3]") {
		t.Fatalf("refresh must re-render the new count:\n%s", v)
	}
	if strings.Contains(v, "Не удалось") || strings.Contains(v, "обновлено") {
		t.Fatalf("a successful refresh must not add status text:\n%s", v)
	}
	if filter.list.Cursor() != 1 {
		t.Fatalf("re-render must preserve the cursor, got %d", filter.list.Cursor())
	}
}

// TestHistoryFilterRefreshUnchangedStaysSilent: when the reload brings
// identical data the screen does NOTHING — byte-identical render.
func TestHistoryFilterRefreshUnchangedStaysSilent(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)

	before := filter.View().Content
	_, cmd := filter.Update(ctrlR())
	settled := cmd()
	filter.Update(settled) // the program loop delivers the message
	if got := filter.View().Content; got != before {
		t.Fatalf("identical data must not touch the screen\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestHistoryFilterRefreshErrorShowsStatusLine: a failed check keeps
// the rendered data, surfaces the error on the status line (fail loud)
// and reaches the logger (the file sink in production).
func TestHistoryFilterRefreshErrorShowsStatusLine(t *testing.T) {
	syncErr := errors.New("shikimori недоступен")
	sync := &fakeSyncFull{err: syncErr}
	var buf bytes.Buffer
	deps := &Deps{
		History:  &fakeHistory{items: historyItems()},
		SyncFull: sync.sync,
		Log:      slog.New(slog.NewTextHandler(&buf, nil)),
	}
	filter := newHistoryFilter(deps)

	_, cmd := filter.Update(ctrlR())
	settled := cmd()
	filter.Update(settled) // the program loop delivers the message
	v := filter.View().Content
	if !strings.Contains(v, "Не удалось обновить списки") || !strings.Contains(v, syncErr.Error()) {
		t.Fatalf("refresh failure must surface on the status line:\n%s", v)
	}
	if !strings.Contains(v, "Смотрю [2]") {
		t.Fatalf("failed refresh must keep the rendered data:\n%s", v)
	}
	if !strings.Contains(buf.String(), "history refresh failed") {
		t.Fatalf("refresh failure must reach the logger:\n%s", buf.String())
	}
}

// TestHistoryFilterRefreshNilSeamFailsLoud: without the sync seam
// (embedded builds) the press still dispatches, and the settled
// errSyncUnavailable surfaces on the status line.
func TestHistoryFilterRefreshNilSeamFailsLoud(t *testing.T) {
	deps := &Deps{History: &fakeHistory{items: historyItems()}}
	filter := newHistoryFilter(deps)

	_, cmd := filter.Update(ctrlR())
	if cmd == nil {
		t.Fatal("s must still dispatch when the sync seam is absent")
	}
	settled := cmd()
	hrm, ok := settled.(historyRefreshMsg)
	if !ok || !errors.Is(hrm.err, errSyncUnavailable) {
		t.Fatalf("nil seam must settle with errSyncUnavailable, got %#v", settled)
	}
	filter.Update(settled) // the program loop delivers the message
	if v := filter.View().Content; !strings.Contains(v, "Не удалось обновить списки") {
		t.Fatalf("nil seam must surface on the status line:\n%s", v)
	}
}

// TestHistoryFilterRefreshSuccessSupersedesStaleError: a failed check
// leaves the error on the status line, but the NEXT successful check —
// even with unchanged data — must supersede it and restore the hint.
func TestHistoryFilterRefreshSuccessSupersedesStaleError(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)

	// First check fails: the error lands on the status line.
	sync.err = errors.New("shikimori недоступен")
	_, failCmd := filter.Update(ctrlR())
	failMsg := failCmd()
	filter.Update(failMsg)
	if v := filter.View().Content; !strings.Contains(v, "Не удалось обновить списки") {
		t.Fatalf("precondition: the failed check must show its error:\n%s", v)
	}

	// Second check succeeds, data unchanged: the stale error must go.
	sync.err = nil
	_, okCmd := filter.Update(ctrlR())
	okMsg := okCmd()
	filter.Update(okMsg)
	v := filter.View().Content
	if strings.Contains(v, "Не удалось обновить списки") {
		t.Fatalf("a fresh success must supersede the stale error:\n%s", v)
	}
	if !strings.Contains(v, historyFilterHint) {
		t.Fatalf("the hint line must be restored after the supersede:\n%s", v)
	}
}

// TestHistoryFilterRefreshRearmsAfterDroppedResult: the re-arm lives
// in the refresh command itself, so a result dropped while the user
// navigated elsewhere cannot permanently wedge the key.
func TestHistoryFilterRefreshRearmsAfterDroppedResult(t *testing.T) {
	sync := &fakeSyncFull{}
	deps := &Deps{History: &fakeHistory{items: historyItems()}, SyncFull: sync.sync}
	filter := newHistoryFilter(deps)

	_, cmd := filter.Update(ctrlR())
	cmd() // completes; the settled message is NEVER delivered to the filter
	if _, next := filter.Update(ctrlR()); next == nil {
		t.Fatal("s must re-arm even when the settled result was delivered elsewhere")
	}
}
