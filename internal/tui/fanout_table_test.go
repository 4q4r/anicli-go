package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// TestSearchErrLabelMapping pins the PR98 short-label classifier: every
// error class the fan-out can surface maps onto ONE short RU label for
// the table cell — the full error text stays in the file log only.
func TestSearchErrLabelMapping(t *testing.T) {
	t.Parallel()

	watchdog := fmt.Errorf("%w: no first byte in 10s", contracts.ErrProviderTimeout)
	wrapped404 := fmt.Errorf("search: %w", &netclient.StatusError{StatusCode: 404, Status: "404 Not Found"})

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"fan-out budget expiry", fmt.Errorf("%w: %w", errSearchTimeout, context.DeadlineExceeded), labelTimeout},
		{"bare deadline", context.DeadlineExceeded, labelTimeout},
		{"netclient watchdog", watchdog, labelTimeout},
		{"cloudflare challenge", &netclient.CFChallengeError{URL: "https://x"}, labelCF},
		{"challenge wrapped by provider", contracts.WrapProvider("kodik", contracts.OpSearch, 403, &netclient.CFChallengeError{URL: "https://x"}), labelCF},
		{"x509 expired", errors.New("Get \"https://x\": x509: certificate has expired or is not yet valid"), labelCert},
		{"x509 wrapped", fmt.Errorf("netclient: do: %w", errors.New("tls: x509: certificate has expired")), labelCert},
		{"sentinel not found", fmt.Errorf("search: %w", contracts.ErrNotFound), labelNotFound},
		{"http 404", wrapped404, labelNotFound},
		{"http 502", &netclient.StatusError{StatusCode: 502, Status: "502 Bad Gateway"}, labelDown},
		{"http 521", &netclient.StatusError{StatusCode: 521, Status: "521 Web Server Is Down"}, labelDown},
		{"connection refused", fmt.Errorf("do: dial tcp 1.2.3.4:443: connect: connection refused"), labelDown},
		{"http 429", &netclient.StatusError{StatusCode: 429, Status: "429 Too Many Requests"}, labelRateLimit},
		{"unclassified", errors.New("provider down"), labelError},
		{"unclassified wrapped", fmt.Errorf("search: animego: %w", errors.New("eof")), labelError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := searchErrLabel(tc.err); got != tc.want {
				t.Fatalf("searchErrLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// tableBox extracts the bordered table block from a view: from the
// top border line through the bottom border line (the view indents
// the box, so leading whitespace is trimmed before matching).
func tableBox(v string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(v, "\n") {
		trim := strings.TrimLeft(line, " ")
		switch {
		case strings.HasPrefix(trim, "┌"):
			in = true
			out = append(out, line)
		case strings.HasPrefix(trim, "└"):
			out = append(out, line)
			return out
		case in:
			out = append(out, line)
		}
	}
	return out
}

// fixtureRows is the PR98 fan-out fixture: a long error class label, a
// wide RU provider name, and every status shape (ok / timeout / error
// / pending count dash).
func fixtureRows() []fanoutRowData {
	return []fanoutRowData{
		{name: "ОченьДлинноеИмяПровайдера", status: "✗ сертификат истёк", count: "0"},
		{name: "AnimeGO", status: "✓ Завершено", count: "12"},
		{name: "AniLib", status: "⏱ таймаут", count: "—"},
		{name: "Kodik", status: "✗ нет данных", count: "0"},
	}
}

// TestRenderFanoutTableBox pins the PR98 bordered table: full box
// border, row separators between every row, identical display width on
// every line (the python «ровные поля» ruling), truncation with … and
// the right-aligned count column.
func TestRenderFanoutTableBox(t *testing.T) {
	t.Parallel()

	t.Run("full box border with a separator between every row", func(t *testing.T) {
		t.Parallel()
		box := tableBox(renderFanoutTable(fixtureRows(), 0))
		rows := len(fixtureRows())
		// top + header + header-sep + rows + (rows-1) seps + bottom.
		if len(box) != 2+1+1+rows+rows-1 {
			t.Fatalf("box must carry top/header/sep/bottom lines, got %d lines:\n%s", len(box), strings.Join(box, "\n"))
		}
		if !strings.HasPrefix(box[0], "┌") || !strings.HasSuffix(box[0], "┐") {
			t.Fatalf("top border must open the box, got %q", box[0])
		}
		if !strings.HasPrefix(box[len(box)-1], "└") || !strings.HasSuffix(box[len(box)-1], "┘") {
			t.Fatalf("bottom border must close the box, got %q", box[len(box)-1])
		}
		seps := 0
		for _, line := range box[1 : len(box)-1] {
			if strings.HasPrefix(line, "├") {
				seps++
			}
		}
		if seps != rows { // header separator + rows-1 row separators
			t.Fatalf("every row must sit on a separator line, got %d of %d:\n%s", seps, rows, strings.Join(box, "\n"))
		}
	})

	t.Run("every line renders the identical display width", func(t *testing.T) {
		t.Parallel()
		box := tableBox(renderFanoutTable(fixtureRows(), 0))
		want := lipgloss.Width(box[0])
		for i, line := range box {
			if w := lipgloss.Width(line); w != want {
				t.Fatalf("line %d width %d != %d ( uneven columns — the PR98 core regression):\n%s", i, w, want, strings.Join(box, "\n"))
			}
		}
	})

	t.Run("narrow terminal truncates cells instead of overflowing", func(t *testing.T) {
		t.Parallel()
		const avail = 40
		box := tableBox(renderFanoutTable(fixtureRows(), avail))
		long := fixtureRows()[0].name
		for i, line := range box {
			if w := lipgloss.Width(line); w != avail {
				t.Fatalf("line %d width %d != %d:\n%s", i, w, avail, strings.Join(box, "\n"))
			}
		}
		if strings.Contains(strings.Join(box, "\n"), long) {
			t.Fatalf("the wide provider name must truncate, got:\n%s", strings.Join(box, "\n"))
		}
		if !strings.Contains(strings.Join(box, "\n"), "…") {
			t.Fatalf("truncated cells must carry the … marker, got:\n%s", strings.Join(box, "\n"))
		}
	})

	t.Run("count column is right-aligned", func(t *testing.T) {
		t.Parallel()
		box := tableBox(renderFanoutTable(fixtureRows(), 0))
		for _, line := range box {
			if strings.Contains(line, "AnimeGO") {
				if !strings.HasSuffix(line, "12 │") {
					t.Fatalf("the count cell must end at the column's right edge, got %q", line)
				}
				return
			}
		}
		t.Fatalf("fixture row missing from the box:\n%s", strings.Join(box, "\n"))
	})

	t.Run("header carries the three column labels", func(t *testing.T) {
		t.Parallel()
		box := tableBox(renderFanoutTable(fixtureRows(), 0))
		for _, want := range []string{"Провайдер", "Статус", "Результатов"} {
			if !strings.Contains(box[1], want) {
				t.Fatalf("header missing %q, got %q", want, box[1])
			}
		}
	})

	t.Run("wide terminal renders the full fixture without truncation", func(t *testing.T) {
		t.Parallel()
		joined := renderFanoutTable(fixtureRows(), 0)
		if !strings.Contains(joined, fixtureRows()[0].name) {
			t.Fatalf("a roomy terminal must not truncate, got:\n%s", joined)
		}
		if !strings.Contains(joined, "✗ сертификат истёк") {
			t.Fatalf("a roomy terminal must fit the longest label, got:\n%s", joined)
		}
	})
}

// TestSearchFanoutViewSettleConsistency pins the PR98 live table:
// settle-by-settle every rendered box stays rectangular AND keeps the
// same width (columns are computed from ALL rows, so partial settles
// never reflow), markers ✓/✗/⏱ land per class, and cells carry the
// short labels only.
func TestSearchFanoutViewSettleConsistency(t *testing.T) {
	t.Parallel()

	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "broke", Name: "ОченьДлинноеИмяПровайдера"},
		{ID: "slow", Name: "AniLib"},
	}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}

	deps := hybridDeps(fs, nil, nil, nil)
	sp := NewSearchProgress(deps, "наруто")

	boxWidth := func(v string) (int, bool) {
		box := tableBox(v)
		if len(box) == 0 {
			return 0, false
		}
		want := lipgloss.Width(box[0])
		for _, line := range box {
			if w := lipgloss.Width(line); w != want {
				t.Fatalf("uneven box: %q has %d cols vs %d:\n%s", line, w, want, v)
			}
		}
		return want, true
	}

	width, ok := boxWidth(sp.View().Content)
	if !ok {
		t.Fatalf("the pending table must render before any settle:\n%s", sp.View().Content)
	}

	settle := func(row ProviderMeta, res []contracts.SearchResult, err error) {
		_, _ = sp.Update(withProvider(providerResultMsg{results: res, err: err}, row))
		got, ok := boxWidth(sp.View().Content)
		if !ok {
			t.Fatalf("table vanished after settling %s", row.ID)
		}
		if got != width {
			t.Fatalf("settling %s reflowed the table: %d != %d\n%s", row.ID, got, width, sp.View().Content)
		}
	}

	settle(fs.providers[0], fs.results["animego"], nil) // ✓ Завершено
	if v := sp.View().Content; !strings.Contains(v, "✓ Завершено") {
		t.Fatalf("ok row must render its marker, got:\n%s", v)
	}
	settle(fs.providers[1], nil, errors.New("provider down")) // ✗ ошибка
	if v := sp.View().Content; !strings.Contains(v, "✗ ошибка") || strings.Contains(v, "provider down") {
		t.Fatalf("error rows render the short label only, got:\n%s", v)
	}
	settle(fs.providers[2], nil, fmt.Errorf("%w: %w", errSearchTimeout, context.DeadlineExceeded)) // ⏱ таймаут
	v := sp.View().Content
	if !strings.Contains(v, "⏱ таймаут") {
		t.Fatalf("timeout row must render the ⏱ marker, got:\n%s", v)
	}
	for _, want := range []string{"Ответившие: 1/3 провайдеров", "Всего результатов: 1"} {
		if !strings.Contains(v, want) {
			t.Fatalf("footer missing %q, got:\n%s", want, v)
		}
	}
}

