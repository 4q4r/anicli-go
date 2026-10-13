package download

import (
	"context"
	"fmt"
	"sync"
)

// Task states (python DownloadStatus pending/downloading/completed/
// failed mapped to the PR9 queued/running/done/failed vocabulary).
const (
	// StateQueued marks a submitted task waiting for a worker slot.
	StateQueued State = "queued"
	// StateRunning marks a task currently downloading.
	StateRunning State = "running"
	// StateDone marks a successfully finished task.
	StateDone State = "done"
	// StateFailed marks a failed task (Err carries the reason).
	StateFailed State = "failed"
)

// State is one task lifecycle state.
type State string

// eventsBuffer bounds the notification channel.
const eventsBuffer = 128

// Task is one background download unit.
type Task struct {
	// ID is the caller-assigned unique key. Resubmitting an id whose
	// task is QUEUED replaces the payload (the newer intent wins,
	// still not started); resubmitting a RUNNING id is rejected with
	// *ErrTaskActive (169-F — never two downloads on one id).
	ID string
	// Title is the display title.
	Title string
	// EpisodeNum is the episode label.
	EpisodeNum string
	// OutputPath is the destination file.
	OutputPath string
	// ChaptersFile is an optional FFMETADATA path to embed.
	ChaptersFile string
	// OnComplete runs after the download settles (python on_complete).
	OnComplete func(success bool, outputPath string) error

	// State is the lifecycle marker; snapshots are copies under the
	// manager lock.
	State State
	// Err holds the failure reason, empty unless failed.
	Err string
	// Progress is the 0..100 estimate.
	Progress float64
}

// Event is one task notification.
type Event struct {
	// TaskID identifies the task.
	TaskID string
	// State is the new state (or unchanged state with new Progress).
	State State
	// Progress is the latest percent.
	Progress float64
	// Err carries the failure reason on failed events.
	Err string
}

// Runner performs one download; progress reports percent estimates
// (the ffmpeg copy pipeline has no parseable progress, so
// implementations emit 0 then 100 like the python spinner).
type Runner func(ctx context.Context, task Task, progress func(percent float64)) error

