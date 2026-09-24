package tui

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Short RU cell labels (PR98): the fan-out table cell carries only the
// error CLASS so every column stays even and no text can overflow its
// cell — the full error text stays in the file log (the per-settle
// «provider settled» line, search.go).
const (
	labelTimeout   = "таймаут"
	labelCF        = "CF-проверка"
	labelCert      = "сертификат истёк"
	labelNotFound  = "нет данных"
	labelDown      = "недоступен"
	labelRateLimit = "лимит"
	labelError     = "ошибка"
)

// fanoutStatusCFOriginDown is Cloudflare's «origin down» status (not in
// net/http): a 5xx-class wall between the client and the provider.
const fanoutStatusCFOriginDown = 521

// searchErrLabel classifies one settled provider failure onto its
// short RU label (PR98, owner rulings): netclient timeout/watchdog →
// «таймаут», CF challenge → «CF-проверка», x509/cert → «сертификат
// истёк», 404/ErrNotFound → «нет данных», 521/502/refused →
// «недоступен», rate-limit → «лимит», everything else → «ошибка».
// Typed chains (ProviderError, wrap) are traversed with
// errors.Is/errors.As; the ruling order is the match order.
func searchErrLabel(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errSearchTimeout),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, contracts.ErrProviderTimeout):
		return labelTimeout
	}
	var cfErr *netclient.CFChallengeError
	if errors.As(err, &cfErr) {
		return labelCF
	}
	var statusErr *netclient.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusNotFound:
			return labelNotFound
		case http.StatusBadGateway, fanoutStatusCFOriginDown:
			return labelDown
		case http.StatusTooManyRequests:
			return labelRateLimit
		}
	}
	if errors.Is(err, contracts.ErrNotFound) {
		return labelNotFound
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "x509"), strings.Contains(text, "certificate"):
		return labelCert
	case strings.Contains(text, "refused"):
		return labelDown
	case strings.Contains(text, "rate limit"), strings.Contains(text, "too many requests"):
		return labelRateLimit
	}
	return labelError
}

// fanoutMinWidths are the outer column minimums (PR98, owner ruling):
// Провайдер 12 / Статус 14 / Результатов 6 — no column renders
// narrower while the terminal still has room.
var fanoutMinWidths = [3]int{12, 14, 6}

// fanoutHardFloors are the below-minimums squeeze floors: a terminal
// narrower than the minimums truncates the STATUS column first
// (owner ruling), then the provider column; the count column keeps
// its minimum so numbers stay readable.
var fanoutHardFloors = [3]int{8, 8, 6}

// fanoutColumnWidths computes the three outer column widths (PR98):
// start at max(natural, minimum), then — when the budget is tighter —
// compress proportionally to each column's headroom above the
// minimum (largest-remainder rounding, so the split is deterministic);
// below the minimums squeeze the status column first down to the hard
// floors. A terminal narrower than the floors accepts the overflow
// (degenerate).
func fanoutColumnWidths(natural, mins, floors [3]int, availInner int) [3]int {
	widths := [3]int{}
	for j := range widths {
		widths[j] = max(natural[j], mins[j])
	}
	overflow := sum3(widths) - availInner
	if overflow <= 0 {
		return widths
	}

	// Phase 1: proportional compression above the minimums — each
	// column gives up a share of the overflow proportional to its
	// compressible headroom.
	spare := [3]int{}
	spareTotal := 0
	for j := range spare {
		spare[j] = widths[j] - mins[j]
		spareTotal += spare[j]
	}
	if spareTotal >= overflow {
		rem, shares, leftover := [3]int{}, [3]int{}, overflow
		for j := range spare {
			raw := overflow * spare[j]
			shares[j] = raw / spareTotal
			rem[j] = raw % spareTotal
			leftover -= shares[j]
		}
		// Largest fractional part gets the leftover columns (ties →
		// lower column index).
		order := [...]int{0, 1, 2}
		sort.SliceStable(order[:], func(i, k int) bool {
			ra, rb := rem[order[i]], rem[order[k]]
			if ra != rb {
				return ra > rb
			}
			return order[i] < order[k]
		})
		for i := 0; leftover > 0; i, leftover = i+1, leftover-1 {
			shares[order[i%len(order)]]++
		}
		for j := range widths {
			widths[j] -= shares[j]
		}
		return widths
	}

	// Phase 2: below the minimums — status truncates hardest first,
	// then the provider column; the count column keeps its floor.
	for j := range widths {
		widths[j] = mins[j]
	}
	overflow = sum3(widths) - availInner
	for j := 1; j >= 0 && overflow > 0; j-- {
		take := min(widths[j]-floors[j], overflow)
		if take > 0 {
			widths[j] -= take
			overflow -= take
		}
	}
	return widths
}