// TestSearchFanoutViewNarrowTerminal pins the PR98 resize behavior:
// the tracked WindowSizeMsg reaches the screen through the App (which
// previously dropped it), the box compresses under the budget, and
// cells truncate instead of overflowing.
func TestSearchFanoutViewNarrowTerminal(t *testing.T) {
	t.Parallel()

	fs := newFakeSearch()
	fs.providers = []ProviderMeta{
		{ID: "animego", Name: "AnimeGO"},
		{ID: "broke", Name: "ОченьДлинноеИмяПровайдера"},
	}
	fs.results["animego"] = []contracts.SearchResult{{Title: "Наруто", URL: "u1", SourceID: "animego"}}
	fs.errs["broke"] = errors.New("boom")

	deps := hybridDeps(fs, nil, nil, nil)
	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)
	model = drive(model, tea.WindowSizeMsg{Width: 44, Height: 30})

	v := topOf(model).View().Content
	box := tableBox(v)
	if len(box) == 0 {
		t.Fatalf("table missing:\n%s", v)
	}
	for _, line := range box {
		if w := lipgloss.Width(strings.TrimLeft(line, " ")); w > 44 {
			t.Fatalf("box overflows the 44-col terminal: %d cols\n%s", w, v)
		}
	}
	if !strings.Contains(v, "…") {
		t.Fatalf("the wide provider name must truncate under the budget, got:\n%s", v)
	}
	if strings.Contains(v, "ОченьДлинноеИмяПровайдера") {
		t.Fatalf("the untruncated name must not fit a 44-col terminal, got:\n%s", v)
	}
}

