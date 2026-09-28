package discord

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- pipe path resolution ---

// TestPipeCandidatesUnix: every candidate directory expands to the
// ten numbered sockets, in order, directory by directory.
func TestPipeCandidatesUnix(t *testing.T) {
	got := pipeCandidates([]string{"/run/user/1000", "/tmp"})
	if len(got) != 20 {
		t.Fatalf("pipeCandidates: got %d candidates, want 20", len(got))
	}
	if got[0] != "/run/user/1000/discord-ipc-0" || got[9] != "/run/user/1000/discord-ipc-9" {
		t.Fatalf("pipeCandidates: first dir block = %q..%q", got[0], got[9])
	}
	if got[10] != "/tmp/discord-ipc-0" || got[19] != "/tmp/discord-ipc-9" {
		t.Fatalf("pipeCandidates: second dir block = %q..%q", got[10], got[19])
	}
}

// TestDefaultPipeDirsEnv: $DISCORD_IPC_PATH wins, then the runtime
// dir, then TMPDIR, then the hard /tmp fallback. Unset vars drop out.
func TestDefaultPipeDirsEnv(t *testing.T) {
	t.Setenv("DISCORD_IPC_PATH", "/custom/ipc")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("TMPDIR", "/var/tmp/user")

	want := []string{"/custom/ipc", "/run/user/1000", "/var/tmp/user", "/tmp"}
	got := defaultPipeDirs()
	if len(got) != len(want) {
		t.Fatalf("defaultPipeDirs: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("defaultPipeDirs[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestDefaultPipeDirsFallback: with nothing set in the environment,
// /tmp is the last resort.
func TestDefaultPipeDirsFallback(t *testing.T) {
	t.Setenv("DISCORD_IPC_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "")

	if got := defaultPipeDirs(); len(got) != 1 || got[0] != "/tmp" {
		t.Fatalf("defaultPipeDirs: got %v, want [/tmp]", got)
	}
}

// --- frame codec ---

// TestEncodeFrame: 8-byte little-endian header (opcode, payload
// length) followed by the payload — the whole frame in one buffer
// (the pipe breaks on split writes).
func TestEncodeFrame(t *testing.T) {
	payload := []byte(`{"v":1}`)
	frame := encodeFrame(opHandshake, payload)
	if len(frame) != 8+len(payload) {
		t.Fatalf("encodeFrame: len=%d, want %d", len(frame), 8+len(payload))
	}
	if op := binary.LittleEndian.Uint32(frame[0:4]); op != 0 {
		t.Fatalf("encodeFrame: opcode=%d, want 0", op)
	}
	if n := binary.LittleEndian.Uint32(frame[4:8]); n != uint32(len(payload)) { //nolint:gosec // test-local payload, tiny
		t.Fatalf("encodeFrame: length=%d, want %d", n, len(payload))
	}
	if !bytes.Equal(frame[8:], payload) {
		t.Fatalf("encodeFrame: payload=%q, want %q", frame[8:], payload)
	}
}

// TestReadFrameRoundTrip: decode(encode(x)) == x.
func TestReadFrameRoundTrip(t *testing.T) {
	payload := []byte(`{"cmd":"SET_ACTIVITY","nonce":"abc"}`)
	op, got, err := readFrame(bytes.NewReader(encodeFrame(opFrame, payload)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if op != opFrame {
		t.Fatalf("readFrame: opcode=%d, want %d", op, opFrame)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("readFrame: payload=%q, want %q", got, payload)
	}
}

// TestReadFrameRejectsOversized: a bogus length header must not
// allocate gigabytes.
func TestReadFrameRejectsOversized(t *testing.T) {
	var buf bytes.Buffer
	var head [8]byte
	binary.LittleEndian.PutUint32(head[0:4], opFrame)
	binary.LittleEndian.PutUint32(head[4:8], 1<<30)
	buf.Write(head[:])
	if _, _, err := readFrame(&buf); err == nil {
		t.Fatal("readFrame: oversized payload accepted")
	}
}

// --- payload builders ---

// TestHandshakePayloadShape: {"v":1,"client_id":"<id>"} exactly.
func TestHandshakePayloadShape(t *testing.T) {
	got, err := handshakePayload("123456789012345678")
	if err != nil {
		t.Fatalf("handshakePayload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("handshake payload not JSON: %v (%s)", err, got)
	}
	if decoded["v"] != float64(1) {
		t.Fatalf("handshake v = %v, want 1", decoded["v"])
	}
	if decoded["client_id"] != "123456789012345678" {
		t.Fatalf("handshake client_id = %v", decoded["client_id"])
	}
	if len(decoded) != 2 {
		t.Fatalf("handshake payload has extra keys: %v", decoded)
	}
}

// TestSetActivityPayloadShape: the FRAME body carries cmd, args.pid,
// args.activity (details/state/type/timestamps/assets) and a nonce.
func TestSetActivityPayloadShape(t *testing.T) {
	act := Activity{
		Details:         "Naruto — ep. 5",
		State:           "Watching anime",
		Type:            ActivityWatching,
		StartUnixMS:     1700000000000,
		AssetsLargeText: "Naruto",
	}
	raw, err := setActivityPayload("nonce-1", 4242, act)
	if err != nil {
		t.Fatalf("setActivityPayload: %v", err)
	}

	var decoded struct {
		Cmd   string `json:"cmd"`
		Nonce string `json:"nonce"`
		Args  struct {
			PID      int `json:"pid"`
			Activity struct {
				Details    string `json:"details"`
				State      string `json:"state"`
				Type       int    `json:"type"`
				Timestamps struct {
					Start int64 `json:"start"`
				} `json:"timestamps"`
				Assets struct {
					LargeText string `json:"large_text"`
				} `json:"assets"`
			} `json:"activity"`
		} `json:"args"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("setActivity payload not JSON: %v (%s)", err, raw)
	}
	if decoded.Cmd != "SET_ACTIVITY" {
		t.Fatalf("cmd = %q", decoded.Cmd)
	}
	if decoded.Nonce != "nonce-1" {
		t.Fatalf("nonce = %q", decoded.Nonce)
	}
	if decoded.Args.PID != 4242 {
		t.Fatalf("args.pid = %d", decoded.Args.PID)
	}
	a := decoded.Args.Activity
	if a.Details != act.Details || a.State != act.State {
		t.Fatalf("activity details/state = %q/%q", a.Details, a.State)
	}
	if a.Type != 3 { // WATCHING
		t.Fatalf("activity type = %d, want 3 (WATCHING)", a.Type)
	}
	if a.Timestamps.Start != 1700000000000 {
		t.Fatalf("timestamps.start = %d (unix ms)", a.Timestamps.Start)
	}
	if a.Assets.LargeText != "Naruto" {
		t.Fatalf("assets.large_text = %q", a.Assets.LargeText)
	}
}

// TestClearActivityPayloadShape: clearing is SET_ACTIVITY with a null
// activity object.
func TestClearActivityPayloadShape(t *testing.T) {
	raw, err := clearActivityPayload("nonce-2", 7)
	if err != nil {
		t.Fatalf("clearActivityPayload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("clear payload not JSON: %v (%s)", err, raw)
	}
	if decoded["cmd"] != "SET_ACTIVITY" {
		t.Fatalf("cmd = %v", decoded["cmd"])
	}
	args, ok := decoded["args"].(map[string]any)
	if !ok {
		t.Fatalf("args not an object: %v", decoded["args"])
	}
	if activity, present := args["activity"]; present && activity != nil {
		t.Fatalf("clear activity = %v, want null", activity)
	}
	if args["pid"] != float64(7) {
		t.Fatalf("clear pid = %v", args["pid"])
	}
}

// --- activity building (i18n) ---

// TestBuildActivity: details/state come from the locale tables; the
// start timestamp is unix milliseconds; the asset text carries the
// title.
func TestBuildActivity(t *testing.T) {
	now := time.Unix(1700000000, 0)
	act := buildActivity("Наруто", "5", true, now)
	if act.Details != "Наруто — ep. 5" {
		t.Fatalf("details = %q", act.Details)
	}
	if act.State != "Watching anime" {
		t.Fatalf("state = %q", act.State)
	}
	if act.Type != ActivityWatching {
		t.Fatalf("type = %d", act.Type)
	}
	if act.StartUnixMS != 1700000000000 {
		t.Fatalf("start = %d, want unix ms", act.StartUnixMS)
	}
	if act.AssetsLargeText != "Наруто" {
		t.Fatalf("large_text = %q", act.AssetsLargeText)
	}
}

// TestBuildActivityWithoutEpisode: show_episode=false keeps the title
// only.
func TestBuildActivityWithoutEpisode(t *testing.T) {
	act := buildActivity("Наруто", "5", false, time.Unix(1, 0))
	if act.Details != "Наруто" {
		t.Fatalf("details = %q, want plain title", act.Details)
	}
}

// --- client behavior ---

// captureLogger records everything the client logs.
type captureLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, string(p))
	return len(p), nil
}

func (l *captureLogger) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// newTestClient builds a Client with injected candidates and dial.
func newTestClient(t *testing.T, showEpisode bool, candidates []string, dial func(string) (readWriter, error)) (*Client, *captureLogger) {
	t.Helper()
	sink := &captureLogger{}
	c := newClient(Options{Enabled: true, ClientID: "42", ShowEpisode: showEpisode},
		slog.New(slog.NewTextHandler(sink, nil)))
	c.candidates = candidates
	c.dial = dial
	t.Cleanup(c.Close)
	return c, sink
}

// TestSetActivityNonBlocking: even with a dial that never returns, the
// SetActivity/Clear calls return immediately — presence must never
// stall playback.
func TestSetActivityNonBlocking(t *testing.T) {
	block := make(chan struct{})
	c, _ := newTestClient(t, true,
		[]string{"/x/discord-ipc-0"},
		func(string) (readWriter, error) { <-block; return nil, errors.New("unreachable") })

	start := time.Now()
	c.SetActivity("Naruto", "5")
	c.Clear()
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("SetActivity/Clear blocked for %v — must be non-blocking", elapsed)
	}
}

// TestDiscordAbsentTypedSkip: every dial fails (Discord not running)
// — the worker logs the typed note and nothing panics.
func TestDiscordAbsentTypedSkip(t *testing.T) {
	c, sink := newTestClient(t, true,
		[]string{"/x/discord-ipc-0"},
		func(string) (readWriter, error) {
			return nil, &os.PathError{Op: "dial", Path: "/x/discord-ipc-0", Err: os.ErrNotExist}
		})

	c.SetActivity("Naruto", "5")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.text(), ErrNotRunning.Error()) {
			return // typed note logged, no crash
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("worker never logged the typed absence note; log so far:\n%s", sink.text())
}

// fakeDiscordServer speaks enough of the protocol for one client
// session: READY after the handshake, an echo per command.
type fakeDiscordServer struct {
	listener net.Listener
	path     string

	mu        sync.Mutex
	handshake []byte
	frames    [][]byte
	closed    chan struct{}
}

func newFakeDiscordServer(t *testing.T) *fakeDiscordServer {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "discord-ipc-0")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("fake discord listen: %v", err)
	}
	s := &fakeDiscordServer{listener: ln, closed: make(chan struct{})}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	s.path = path
	return s
}

func (s *fakeDiscordServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(); close(s.closed) }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	op, payload, err := readFrame(conn)
	if err != nil || op != opHandshake {
		return
	}
	s.record(payload)

	ready := map[string]any{"v": 1, "cfg": map[string]any{}, "user": map[string]any{}, "evt": "READY"}
	body, _ := json.Marshal(ready)
	if _, err := conn.Write(encodeFrame(opFrame, body)); err != nil {
		return
	}

	for {
		op, payload, err := readFrame(conn)
		if err != nil {
			return // client closed the socket (or errored)
		}
		if op != opFrame {
			return
		}
		s.record(payload)

		var req struct {
			Cmd   string `json:"cmd"`
			Nonce string `json:"nonce"`
		}
		_ = json.Unmarshal(payload, &req)
		echo := map[string]any{"cmd": req.Cmd, "data": nil, "evt": nil, "nonce": req.Nonce}
		body, _ := json.Marshal(echo)
		if _, err := conn.Write(encodeFrame(opFrame, body)); err != nil {
			return
		}
	}
}

func (s *fakeDiscordServer) record(frame []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handshake == nil {
		s.handshake = append([]byte(nil), frame...)
		return
	}
	s.frames = append(s.frames, append([]byte(nil), frame...))
}

func (s *fakeDiscordServer) frameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// frame returns command frame i (skipping the handshake).
func (s *fakeDiscordServer) frame(i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames[i]
}

// waitFrames blocks until n command frames arrived or the deadline.
func (s *fakeDiscordServer) waitFrames(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.frameCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake discord: only %d command frames in 3s (want %d)", s.frameCount(), n)
}

// waitClose blocks until the client disconnects.
func (s *fakeDiscordServer) waitClose(t *testing.T) {
	t.Helper()
	select {
	case <-s.closed:
		return
	case <-time.After(3 * time.Second):
		t.Fatal("fake discord: client never disconnected")
	}
}

// TestClientHandshakeAndActivity: the full happy path — handshake
// {v:1, client_id}, SET_ACTIVITY with the rendered activity, and the
// process pid in args.
func TestClientHandshakeAndActivity(t *testing.T) {
	srv := newFakeDiscordServer(t)
	c, _ := newTestClient(t, true, []string{srv.path},
		func(path string) (readWriter, error) {
			conn, err := net.DialTimeout("unix", path, 2*time.Second)
			if err != nil {
				return nil, err
			}
			return conn.(readWriter), nil
		})

	c.SetActivity("Наруто", "12")
	srv.waitFrames(t, 1)

	// Handshake: {"v":1,"client_id":"42"}.
	var hs struct {
		V        int    `json:"v"`
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(srv.handshake, &hs); err != nil {
		t.Fatalf("handshake frame not JSON: %v (%s)", err, srv.handshake)
	}
	if hs.V != 1 || hs.ClientID != "42" {
		t.Fatalf("handshake = %+v, want v=1 client_id=42", hs)
	}

	// SET_ACTIVITY frame shape.
	var frame struct {
		Cmd  string `json:"cmd"`
		Args struct {
			PID      int `json:"pid"`
			Activity struct {
				Details    string `json:"details"`
				State      string `json:"state"`
				Timestamps struct {
					Start int64 `json:"start"`
				} `json:"timestamps"`
				Assets struct {
					LargeText string `json:"large_text"`
				} `json:"assets"`
			} `json:"activity"`
		} `json:"args"`
	}
	if err := json.Unmarshal(srv.frame(0), &frame); err != nil {
		t.Fatalf("activity frame not JSON: %v (%s)", err, srv.frame(0))
	}
	if frame.Cmd != "SET_ACTIVITY" {
		t.Fatalf("cmd = %q", frame.Cmd)
	}
	if frame.Args.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", frame.Args.PID, os.Getpid())
	}
	act := frame.Args.Activity
	if act.Details != "Наруто — ep. 12" {
		t.Fatalf("details = %q", act.Details)
	}
	if act.State != "Watching anime" {
		t.Fatalf("state = %q", act.State)
	}
	if d := time.Since(time.UnixMilli(act.Timestamps.Start)); d < 0 || d > time.Minute {
		t.Fatalf("timestamps.start = %d — not a fresh unix-ms stamp", act.Timestamps.Start)
	}
	if act.Assets.LargeText != "Наруто" {
		t.Fatalf("large_text = %q", act.Assets.LargeText)
	}
}

// TestClientClearDisconnects: Clear sends the null-activity frame and
// gracefully closes the pipe.
func TestClientClearDisconnects(t *testing.T) {
	srv := newFakeDiscordServer(t)
	c, _ := newTestClient(t, true, []string{srv.path},
		func(path string) (readWriter, error) {
			conn, err := net.DialTimeout("unix", path, 2*time.Second)
			if err != nil {
				return nil, err
			}
			return conn.(readWriter), nil
		})

	c.SetActivity("Наруто", "1")
	srv.waitFrames(t, 1)

	c.Clear()
	srv.waitFrames(t, 2)

	var frame map[string]any
	if err := json.Unmarshal(srv.frame(1), &frame); err != nil {
		t.Fatalf("clear frame not JSON: %v (%s)", err, srv.frame(1))
	}
	if frame["cmd"] != "SET_ACTIVITY" {
		t.Fatalf("clear cmd = %v", frame["cmd"])
	}
	args := frame["args"].(map[string]any)
	if activity, present := args["activity"]; present && activity != nil {
		t.Fatalf("clear activity = %v, want null", activity)
	}

	srv.waitClose(t) // graceful disconnect after the clear
}

// TestNoopClient: the disabled integration is inert — no goroutine,
// no pipes, no panics; Close is idempotent.
func TestNoopClient(t *testing.T) {
	c := newClient(Options{Enabled: false}, nil)
	c.SetActivity("T", "1")
	c.Clear()
	c.Close()
	c.Close() // must be safe twice
}
