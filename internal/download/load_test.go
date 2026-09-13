//go:build load

package download

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Download manager load profile: 500 queued tasks through a
// concurrency-4 manager; asserts full completion, bounded parallelism
// and no goroutine leak after Close.

const (
	loadTasks        = 500
	loadConcurrency  = 4
	loadTaskWorkTime = 200 * time.Microsecond
)

// TestLoadDownloadManager500Tasks drives 500 tasks at concurrency 4.
func TestLoadDownloadManager500Tasks(t *testing.T) {
	var running, maxRunning atomic.Int64

	manager := NewManager(loadConcurrency, func(_ context.Context, _ Task, progress func(float64)) error {
		cur := running.Add(1)
		for {
			old := maxRunning.Load()
			if cur <= old || maxRunning.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(loadTaskWorkTime)
		progress(100)
		running.Add(-1)
		return nil
	})
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Fatalf("close manager: %v", err)
		}
	})

	goroutinesBefore := runtime.NumGoroutine()

	for i := range loadTasks {
		manager.Submit(Task{
			ID:         fmt.Sprintf("load-%d", i),
			Title:      fmt.Sprintf("Load Anime %d", i/12),
			EpisodeNum: fmt.Sprint(i%12 + 1),
			OutputPath: fmt.Sprintf("/tmp/anicli-load/%d.mkv", i),
		})
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(manager.ActiveTasks()) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	tasks := manager.AllTasks()
	if len(tasks) != loadTasks {
		t.Fatalf("manager tracks %d tasks, want %d", len(tasks), loadTasks)
	}
	done := 0
	for _, task := range tasks {
		if task.State != StateDone {
			t.Fatalf("task %s state %q, want %q", task.ID, task.State, StateDone)
		}
		done++
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	observed := maxRunning.Load()
	if observed > loadConcurrency {
		t.Fatalf("bounded workers violated: %d concurrent runners > %d", observed, loadConcurrency)
	}

	leaked := goroutineDelta(t, goroutinesBefore, 2*time.Second, 25)
	fmt.Fprintf(os.Stdout, "\n=== download manager load results ===\n")
	fmt.Fprintf(os.Stdout, "tasks\t%d\tcompleted %d\tconcurrency %d (observed max %d)\tleak delta %d\n\n",
		loadTasks, done, loadConcurrency, observed, leaked)
	if leaked > 10 {
		t.Fatalf("goroutine leak: delta %d after settle", leaked)
	}
}

// goroutineDelta waits for the goroutine count to settle and returns
// the delta against baseline.
func goroutineDelta(t *testing.T, baseline int, window time.Duration, allowed int) int {
	t.Helper()
	deadline := time.Now().Add(window)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current == last && current <= baseline+allowed {
			return current - baseline
		}
		last = current
	}
	return last - baseline
}
