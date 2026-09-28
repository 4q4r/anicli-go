// Package discord is the optional Rich Presence integration (PR115):
// when the user enables it, playback announces itself to the local
// Discord client over the documented IPC protocol — raw JSON frames
// over a Unix domain socket (Windows: named pipe), stdlib only, no
// third-party SDK.
//
// Protocol (verified against the official RPC over IPC documentation):
//   - every message is one frame: uint32 little-endian opcode, uint32
//     little-endian payload length, then the JSON payload, written in
//     a single Write (split writes break the pipe);
//   - opcode 0 HANDSHAKE carries {"v":1,"client_id":...} and the
//     server answers a FRAME (opcode 1) with evt "READY";
//   - opcode 1 FRAME carries commands like SET_ACTIVITY
//     {"cmd","args":{"pid","activity"},"nonce"}; the server echoes
//     every command with the same nonce (evt null on success);
//   - clearing the presence is SET_ACTIVITY with a null activity.
//
// Reliability contract: presence is decoration — it must never stall
// or fail playback. SetActivity/Clear are non-blocking (a background
// worker owns the socket); a missing Discord client surfaces as the
// typed ErrNotRunning note in the log and nothing else.
package discord

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/an0nx/anicli-go/internal/i18n"
)

// IPC opcodes (official RPC-over-IPC opcode table).
const (
	opHandshake uint32 = 0
	opFrame     uint32 = 1
	opClose     uint32 = 2
)

// Protocol budgets.
const (
	// maxPayload bounds an inbound frame so a bogus length header
	// cannot make the reader allocate gigabytes.
	maxPayload = 1 << 20
	// ioDeadline bounds one socket round-trip (write plus response)
	// so a hung Discord pipe never wedges the worker.
	ioDeadline = 5 * time.Second
	// commandSet is "SET_ACTIVITY" for both showing and clearing.
	commandSetActivity = "SET_ACTIVITY"
	// eventReady is the handshake response event.
	eventReady = "READY"
	// eventError marks a command echo as failed.
	eventError = "ERROR"
	// discordTextLimit is Discord's activity text cap (details, state
	// and asset texts are 2–128 characters).
	discordTextLimit = 128
)

// ActivityWatching is the activity type 3 — semantically what an
// anime session is (Discord renders "Watching <app name>").
const ActivityWatching = 3

// ErrNotRunning is the typed note for "Discord is not running" (no
// IPC pipe accepted a connection). Presence is skipped, never fatal.
var ErrNotRunning = errors.New("discord: not running (no IPC pipe found)")

// Options mirrors the [discord] settings section.
type Options struct {
	// Enabled gates the integration (opt-in; default off).
	Enabled bool
	// ClientID is the Discord application id (discord.com developers).
	ClientID string
	// ShowEpisode includes the episode number in the presence text.
	ShowEpisode bool
}

// Activity is the rich-presence payload subset anicli sends.
type Activity struct {
	Details         string
	State           string
	Type            int
	StartUnixMS     int64
	AssetsLargeText string
}

// readWriter is the transport seam: a Unix socket conn and the
// Windows named-pipe wrapper both satisfy it; tests inject fakes.
type readWriter interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// Client publishes the watching presence through a background worker.
// The zero-value path is New: disabled options yield an inert client
// whose methods are no-ops, so callers never nil-check.
type Client struct {
	opts Options
	log  *slog.Logger
	pid  int

	// transport seams (tests inject).
	candidates []string
	dial       func(path string) (readWriter, error)
	now        func() time.Time

	ch       chan cmd
	done     chan struct{}
	closeOne sync.Once
	noop     bool
}

// cmd is one queued presence operation.
type cmd struct {
	clear bool
	act   Activity
}

// New builds the client and, when the integration is enabled with a
// client id, starts the worker goroutine. A nil log degrades to
// discard (presence logs ride the app's file sink, never stderr).
func New(opts Options, log *slog.Logger) *Client {
	return newClient(opts, log)
}

// newClient is the constructor seam tests use (they overwrite the
// candidate list and dial function afterwards).
func newClient(opts Options, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Client{
		opts:       opts,
		log:        log,
		pid:        os.Getpid(),
		candidates: pipeCandidates(defaultPipeDirs()),
		dial:       dialPipe,
		now:        time.Now,
		ch:         make(chan cmd, 4),
		done:       make(chan struct{}),
	}
	// Disabled (or unconfigured) integration: inert client, no
	// goroutine, no pipe probing.
	c.noop = !opts.Enabled || strings.TrimSpace(opts.ClientID) == ""
	if !c.noop {
		go c.run()
	}
	return c
}

// SetActivity announces "now watching <title> episode <ep>" without
// blocking the caller: the command is queued for the worker; a full
// queue (worker wedged on IO) drops the update — presence must never
// stall playback.
func (c *Client) SetActivity(title, episode string) {
	if c.noop {
		return
	}
	act := buildActivity(title, episode, c.opts.ShowEpisode, c.now())
	select {
	case c.ch <- cmd{act: act}:
	default:
		c.log.Warn("discord: presence queue full; update dropped",
			"title", title, "episode", episode)
	}
}

