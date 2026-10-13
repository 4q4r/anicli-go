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
		_, _ = m.Submit(Task{ID: string(rune('a' + i)), Title: "T", EpisodeNum: "1", OutputPath: "/x"})
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

	ok, _ := m.Submit(Task{ID: "ok", Title: "A", EpisodeNum: "1", OutputPath: "/a"})
	bad, _ := m.Submit(Task{ID: "bad", Title: "B", EpisodeNum: "2", OutputPath: "/b"})
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

// TestManagerDuplicateIDOverwrites (169-F contract): a resubmit of a
// QUEUED id replaces the payload (the newer intent wins, still not
// started); the tracked map stays one-entry-per-id. The RUNNING
// rejection is pinned separately.
func TestManagerDuplicateIDOverwrites(t *testing.T) {
	t.Parallel()

	dl := func(context.Context, Task, func(float64)) error {
		time.Sleep(50 * time.Millisecond)
		return nil
	}
	m := NewManager(1, dl)
	defer func() { _ = m.Close() }()

	first, err := m.Submit(Task{ID: "dup", Title: "first"})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if first.Title != "first" {
		t.Fatalf("first submit = %+v", first)
	}
	second, err := m.Submit(Task{ID: "dup", Title: "second"})
	if err != nil {
		t.Fatalf("queued resubmit must replace, got %v", err)
	}
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

// TestAdvManagerDuplicateIDDoubleRun (169-F): a resubmit with the same
// id while the original is queued replaced the tracked record, and BOTH
// workers re-read it — the live repro ran v1×0 and v2×2 (two ffmpeg on
// one .part plus a rename race, the original silently dropped). The
// newer intent must win the QUEUED slot and run EXACTLY once.
func TestAdvManagerDuplicateIDDoubleRun(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var outputs []string
	dl := func(_ context.Context, task Task, _ func(float64)) error {
		mu.Lock()
		outputs = append(outputs, task.OutputPath)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return nil
	}
	m := NewManager(1, dl)
	defer func() { _ = m.Close() }()

	m.Submit(Task{ID: "ep", OutputPath: "v1.mp4"})
	m.Submit(Task{ID: "ep", OutputPath: "v2.mp4"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(outputs)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // let any double-run surface

	mu.Lock()
	defer mu.Unlock()
	if len(outputs) != 1 {
		t.Fatalf("invocations = %v (%d), want exactly one — never a double-run", outputs, len(outputs))
	}
	if outputs[0] != "v2.mp4" {
		t.Fatalf("ran %q, want the newer intent v2.mp4", outputs[0])
	}
}

// TestAdvManagerSubmitRejectsWhileRunning (169-F): a resubmit of a
// RUNNING id is rejected with a typed error — an honest «уже
// скачивается» instead of two ffmpeg on one .part.
func TestAdvManagerSubmitRejectsWhileRunning(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	dl := func(ctx context.Context, task Task, _ func(float64)) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	m := NewManager(1, dl)
	// release FIRST, then Close: a deferred Close would wait on runners
	// still blocked in the runner when the test aborts early.
	t.Cleanup(func() {
		close(release)
		_ = m.Close()
	})

	m.Submit(Task{ID: "ep", OutputPath: "v1.mp4"})
	<-started

	if _, err := m.Submit(Task{ID: "ep", OutputPath: "v2.mp4"}); err == nil {
		t.Fatal("resubmitting a RUNNING id must fail")
	} else {
		var active *ErrTaskActive
		if !errors.As(err, &active) || active.ID != "ep" {
			t.Fatalf("err = %v, want *ErrTaskActive naming the id", err)
		}
	}
	tk, ok := m.Task("ep")
	if !ok || tk.State != StateRunning || tk.OutputPath != "v1.mp4" {
		t.Fatalf("running task = %+v (ok=%v), want v1.mp4 untouched", tk, ok)
	}
}
