package tui

// Download-flow helpers (PR64): the availability line of the
// download-range prompt (the per-episode dub resolution lands with
// the range-download fix).

import (
	"fmt"
	"strconv"
	"strings"
)

// describeAvailableEpisodes renders the compact availability line of
// the download-range prompt (PR64 #2): the count plus the real
// available set — consecutive episodes collapsed into runs, gaps and
// non-numeric labels listed as-is, so the user never guesses what
// exists before typing a range. The caller's order is preserved —
// the session passes its merged (display) order.
func describeAvailableEpisodes(order []string) string {
	if len(order) == 0 {
		return "Доступных серий нет"
	}
	runs := make([]string, 0, len(order))
	for i := 0; i < len(order); {
		n, err := strconv.Atoi(order[i])
		if err != nil {
			runs = append(runs, order[i])
			i++
			continue
		}
		j := i + 1
		for j < len(order) {
			m, err := strconv.Atoi(order[j])
			if err != nil || m != n+(j-i) {
				break
			}
			j++
		}
		if j-i == 1 {
			runs = append(runs, order[i])
		} else {
			runs = append(runs, order[i]+"–"+order[j-1])
		}
		i = j
	}
	return fmt.Sprintf("Доступно серий: %d (%s)", len(order), strings.Join(runs, ", "))
}
