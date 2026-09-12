package download

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestManagerBoundedConcurrency pins: submitted tasks run, but never
// more than MaxConcurrency at once; all complete as done.
func TestManagerBoundedConcurrency(t *testing.T) {
	t.Parallel()

	var running, highWater atomic.Int64
	var mu sync.Mutex
	order := map[string]bool{}

	dl := func(ctx context.Context, t Task, _ func(float64)) error {
		cur := running.Add(1)
		for {
			hw := highWater.Load()
			if cur <= hw || highWater.CompareAndSwap(hw, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		running.Add(-1)
		mu.Lock()
		order[t.ID] = true
		mu.Unlock()
		return nil
	}

	m := NewManager(2, dl)
	defer func() { _ = m.Close() }()

	for i := range 6 {
		_ = m.Submit(Task{ID: string(rune('a' + i)), Title: "T", EpisodeNum: "1", OutputPath: "/x"})
	}

	waitFor(t, 3*time.Second, func() bool { return len(m.AllTasks()) == 6 && allState(m, StateDone) })
	if hw := highWater.Load(); hw > 2 {
		t.Errorf("high-water concurrency = %d, want <= 2", hw)
	}
	if hw := highWater.Load(); hw != 2 {
		t.Errorf("high-water concurrency = %d, want exactly 2 (queue actually bounds)", hw)
	}
}

// TestManagerStatesAndEvents pins: tasks move queued -> running ->
// done/failed and the event channel observes the transitions.
func TestManagerStatesAndEvents(t *testing.T) {
	t.Parallel()

	dl := func(ctx context.Context, t Task, progress func(float64)) error {
		progress(50)
		if t.ID == "bad" {
			return errors.New("ffmpeg exploded")
		}
		return nil
	}

	m := NewManager(2, dl)
	defer func() { _ = m.Close() }()

	ok := m.Submit(Task{ID: "ok", Title: "A", EpisodeNum: "1", OutputPath: "/a"})
	bad := m.Submit(Task{ID: "bad", Title: "B", EpisodeNum: "2", OutputPath: "/b"})
	if ok.State != StateQueued || bad.State != StateQueued {
		t.Fatalf("initial states = %s/%s, want queued", ok.State, bad.State)
	}

	seen := map[TaskIDState]bool{}
	deadline := time.After(3 * time.Second)
	for len(seen) < 4 {
		select {
		case ev := <-m.Events():
			seen[TaskIDState{ev.TaskID, ev.State}] = true
		case <-deadline:
			t.Fatalf("events incomplete after deadline: %v", seen)
		}
	}

	waitFor(t, 3*time.Second, func() bool {
		okT, _ := m.Task("ok")
		badT, _ := m.Task("bad")
		return okT.State == StateDone && badT.State == StateFailed
	})

	got, _ := m.Task("ok")
	if got.State != StateDone || got.Progress != 100 {
		t.Errorf("ok task = %+v, want done/100", got)
	}
	got, _ = m.Task("bad")
	if got.State != StateFailed || got.Err == "" {
		t.Errorf("bad task = %+v, want failed with error text", got)
	}
}

// TaskIDState keys event observations.
type TaskIDState struct {
	ID    string
	State State
}

// TestManagerOnComplete pins the per-task completion callback
// (python on_complete port).
func TestManagerOnComplete(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	results := map[string]bool{}

	dl := func(context.Context, Task, func(float64)) error { return nil }
	m := NewManager(1, dl)
	defer func() { _ = m.Close() }()

	m.Submit(Task{ID: "a", OutputPath: "/a", OnComplete: func(ok bool, path string) error {
		mu.Lock()
		results[path] = ok
		mu.Unlock()
		return nil
	}})
	m.Submit(Task{ID: "b", OutputPath: "/b"})

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return results["/a"] == true
	})
}

// TestManagerClearFinished pins: done and failed tasks clear, active
// stay.
func TestManagerClearFinished(t *testing.T) {
	t.Parallel()

	dl := func(context.Context, Task, func(float64)) error { return nil }
	m := NewManager(2, dl)
	defer func() { _ = m.Close() }()

	m.Submit(Task{ID: "a"})
	waitFor(t, 3*time.Second, func() bool {
		tk, _ := m.Task("a")
		return tk.State == StateDone
	})

	if n := m.ClearFinished(); n != 1 {
		t.Errorf("ClearFinished = %d, want 1", n)
	}
	if _, ok := m.Task("a"); ok {
		t.Error("cleared task still queryable")
	}
}

// TestManagerCloseCancelsRunning pins: Close cancels in-flight
// downloads and waits for the workers.
func TestManagerCloseCancelsRunning(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	dl := func(ctx context.Context, _ Task, _ func(float64)) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	m := NewManager(1, dl)
	m.Submit(Task{ID: "a"})
	<-started

	done := make(chan struct{})
	go func() {
		_ = m.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after cancelling workers")
	}
	tk, ok := m.Task("a")
	if !ok || tk.State != StateFailed {
		t.Errorf("task after Close = %+v (ok=%v), want failed", tk, ok)
	}
}

// TestManagerDuplicateIDOverwrites pins the python map semantics: a
// duplicate id replaces the tracked task.
func TestManagerDuplicateIDOverwrites(t *testing.T) {
	t.Parallel()

	dl := func(context.Context, Task, func(float64)) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	m := NewManager(1, dl)
	defer func() { _ = m.Close() }()

	first := m.Submit(Task{ID: "dup", Title: "first"})
	if first.Title != "first" {
		t.Fatalf("first submit = %+v", first)
	}
	second := m.Submit(Task{ID: "dup", Title: "second"})
	if second.Title != "second" {
		t.Errorf("duplicate submit = %+v, want replacement", second)
	}
	if n := len(m.AllTasks()); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
}

// allState reports whether every task reached the wanted state.
func allState(m *Manager, state State) bool {
	for _, tk := range m.AllTasks() {
		if tk.State != state {
			return false
		}
	}
	return true
}

// waitFor polls cond until true or the timeout fires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}