// Clear queues the null-activity command (and the graceful socket
// close) for the worker. Also non-blocking.
func (c *Client) Clear() {
	if c.noop {
		return
	}
	select {
	case c.ch <- cmd{clear: true}:
	default:
		c.log.Warn("discord: presence queue full; clear dropped")
	}
}

// Close stops the worker. If a presence is showing, the worker
// clears it before releasing the socket. Safe to call twice.
func (c *Client) Close() {
	c.closeOne.Do(func() { close(c.done) })
}

// run is the worker loop: it owns the only socket reference, connects
// lazily on the first activity and disconnects on clear.
func (c *Client) run() {
	var conn readWriter
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	for {
		select {
		case m := <-c.ch:
			if m.clear {
				if conn != nil {
					if err := c.sendClear(conn); err != nil {
						c.log.Warn("discord: clear failed", "error", err)
					}
					_ = conn.Close()
					conn = nil
				}
				continue
			}
			if conn == nil {
				d, err := c.connect()
				if err != nil {
					// Typed absence note: playback continues regardless.
					c.log.Warn("discord: rich presence skipped", "error", err)
					continue
				}
				conn = d
			}
			if err := c.sendActivity(conn, m.act); err != nil {
				c.log.Warn("discord: presence update failed", "error", err)
				_ = conn.Close()
				conn = nil // next activity reconnects
			}
		case <-c.done:
			if conn != nil {
				// Best-effort graceful clear on shutdown.
				if err := c.sendClear(conn); err != nil {
					c.log.Warn("discord: shutdown clear failed", "error", err)
				}
			}
			return
		}
	}
}

// connect walks the candidate pipes, handshakes the first one that
// accepts a connection and returns the ready socket.
func (c *Client) connect() (readWriter, error) {
	var lastErr error
	for _, path := range c.candidates {
		conn, err := c.dial(path)
		if err != nil {
			lastErr = err
			continue
		}
		if err := c.handshake(conn); err != nil {
			_ = conn.Close()
			lastErr = err
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		return nil, ErrNotRunning
	}
	return nil, fmt.Errorf("%w: %d pipe candidates tried, last error: %w",
		ErrNotRunning, len(c.candidates), lastErr)
}

// handshake sends the HANDSHAKE frame and waits for the READY FRAME.
func (c *Client) handshake(conn readWriter) error {
	payload, err := handshakePayload(c.opts.ClientID)
	if err != nil {
		return fmt.Errorf("handshake payload: %w", err)
	}
	if err := writeCommand(conn, opHandshake, payload); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}
	op, body, err := readResponse(conn)
	if err != nil {
		return fmt.Errorf("read handshake response: %w", err)
	}
	if op != opFrame {
		return fmt.Errorf("handshake response opcode %d, want FRAME (%d)", op, opFrame)
	}
	var resp struct {
		Evt string `json:"evt"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("handshake response not JSON: %w", err)
	}
	if resp.Evt != eventReady {
		return fmt.Errorf("handshake response evt %q, want %q", resp.Evt, eventReady)
	}
	return nil
}

// sendActivity writes one SET_ACTIVITY frame and checks its echo.
func (c *Client) sendActivity(conn readWriter, act Activity) error {
	payload, err := setActivityPayload(newNonce(), c.pid, act)
	if err != nil {
		return fmt.Errorf("activity payload: %w", err)
	}
	return c.writeSetActivity(conn, payload)
}

// sendClear writes the null-activity SET_ACTIVITY frame and checks
// its echo.
func (c *Client) sendClear(conn readWriter) error {
	payload, err := clearActivityPayload(newNonce(), c.pid)
	if err != nil {
		return fmt.Errorf("clear payload: %w", err)
	}
	return c.writeSetActivity(conn, payload)
}

// writeSetActivity sends the frame and validates the command echo.
func (c *Client) writeSetActivity(conn readWriter, payload []byte) error {
	var nonce struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &nonce); err != nil {
		return fmt.Errorf("nonce decode: %w", err)
	}
	if err := writeCommand(conn, opFrame, payload); err != nil {
		return fmt.Errorf("send command: %w", err)
	}
	// The server may push unrelated frames; scan until our nonce.
	for range 16 {
		op, body, err := readResponse(conn)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if op == opClose {
			return errors.New("server closed the pipe")
		}
		if op != opFrame {
			continue
		}
		var resp struct {
			Nonce string `json:"nonce"`
			Evt   string `json:"evt"`
			Data  struct {
				Message string `json:"message"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			continue // tolerate non-JSON pushes; keep scanning
		}
		if resp.Nonce != nonce.Nonce {
			continue
		}
		if resp.Evt == eventError {
			return fmt.Errorf("command rejected: %s", resp.Data.Message)
		}
		return nil
	}
	return errors.New("no command echo within frame budget")
}

// writeCommand frames payload and writes it in ONE Write (the pipe
// protocol breaks on split writes) under the IO deadline.
func writeCommand(conn readWriter, op uint32, payload []byte) error {
	if err := conn.SetDeadline(time.Now().Add(ioDeadline)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}
	_, err := conn.Write(encodeFrame(op, payload))
	return err
}