// Manager runs background downloads with bounded concurrency
// (config Download.MaxConcurrency), a queue and queryable state
// (python BackgroundDownloadManager; the bounded queue is the PR9
// hardening — python spawned unbounded tasks).
type Manager struct {
	download Runner

	mu    sync.Mutex
	tasks map[string]*managerRecord
	gen   uint64

	sem    chan struct{}
	events chan Event

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// managerRecord pairs the live task with the generation of the Submit
// that created it (169-F): a worker is bound to its generation, so a
// replaced record can never be picked up by the superseded worker —
// the old shape re-read the REPLACED record and ran the new task's
// OutputPath concurrently with the replacement's own worker.
type managerRecord struct {
	task Task
	gen  uint64
}

// NewManager builds the manager. maxConcurrency <= 0 means 1.
func NewManager(maxConcurrency int, download Runner) *Manager {
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		download: download,
		tasks:    make(map[string]*managerRecord),
		sem:      make(chan struct{}, maxConcurrency),
		events:   make(chan Event, eventsBuffer),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Events returns the notification stream. Events are best-effort: the
// buffer is bounded and drops under backpressure, so Task/AllTasks
// queries are the authoritative state.
func (m *Manager) Events() <-chan Event { return m.events }

// ErrTaskActive reports a resubmit of an id whose task is currently
// downloading (169-F: the newer intent must never run the same id
// twice — two ffmpeg on one .part plus a rename race).
type ErrTaskActive struct {
	// ID is the id of the running task.
	ID string
}

// Error implements error.
func (e *ErrTaskActive) Error() string {
	return fmt.Sprintf("download: task %q already running", e.ID)
}

// Submit queues a task and returns its snapshot. The download starts
// as soon as a concurrency slot frees up. A resubmit of an id whose
// task is QUEUED replaces the payload (the newer intent wins, still
// not started); a resubmit of a RUNNING id is rejected with
// *ErrTaskActive (169-F).
func (m *Manager) Submit(task Task) (Task, error) {
	task.State = StateQueued
	task.Progress = 0

	m.mu.Lock()
	if existing, ok := m.tasks[task.ID]; ok && existing.task.State == StateRunning {
		m.mu.Unlock()
		return existing.task, &ErrTaskActive{ID: task.ID}
	}
	m.gen++
	rec := &managerRecord{task: task, gen: m.gen}
	m.tasks[task.ID] = rec
	m.mu.Unlock()

	m.emit(Event{TaskID: task.ID, State: StateQueued})

	m.wg.Add(1)
	go m.worker(rec)
	return task, nil
}

// worker runs one task through the queue. The worker is bound to the
// generation of its Submit: when a newer Submit replaced the record
// while this one waited, it exits without running or settling — the
// replacement's own worker owns the id (169-F, no double-run).
func (m *Manager) worker(rec *managerRecord) {
	defer m.wg.Done()

	select {
	case <-m.ctx.Done():
		m.settleGen(rec, StateFailed, "manager closed", 0)
		return
	case m.sem <- struct{}{}:
	}
	defer func() { <-m.sem }()

	// 169-F: a newer Submit with the same id replaced the record while
	// this worker waited for its slot — it owns nothing anymore.
	m.mu.Lock()
	live, ok := m.tasks[rec.task.ID]
	if !ok || live.gen != rec.gen {
		m.mu.Unlock()
		return
	}
	live.task.State = StateRunning
	snapshot := live.task
	m.mu.Unlock()
	m.emit(Event{TaskID: rec.task.ID, State: StateRunning})

	progress := func(percent float64) {
		m.mu.Lock()
		if live2, ok := m.tasks[rec.task.ID]; ok && live2.gen == rec.gen {
			live2.task.Progress = percent
		}
		m.mu.Unlock()
		m.emit(Event{TaskID: rec.task.ID, State: StateRunning, Progress: percent})
	}

	err := m.download(m.ctx, snapshot, progress)
	if err != nil {
		msg := err.Error()
		m.settleGen(rec, StateFailed, msg, 0)
		m.runComplete(rec.task, false, err)
		return
	}
	m.settleGen(rec, StateDone, "", 100)
	m.runComplete(rec.task, true, nil)
}

// runComplete invokes the task callback; callback errors are recorded
// on the task without changing the download verdict (python appended
// "| callback error").
func (m *Manager) runComplete(task Task, success bool, dlErr error) {
	if task.OnComplete == nil {
		return
	}
	if err := task.OnComplete(success, task.OutputPath); err != nil {
		m.mu.Lock()
		if live, ok := m.tasks[task.ID]; ok {
			suffix := "callback error: " + err.Error()
			if live.task.Err != "" {
				suffix = live.task.Err + " | " + suffix
			}
			live.task.Err = suffix
		}
		m.mu.Unlock()
		_ = dlErr
	}
}

// settle writes a terminal state.
func (m *Manager) settle(id string, state State, msg string, progress float64) {
	m.mu.Lock()
	if live, ok := m.tasks[id]; ok {
		live.task.State = state
		live.task.Err = msg
		if progress > 0 {
			live.task.Progress = progress
		}
		if state == StateDone {
			live.task.Progress = 100
		}
	}
	m.mu.Unlock()
	m.emit(Event{TaskID: id, State: state, Progress: progress, Err: msg})
}

// settleGen is settle bound to a generation: a superseded worker's
// terminal state never clobbers the record its replacement owns
// (169-F).
func (m *Manager) settleGen(rec *managerRecord, state State, msg string, progress float64) {
	m.mu.Lock()
	live, ok := m.tasks[rec.task.ID]
	if !ok || live.gen != rec.gen {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.settle(rec.task.ID, state, msg, progress)
}

// Task returns a snapshot of one task.
func (m *Manager) Task(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	live, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return live.task, true
}

// AllTasks returns snapshots of every tracked task.
func (m *Manager) AllTasks() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, live := range m.tasks {
		out = append(out, live.task)
	}
	return out
}

// ActiveTasks returns queued and running tasks (python
// get_active_tasks).
func (m *Manager) ActiveTasks() []Task {
	var out []Task
	for _, task := range m.AllTasks() {
		if task.State == StateQueued || task.State == StateRunning {
			out = append(out, task)
		}
	}
	return out
}

// ClearFinished removes done and failed tasks and returns how many
// were cleared (python clear_finished).
func (m *Manager) ClearFinished() int {
	m.mu.Lock()
	var removed []string
	for id, live := range m.tasks {
		if live.task.State == StateDone || live.task.State == StateFailed {
			removed = append(removed, id)
		}
	}
	for _, id := range removed {
		delete(m.tasks, id)
	}
	m.mu.Unlock()
	return len(removed)
}

// Close cancels running downloads and waits for all workers to exit.
func (m *Manager) Close() error {
	m.cancel()
	m.wg.Wait()
	return nil
}

// emit sends a best-effort notification.
func (m *Manager) emit(ev Event) {
	select {
	case m.events <- ev:
	default:
		// Dropped under backpressure; queries stay authoritative.
	}
}

// String renders the manager summary for logs.
func (m *Manager) String() string {
	active := len(m.ActiveTasks())
	return fmt.Sprintf("download.Manager(active=%d)", active)
}
