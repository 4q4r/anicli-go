package audsync

// The window schedule (research §4): the reference windows sample the
// video's own track at t = 90, D/2, 2D/3 (head-adjacent, middle,
// late-episode); the donor search window starts one reference length
// earlier, clamped at zero, so a ±60 s misalignment stays inside it.

// Window is one analysis window pair in seconds of stream time.
type Window struct {
	// RefStart is the reference (video) window start.
	RefStart float64
	// SearchStart is the donor search window start (≥ 0).
	SearchStart float64
}

// WindowSchedule returns the analysis windows for an episode of dur
// seconds, or nil when the episode is shorter than MinEpisodeDur (the
// analysis is not worth its budget there).
func WindowSchedule(dur float64) []Window {
	if dur < MinEpisodeDur {
		return nil
	}
	starts := []float64{90, dur / 2, dur * 2 / 3}
	ws := make([]Window, 0, WindowCount)
	for _, t := range starts {
		search := t - ReferenceDur
		if search < 0 {
			search = 0
		}
		ws = append(ws, Window{RefStart: t, SearchStart: search})
	}
	return ws
}
