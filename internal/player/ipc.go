// Package player — the #166 position tracking over mpv's JSON IPC:
// every launch binds a per-launch --input-ipc-server unix socket,
// observes the time-pos property while the process lives (the
// official protocol requires the connection to stay open), and the
// FINAL observed value survives the process exit. The redub flow
// reads it after playedMsg — the TUI reaches the redub menu only once
// the player has exited, so a live query at pick time would find no
// socket; the tracker closes that gap. Every failure degrades to "no
// position": the watcher gives up bounded, logs, and never blocks or
// fails the playback itself.
package player

import (
	"encoding/json"
	"net"
	"time"
)

// ipcDialWindow bounds the post-spawn connect retries: mpv creates
// the socket asynchronously, and a build without IPC support never
// creates it at all.
const ipcDialWindow = 3 * time.Second

// ipcMsg is the loose shape of every JSON IPC line (command replies
// carry "error", property events carry "event"+"name"+"data").
type ipcMsg struct {
	Event string   `json:"event"`
	Name  string   `json:"name"`
	Data  *float64 `json:"data"`
}

// resetPosition starts a fresh tracking round: the tracker reports
// only the MOST RECENT launch's position, so a failed or IPC-less
// launch can never leak an older episode's position into a redub.
func (p *Player) resetPosition(url string) {
	p.posMu.Lock()
	defer p.posMu.Unlock()
	p.lastPosURL = url
	p.lastPos = 0
	p.lastPosOK = false
}

// setPosition records one observed time-pos (the watcher's event loop).
func (p *Player) setPosition(pos float64) {
	p.posMu.Lock()
	defer p.posMu.Unlock()
	p.lastPos = pos
	p.lastPosOK = true
}

// LastPosition reports the final tracked position of the most recent
// launch: the URL it belongs to (the redub guards with it — a stale
// position must never resume a different episode), the seconds, and
// whether any position was observed at all.
func (p *Player) LastPosition() (url string, seconds float64, ok bool) {
	p.posMu.Lock()
	defer p.posMu.Unlock()
	return p.lastPosURL, p.lastPos, p.lastPosOK
}

// watchPosition connects to the launch's IPC socket, subscribes to
// time-pos and tracks it until the connection dies (the process
// exited). Fire-and-forget: every failure is a log line, never an
// error path for Play.
func (p *Player) watchPosition(sock string, proc *process) {
	conn := dialIPC(sock, proc)
	if conn == nil {
		p.logLine("player: ipc: socket never came up, position tracking skipped")
		return
	}
	defer func() { _ = conn.Close() }()

	const observe = "{\"command\":[\"observe_property\",1,\"time-pos\"]}\n"
	if _, err := conn.Write([]byte(observe)); err != nil {
		p.logLine("player: ipc: observe write failed: " + err.Error())
		return
	}

	dec := json.NewDecoder(conn)
	for {
		var msg ipcMsg
		if err := dec.Decode(&msg); err != nil {
			return // EOF on process exit — the normal termination
		}
		if msg.Event == "property-change" && msg.Name == "time-pos" && msg.Data != nil && *msg.Data >= 0 {
			p.setPosition(*msg.Data)
		}
	}
}

// dialIPC retries the connect until the socket appears, the process
// dies or the bounded window runs out.
func dialIPC(sock string, proc *process) net.Conn {
	deadline := time.Now().Add(ipcDialWindow)
	for {
		conn, err := net.DialTimeout("unix", sock, 300*time.Millisecond)
		if err == nil {
			return conn
		}
		select {
		case <-proc.exited:
			return nil
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil
		}
	}
}