// TestSearchFanoutFullErrorOnlyInFileLog pins the owner ruling: the
// table cell carries the short label; the FULL error goes to the file
// log's per-settle line and nowhere else on screen.
func TestSearchFanoutFullErrorOnlyInFileLog(t *testing.T) {
	t.Parallel()

	full := "вот такая вот очень длинная ошибка провайдера целиком"
	fs := newFakeSearch()
	fs.providers = fs.providers[:1]
	fs.errs["animego"] = errors.New(full)

	var buf bytes.Buffer
	deps := hybridDeps(fs, nil, nil, nil)
	deps.Log = slog.New(slog.NewTextHandler(&buf, nil))

	model := drive(NewApp(NewRootScreen(deps), deps, testLogger()),
		pushMsg{screen: NewSearchProgress(deps, "наруто")})
	model = drainCmds(model)

	v := topOf(model).View().Content
	if strings.Contains(v, full) {
		t.Fatalf("the full error must never render in the table, got:\n%s", v)
	}
	if !strings.Contains(v, "✗ ошибка") {
		t.Fatalf("the cell must carry the short label, got:\n%s", v)
	}
	if !strings.Contains(buf.String(), full) {
		t.Fatalf("the file log must carry the full error, got:\n%s", buf.String())
	}
}

// TestFanoutColumnWidths pins the PR98 width algorithm: natural widths
// when the terminal is wide, proportional compression above the
// minimums, and status-first squeezing below them.
func TestFanoutColumnWidths(t *testing.T) {
	t.Parallel()

	mins := fanoutMinWidths
	floors := fanoutHardFloors

	tests := []struct {
		name       string
		natural    [3]int
		availInner int
		want       [3]int
	}{
		{
			name:       "wide terminal keeps natural widths",
			natural:    [3]int{14, 22, 13},
			availInner: 200,
			want:       [3]int{14, 22, 13},
		},
		{
			name:       "narrow content lifts columns to the minimums",
			natural:    [3]int{11, 10, 13},
			availInner: 200,
			want:       [3]int{12, 14, 13},
		},
		{
			name:       "compression above minimums is proportional to headroom",
			natural:    [3]int{20, 40, 13},
			availInner: 43, // total 73 → 30 columns of overflow, spare 8+26+1=35
			want:       [3]int{14, 21, 8},
		},
		{
			name:       "below minimums status squeezes first",
			natural:    [3]int{20, 40, 13},
			availInner: 26, // total 36 at minimums → status pays 10 down to its floor
			want:       [3]int{12, 8, 6},
		},
		{
			name:       "degenerate terminal clamps at the floors",
			natural:    [3]int{20, 40, 13},
			availInner: 10,
			want:       [3]int{8, 8, 6},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fanoutColumnWidths(tc.natural, mins, floors, tc.availInner)
			if got != tc.want {
				t.Fatalf("fanoutColumnWidths = %v, want %v", got, tc.want)
			}
		})
	}
}
