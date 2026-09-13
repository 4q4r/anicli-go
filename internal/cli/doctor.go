package cli

// The search-based environment diagnostics (PR24) live here: the
// doctor command probes every registered provider with two real
// queries instead of merely listing them, renders a live
// Provider|Статус|Результатов table and marks unconfigured or excluded
// providers ОТКЛЮЧЁН without probing them.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// doctorQueries are the two probe queries every provider must answer
// (PR24): the Russian and the romaji spelling of the same title.
var doctorQueries = [2]string{"Наруто", "Naruto"}

// defaultDoctorTimeout is the fallback per-provider budget when the
// settings carry none.
const defaultDoctorTimeout = 30 * time.Second

// doctorCheck is one settled provider row.
type doctorCheck struct {
	id      string
	results int
	err     error
}

// doctorProbe runs one provider's two-query check inside the budget.
// It is a package-level seam so tests can verify the doctor table
// without network egress; the production implementation performs the
// real searches.
var doctorProbe = func(ctx context.Context, p contracts.Provider, budget time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	total := 0
	for _, q := range doctorQueries {
		res, err := p.Search(ctx, q)
		if err != nil {
			return total, err
		}
		total += len(res)
	}
	return total, nil
}

// startupNotices builds the PR24 startup warning lines — one per
// provider disabled for missing configuration.
func startupNotices(cfg config.Settings) []string {
	var out []string
	for _, d := range providers.UnconfiguredProviders(cfg) {
		out = append(out, fmt.Sprintf("⚠ Провайдер '%s' отключён: %s", d.ID, d.Reason))
	}
	return out
}

// noticeRed is the red startup-notice style; lipgloss degrades to
// plain text when the writer has no color support.
var noticeRed = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)

// startupRed renders one notice line in ANSI red.
func startupRed(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, noticeRed.Render(line))
}

// runDoctor probes every registered provider with the two test
// queries, renders the settled table and reports disabled/excluded
// providers as ОТКЛЮЧЁН (PR24). A TTY output gets an animated spinner
// line while the checks run; non-TTY output degrades to the final
// table only.
func runDoctor(ctx context.Context, settingsPath string, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "anicli doctor")

	settings, err := loadSettingsOrFail(settingsPath)
	if err != nil {
		return err
	}

	reg, err := providers.NewRegistry(*settings, nil)
	if err != nil {
		return fmt.Errorf("build provider registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	budget := settings.Network.SearchTimeout
	if budget <= 0 {
		budget = defaultDoctorTimeout
	}

	type row struct {
		id      string
		name    string
		status  string
		results string
		failed  bool
	}
	rows := make([]row, 0, 16)

	// Disabled (unconfigured) rows: never probed, reason rendered.
	for _, d := range reg.Disabled() {
		rows = append(rows, row{id: d.ID, name: d.ID, status: "ОТКЛЮЧЁН: " + d.Reason, results: "—"})
	}
	// Explicitly excluded rows (PR23 visibility, kept).
	disabledOrExcluded := make(map[string]bool, len(rows))
	for _, r := range rows {
		disabledOrExcluded[r.id] = true
	}
	for _, id := range settings.Providers.Exclude {
		if disabledOrExcluded[id] {
			continue
		}
		rows = append(rows, row{id: id, name: id, status: "ОТКЛЮЧЁН: исключён в настройках (providers.exclude)", results: "—"})
	}

	// Probe the registered providers concurrently.
	probes := reg.List()
	checks := make([]doctorCheck, len(probes))
	stop := startDoctorSpinner(out, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results, err := doctorProbe(ctx, p, budget)
			checks[i] = doctorCheck{id: p.ID(), results: results, err: err}
		}()
	}
	wg.Wait()
	stop()

	for _, c := range checks {
		switch {
		case c.err != nil:
			rows = append(rows, row{id: c.id, name: c.id, status: "ОШИБКА: " + c.err.Error(), results: "—", failed: true})
		default:
			// Both searches answered (0 results still means OK — the
			// provider responded).
			rows = append(rows, row{id: c.id, name: c.id, status: "OK", results: fmt.Sprintf("%d", c.results)})
		}
	}

	// Stable render order: registry order (probes) then disabled.
	const fmtRow = "%-18s %-38s %s\n"
	_, _ = fmt.Fprintf(out, fmtRow, "Провайдер", "Статус", "Результатов")
	for _, r := range rows {
		_, _ = fmt.Fprintf(out, fmtRow, r.name, r.status, r.results)
	}
	return nil
}

// startDoctorSpinner renders an animated single-line progress on TTY
// writers; non-TTY writers get nothing (the final table is the whole
// output). The returned stop func clears the line.
func startDoctorSpinner(out io.Writer, total int) (stop func()) {
	f, isFile := out.(*os.File)
	if !isFile || total == 0 {
		return func() {}
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}

	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_, _ = fmt.Fprintf(f, "\r%s проверка провайдеров (%d)…", frames[i%len(frames)], total)
				i++
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
		_, _ = fmt.Fprint(f, "\r\033[K")
	}
}
