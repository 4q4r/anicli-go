//go:build live

package tui

// LIVE probes for PR62 (binding persistence, provider log routing).
// Excluded from the hermetic default suite by the `live` build tag.
// Run manually:
//
//	go test -tags live -run TestLivePR62 -count=1 -v ./internal/tui/
//
// The screens are driven for real over a real store and the real
// provider roster; the network egress is the only external dependency.
// The shikimori PATCH payload proof is the hermetic exact-body pin
// (internal/shikimori/client_test.go, "want exactly episodes+status");
// the unauthenticated typed-skip path is TestLivePR61UnauthSkip.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/storage"
)

// bufferSink is a capturing stand-in for the TUI file logger
// (/tmp/anicli-tui.log): everything a provider or screen logs lands
// here, never on stderr.
type bufferSink struct {
	buf *bytes.Buffer
}

func newBufferSink() (*bufferSink, *slog.Logger) {
	b := &bufferSink{buf: &bytes.Buffer{}}
	return b, slog.New(slog.NewTextHandler(b.buf, nil))
}

func (b *bufferSink) String() string { return b.buf.String() }

// TestLivePR62BindPersistSkipSearchRebind (PR62 #2/#3): the provider
// pick persists the binding (needs_correction clears, the row moves),
// the re-entry resumes the session WITHOUT the fan-out, and
// «🔄 Перепривязать» re-runs the fan-out over the same record.
func TestLivePR62BindPersistSkipSearchRebind(t *testing.T) {
	_, deps := liveDeps(t)
	sink, sinkLog := newBufferSink()
	deps.Log = sinkLog

	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "черная лагуна"
	}

	// Real fan-out (the same call searchProviderVariants makes).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	perProvider := map[string][]contracts.SearchResult{}
	for _, p := range deps.Search.Providers() {
		results, err := deps.Search.Search(ctx, p.ID, query)
		if err != nil {
			t.Logf("  provider %-14s → err: %v", p.ID, err)
			continue
		}
		if len(results) > 0 {
			t.Logf("  provider %-14s → %d results", p.ID, len(results))
			perProvider[p.ID] = results
		}
	}
	if len(perProvider) < 2 {
		t.Fatalf("need results from 2+ providers, got %d", len(perProvider))
	}

	// The catalog placeholder the owner starts from.
	store := deps.History.(*realHistory).store
	rec := storage.AnimeProgress{
		Title: query, SourceID: "shikimori", SourceURL: "999999",
		ShikimoriID: ptrTo(int64(999999)), ShikimoriStatus: "planned",
		NeedsCorrection: true, UpdatedAt: nowUTC(),
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dbCancel()
	if err := store.Progress.Upsert(dbCtx, &rec); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	// Drive the rebind screen: settle the REAL results, select all, enter.
	rp := newRebindProgress(deps, &rec)
	t.Logf("rebind screen id: %s, title: %s", rp.ID(), rp.titleOverride)
	for id, results := range perProvider {
		rp.Update(providerResultMsg{provider: ProviderMeta{ID: id}, results: results})
	}
	// Providers that never answered (dead/CF-blocked) must settle too,
	// or the checklist never appears (Enter stays gated on pending).
	for _, row := range rp.rows {
		if rp.pending[row.ID] {
			rp.Update(providerResultMsg{provider: row, results: nil})
		}
	}
	next, _ := rp.Update(tea.KeyPressMsg{Code: 'a'})
	rp = next.(*rebindProgress)
	_, cmd := rp.Update(enter())
	rm, ok := cmd().(replaceMsg)
	if !ok {
		t.Fatalf("pick must replace into the session, got %#v", cmd())
	}
	sess, ok := rm.screen.(*sessionScreen)
	if !ok {
		t.Fatalf("pick must open the session, got %T", rm.screen)
	}
	t.Logf("STEP 1 pick → session %s on %s/%s (primary)", sess.ID(), sess.primary.SourceID, sess.primary.URL)

	// THE #2 PROOF: the record moved onto the checked primary and the
	// placeholder flag cleared.
	got, err := store.Progress.GetByShikimoriID(dbCtx, 999999)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Logf("STEP 1 store row: needs_correction=%v source=%s/%q bound_title=%v rate_id=%v",
		got.NeedsCorrection, got.SourceID, got.SourceURL, got.BoundTitle, got.ShikimoriRateID)
	if got.NeedsCorrection {
		t.Fatalf("needs_correction must clear after the pick")
	}
	if got.SourceID == "shikimori" || got.SourceURL == "999999" {
		t.Fatalf("the row must move onto the checked primary, got %s/%s", got.SourceID, got.SourceURL)
	}
	t.Logf("file log after the pick:\n%s", sink)

	// THE #3 PROOF (skip): re-entry of the bound record resumes the
	// session directly — no fan-out screen.
	items, err := loadHistory(deps)
	if err != nil {
		t.Fatalf("reload history: %v", err)
	}
	list := NewHistoryList(deps, "", items)
	for i := range len(items) {
		list.list.Jump(i)
		_, cmd := list.Update(enter())
		pm, ok := cmd().(pushMsg)
		if !ok {
			t.Fatalf("pick %d must push a screen", i)
		}
		if s, ok := pm.screen.(*sessionScreen); ok {
			t.Logf("STEP 2 re-entry of record id=%d → %s directly (no provider fan-out), resume episode %q",
				items[i].ID, s.ID(), items[i].CurrentEpisode)
			if items[i].ID != got.ID {
				continue
			}
			// The bound record: drive its session menu for STEP 3.
			s.loadEpisodesSync()
			item := sessionMenuItem(s, "rebind")
			if item == nil {
				t.Fatalf("resumed session must offer «Перепривязать»")
			}
			for j := range s.list.Menu().Items {
				if s.list.Menu().Items[j].ID == "rebind" {
					s.list.Jump(j)
					break
				}
			}
			_, rebindCmd := s.Update(enter())
			rm2, ok := rebindCmd().(replaceMsg)
			if !ok {
				t.Fatalf("rebind must replace, got %#v", rebindCmd())
			}
			rp2, ok := rm2.screen.(*rebindProgress)
			if !ok {
				t.Fatalf("rebind must open the fan-out, got %T", rm2.screen)
			}
			t.Logf("STEP 3 «Перепривязать» → fan-out %s over record id=%d (search re-runs by request)",
				rp2.ID(), rp2.rec.ID)
			if rp2.rec.ID != got.ID {
				t.Fatalf("the fan-out must carry the same record, got %+v", rp2.rec)
			}
			return
		}
		t.Logf("  record id=%d (%s) → %T", items[i].ID, items[i].Title, pm.screen)
	}
	t.Fatalf("the bound record was never reached")
}