// sum3 adds the three widths.
func sum3(w [3]int) int { return w[0] + w[1] + w[2] }

// fanoutColumns indexes the fan-out table columns.
const (
	fanoutColProvider = iota
	fanoutColStatus
	fanoutColCount
)

// fanoutRowData is one rendered fan-out table row: raw (unstyled) cell
// text plus the row's status style. Raw text keeps width measurement
// and truncation deterministic — lipgloss applies the style per cell.
type fanoutRowData struct {
	name   string
	status string
	count  string
	style  lipgloss.Style
}

// fanoutHeaders are the column titles.
var fanoutHeaders = [3]string{"Провайдер", "Статус", "Результатов"}

// fanoutBorderOverhead is the horizontal cost of the full box: left
// and right edges plus the two column separators.
const fanoutBorderOverhead = 4

// fanoutCellPad is one column of padding on each side of a cell (the
// python table's even fields).
const fanoutCellPad = 2

// fanoutCell returns the raw text of one row's cell.
func fanoutCell(r fanoutRowData, col int) string {
	switch col {
	case fanoutColStatus:
		return r.status
	case fanoutColCount:
		return r.count
	default:
		return r.name
	}
}

// fanoutData adapts the styled fan-out rows onto the lipgloss table
// Data interface (the cells stay raw; styles ride the StyleFunc).
type fanoutData struct {
	rows []fanoutRowData
}

// At implements table.Data.
func (d fanoutData) At(row, col int) string { return fanoutCell(d.rows[row], col) }

// Rows implements table.Data.
func (d fanoutData) Rows() int { return len(d.rows) }

// Columns implements table.Data.
func (d fanoutData) Columns() int { return len(fanoutHeaders) }

// fanoutWidths computes the rendered column widths for the row set:
// natural sizing (no compression) when availWidth is not tracked.
func fanoutWidths(rows []fanoutRowData, availWidth int) [3]int {
	// Natural (outer) widths: header vs widest cell, plus padding.
	natural := [3]int{}
	for j, h := range fanoutHeaders {
		natural[j] = lipgloss.Width(h) + fanoutCellPad
		for _, r := range rows {
			if w := lipgloss.Width(fanoutCell(r, j)) + fanoutCellPad; w > natural[j] {
				natural[j] = w
			}
		}
	}

	availInner := availWidth - fanoutBorderOverhead
	if availWidth <= 0 {
		// Natural mode (terminal width not tracked yet): no
		// compression — the budget is whatever the min-lifted columns
		// need, so a roomy screen never truncates.
		for j := range natural {
			natural[j] = max(natural[j], fanoutMinWidths[j])
		}
		availInner = natural[0] + natural[1] + natural[2]
	}
	return fanoutColumnWidths(natural, fanoutMinWidths, fanoutHardFloors, availInner)
}

// fanoutBoxWidth is the full box width the footer centers into.
func fanoutBoxWidth(rows []fanoutRowData, availWidth int) int {
	w := fanoutWidths(rows, availWidth)
	return sum3(w) + fanoutBorderOverhead
}

// renderFanoutTable renders the bordered fan-out table (PR98, owner
// ruling: «ровные поля и линии, текст не мог выступить за границы
// своей клетки»): a full box border, a separator between every row,
// fixed column widths and per-cell …-truncation via the lipgloss
// table package. availWidth is the tracked terminal budget for the
// whole box; zero or negative means natural sizing (no compression).
func renderFanoutTable(rows []fanoutRowData, availWidth int) string {
	if len(rows) == 0 {
		return ""
	}

	widths := fanoutWidths(rows, availWidth)

	t := table.New().
		Wrap(false). // truncate with … instead of wrapping — cells never grow rows
		BorderRow(true).
		StyleFunc(func(row, col int) lipgloss.Style {
			base := lipgloss.NewStyle().
				PaddingLeft(1).
				PaddingRight(1).
				Width(widths[col])
			if col == fanoutColCount {
				base = base.Align(lipgloss.Right)
			}
			if row == table.HeaderRow {
				return base
			}
			if col == fanoutColStatus {
				return base.Inherit(rows[row].style)
			}
			return base
		}).
		Headers(fanoutHeaders[:]...).
		Data(fanoutData{rows: rows})

	return t.String()
}
