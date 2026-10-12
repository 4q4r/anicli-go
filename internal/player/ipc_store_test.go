package player

import "testing"

// TestPositionTrackerDropsSupersededLaunchEvents (review M1): the
// tracker's (URL tag, position value) pair must be atomic per launch.
// A superseded launch's watcher can deliver its already-buffered
// property events AFTER the next Play reset the store — without a
// generation gate the store would then read (newURL, oldPos), a pair
// belonging to neither launch. Every observation is tagged with its
// own launch's generation; a stale-generation write is dropped, the
// fresh launch's reset state stands until its own watcher observes.
func TestPositionTrackerDropsSupersededLaunchEvents(t *testing.T) {
	t.Parallel()

	p := New(Options{Bin: "mpv"})

	gen1 := p.resetPosition("u-ep1-dub-a")
	p.setPosition(gen1, 42)
	if _, pos, ok := p.LastPosition(); !ok || pos != 42 {
		t.Fatalf("a current-generation observation must land: (_, %v, %v), want (_, 42, true)", pos, ok)
	}

	// The next launch resets the tracker; the OLD watcher's buffered
	// events arrive afterwards (the M1 race).
	gen2 := p.resetPosition("u-ep1-dub-b")
	p.setPosition(gen1, 99)

	url, pos, ok := p.LastPosition()
	if ok {
		t.Fatalf("a superseded launch's event leaked into the fresh round: (%q, %v, %v)", url, pos, ok)
	}
	if url != "u-ep1-dub-b" {
		t.Errorf("reset tag = %q, want the fresh launch's URL", url)
	}

	// The current generation's observations land normally.
	p.setPosition(gen2, 7)
	if url, pos, ok := p.LastPosition(); !ok || url != "u-ep1-dub-b" || pos != 7 {
		t.Fatalf("current-generation event = (%q, %v, %v), want (u-ep1-dub-b, 7, true)", url, pos, ok)
	}
}