// TestLivePR62AnilibDropsOnlyInFileLog (PR62 #4): an anilib search that
// drops contentless releases logs them to the FILE sink only — the TUI
// footer renders the clean counter, never raw log text.
func TestLivePR62AnilibDropsOnlyInFileLog(t *testing.T) {
	sink, sinkLog := newBufferSink()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	store, err := storage.Open(ctx, t.TempDir()+"/live62.db")
	if err != nil {
		t.Fatalf("open live store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	real, err := NewRealDeps(liveSettings(t), store, WithLogger(sinkLog))
	if err != nil {
		t.Fatalf("build live deps: %v", err)
	}
	t.Cleanup(real.Close)
	_ = real

	query := os.Getenv("ANICLI_LIVE_QUERY")
	if query == "" {
		query = "черная лагуна"
	}
	results, err := real.Deps.Search.Search(ctx, "anilib", query)
	if err != nil {
		t.Fatalf("live anilib search: %v", err)
	}
	t.Logf("live anilib search %q → %d results", query, len(results))

	logged := sink.String()
	if !strings.Contains(logged, "anilib: dropped contentless release") {
		t.Logf("file log:\n%s", logged)
		t.Fatalf("expected at least one contentless drop for %q in the file sink", query)
	}
	drops := 0
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "anilib: dropped contentless release") {
			drops++
			t.Logf("FILE LOG: %s", line)
		}
	}
	t.Logf("%d drop line(s) in the file sink", drops)

	// The search screen renders the settled results: the footer is the
	// clean counter — the drop lines never render into the TUI.
	fs := newFakeSearch()
	fs.providers = []ProviderMeta{{ID: "anilib", Name: "AniLibria"}}
	sp := NewSearchProgress(&Deps{Search: fs}, query)
	sp = settleSearch(sp,
		providerResultMsg{provider: fs.providers[0], results: results})
	view := sp.View().Content
	footer := ""
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Ответившие:") {
			footer = strings.TrimSpace(line)
		}
	}
	t.Logf("TUI FOOTER: %s", footer)
	if footer == "" {
		t.Fatalf("the counter footer must render, got:\n%s", view)
	}
	for _, banned := range []string{"contentless", "level=", "INFO", "release="} {
		if strings.Contains(view, banned) {
			t.Fatalf("raw log text %q rendered INTO the TUI:\n%s", banned, view)
		}
	}
}