// readResponse reads one frame under the IO deadline.
func readResponse(conn readWriter) (uint32, []byte, error) {
	if err := conn.SetDeadline(time.Now().Add(ioDeadline)); err != nil {
		return 0, nil, fmt.Errorf("set deadline: %w", err)
	}
	return readFrame(conn)
}

// --- frame codec ---

// encodeFrame builds one wire frame: LE opcode, LE payload length,
// payload — as a single buffer.
func encodeFrame(op uint32, payload []byte) []byte {
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], op)
	// Payloads are built from clamped (<128 runes) strings — far below
	// the uint32 length ceiling on every platform.
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload))) //nolint:gosec // bounded by construction
	copy(frame[8:], payload)
	return frame
}

// readFrame decodes one frame from r, enforcing the sanity ceiling on
// the advertised payload length.
func readFrame(r io.Reader) (uint32, []byte, error) {
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	op := binary.LittleEndian.Uint32(head[0:4])
	size := binary.LittleEndian.Uint32(head[4:8])
	if size > maxPayload {
		return 0, nil, fmt.Errorf("discord: frame payload %d exceeds ceiling %d", size, maxPayload)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// --- payload builders ---

// handshakeMsg is the HANDSHAKE body: RPC version 1 plus the app id.
type handshakeMsg struct {
	V        int    `json:"v"`
	ClientID string `json:"client_id"`
}

// handshakePayload renders {"v":1,"client_id":"<id>"}.
func handshakePayload(clientID string) ([]byte, error) {
	return json.Marshal(handshakeMsg{V: 1, ClientID: clientID})
}

// timestampsJSON is the elapsed-time block (unix milliseconds).
type timestampsJSON struct {
	Start int64 `json:"start"`
}

// assetsJSON is the presence art block; anicli ships no art assets,
// only the hover text.
type assetsJSON struct {
	LargeText string `json:"large_text,omitempty"`
}

// activityJSON is the wire form of Activity.
type activityJSON struct {
	Details    string          `json:"details"`
	State      string          `json:"state"`
	Type       int             `json:"type"`
	Timestamps *timestampsJSON `json:"timestamps,omitempty"`
	Assets     *assetsJSON     `json:"assets,omitempty"`
}

// argsJSON is the SET_ACTIVITY argument block; a nil Activity is the
// documented clear form ("activity": null).
type argsJSON struct {
	PID      int           `json:"pid"`
	Activity *activityJSON `json:"activity"`
}

// commandJSON is one FRAME command body.
type commandJSON struct {
	Cmd   string   `json:"cmd"`
	Args  argsJSON `json:"args"`
	Nonce string   `json:"nonce"`
}

// setActivityPayload renders the SET_ACTIVITY FRAME body.
func setActivityPayload(nonce string, pid int, act Activity) ([]byte, error) {
	return json.Marshal(commandJSON{
		Cmd: commandSetActivity,
		Args: argsJSON{PID: pid, Activity: &activityJSON{
			Details: clampText(act.Details),
			State:   clampText(act.State),
			Type:    act.Type,
			Timestamps: &timestampsJSON{
				Start: act.StartUnixMS,
			},
			Assets: &assetsJSON{LargeText: clampText(act.AssetsLargeText)},
		}},
		Nonce: nonce,
	})
}

// clearActivityPayload renders the null-activity SET_ACTIVITY body.
func clearActivityPayload(nonce string, pid int) ([]byte, error) {
	return json.Marshal(commandJSON{
		Cmd:   commandSetActivity,
		Args:  argsJSON{PID: pid},
		Nonce: nonce,
	})
}

// clampText truncates to Discord's 128-character activity text cap,
// rune-safe.
func clampText(s string) string {
	if len(s) <= discordTextLimit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= discordTextLimit {
		return s
	}
	return string(runes[:discordTextLimit])
}

// buildActivity renders the user-facing presence strings through the
// locale tables (owner ruling: multilocalization) and stamps the
// watch start time.
func buildActivity(title, episode string, showEpisode bool, now time.Time) Activity {
	act := Activity{
		State:           i18n.T("discord.state"),
		Type:            ActivityWatching,
		StartUnixMS:     now.UnixMilli(),
		AssetsLargeText: title,
	}
	if showEpisode && episode != "" {
		act.Details = i18n.T("discord.details", i18n.Vals{"title": title, "ep": episode})
	} else {
		act.Details = i18n.T("discord.details_plain", i18n.Vals{"title": title})
	}
	return act
}

// newNonce builds a unique per-command nonce (16 random bytes, hex).
// A broken entropy source degrades to a nanosecond stamp — still
// unique within one client session, which is all the nonce needs.
func newNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// pipeCandidates expands the candidate base directories into the
// numbered socket paths discord-ipc-0 … discord-ipc-9 (first pipe
// that accepts a connection wins; Discord may sit on any of them).
func pipeCandidates(dirs []string) []string {
	out := make([]string, 0, len(dirs)*10)
	for _, dir := range dirs {
		for i := range 10 {
			out = append(out, filepath.Join(dir, fmt.Sprintf("discord-ipc-%d", i)))
		}
	}
	return out
}
