package torrent

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// Status is the lifecycle state of one release.
type Status string

const (
	// StatusFetching means the metadata is still being downloaded
	// (GotInfo pending) — the UI renders it as a live row, never
	// blocks on it.
	StatusFetching Status = "fetching"
	// StatusReady means the file list is known and playback can start.
	StatusReady Status = "ready"
	// StatusError means metadata never arrived (see Release.Err).
	StatusError Status = "error"
)

// InfoHash aliases the library hash type so callers never import
// anacrolix packages to talk to the engine.
type InfoHash = metainfo.Hash

// FileEntry is one file of a release.
type FileEntry struct {
	// Path is the torrent-relative file path.
	Path string
	// Size is the file length in bytes.
	Size int64
	// Index is the file index used by Resolve and the stream URL.
	Index int
	// Episodes are the episode numbers parsed from the file name
	// (empty when none is found).
	Episodes []int
}

// Release is the user-facing view of one added torrent.
type Release struct {
	// InfoHash identifies the torrent (dedupe key).
	InfoHash InfoHash
	// DisplayName is the magnet dn= name, the torrent name or the
	// infohash hex — whatever is known best.
	DisplayName string
	// Files is the file list; empty until Status is ready.
	Files []FileEntry
	// Status is the metadata lifecycle state.
	Status Status
	// Quality is the release-name parsing result.
	Quality Quality
	// Err carries the failure reason when Status is error.
	Err string
}

// StreamHandle is one open playback stream.
type StreamHandle struct {
	// Reader seeks the torrent file on demand; reads block until the
	// pieces arrive (streaming, no full download first).
	Reader io.ReadSeekCloser
	// Size is the full file size (Content-Length).
	Size int64
	// Name is the base file name.
	Name string
	// MimeType is derived from the extension.
	MimeType string
}

// Typed errors: the UI and the stream server map them onto hints and
// HTTP statuses instead of string matching.
var (
	// ErrDisabled is returned by every engine call when the [torrent]
	// section is disabled. No client is ever started.
	ErrDisabled = errors.New("torrent: subsystem disabled")
	// ErrZeroInfoHash guards the library's panic on zero infohashes
	// (AddTorrentOpt → panicif.Zero): links without a usable btih are
	// rejected before they ever reach the client.
	ErrZeroInfoHash = errors.New("torrent: link has no usable infohash")
	// ErrUnsupportedLink reports links that are neither a magnet: URI,
	// an http(s) URL nor a bare 40-hex infohash.
	ErrUnsupportedLink = errors.New("torrent: unsupported link")
	// ErrNotMetainfo reports an http(s) link whose response body did
	// not parse as bencode metainfo (a topic page, a login wall — the
	// content check IS the URL-ingest guard since PR38; there is no
	// URL-shape precheck).
	ErrNotMetainfo = errors.New("torrent: content is not bencode metainfo")
	// ErrUnknownRelease is Resolve/AddLink on an infohash never added.
	ErrUnknownRelease = errors.New("torrent: release not added")
	// ErrBadFileIndex is Resolve with an out-of-bounds file index.
	ErrBadFileIndex = errors.New("torrent: file index out of range")
	// ErrMetadataTimeout is the background fetch deadline.
	ErrMetadataTimeout = errors.New("torrent: metadata fetch timed out")
	// ErrClosed is Resolve racing engine shutdown.
	ErrClosed = errors.New("torrent: engine closed")
)

// metadataTimeout bounds ONE background metadata fetch; the UI stays
// responsive regardless (statuses update live).
const metadataTimeout = 10 * time.Minute

// Engine wraps the process-wide torrent client plus the loopback HTTP
// stream server. It is lazy: NewEngine starts nothing; the first added
// link (or SeedMetaInfo) starts the client and the stream server, and
// Close tears both down. All methods are goroutine-safe.
type Engine struct {
	cfg config.Torrent
	net *netclient.Client
	log *slog.Logger
	// testNoExternal strips DHT bootstrap, UPnP and webtorrent for
	// offline E2E tests (peers arrive via explicit x.pe instead).
	testNoExternal bool

	closeOnce sync.Once
	closed    chan struct{}

	mu       sync.Mutex
	client   *torrent.Client
	httpSrv  *http.Server
	httpPort int
	dataDir  string
	releases map[InfoHash]*Release
	torrents map[InfoHash]*torrent.Torrent
	// trackerHealth is the latest probe verdict per configured
	// tracker URL (empty until the first CheckTrackers ran).
	trackerHealth map[string]TrackerHealth
	// listTrackers are the announce URLs parsed from [torrent]
	// tracker_lists (PR41) and merged into the pool; empty until the
	// first fetch ran. Guarded by mu.
	listTrackers []string
	// trackerListStatuses is the per-URL fetch outcome (empty until
	// the first fetch ran). Guarded by mu.
	trackerListStatuses []TrackerListStatus
	// probeTimeoutOverride lets tests shrink the per-tracker probe
	// budget; 0 keeps the default.
	probeTimeoutOverride time.Duration
}

// NewEngine builds an idle engine from the [torrent] config section.
// No client, no listeners, no goroutines: nothing runs until the first
// AddLink — enabling the subsystem alone never starts network
// machinery (the [torrent] enabled contract).
func NewEngine(cfg config.Torrent, net *netclient.Client, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:           cfg,
		net:           net,
		log:           log,
		closed:        make(chan struct{}),
		releases:      map[InfoHash]*Release{},
		torrents:      map[InfoHash]*torrent.Torrent{},
		trackerHealth: map[string]TrackerHealth{},
	}
}

// NewOfflineEngineForTests builds an engine with every external
// discovery channel disabled (DHT bootstrap nodes, periodic DHT
// announces, webtorrent, UPnP). TEST-ONLY by convention: provider- and
// UI-package suites use it so the default `go test -race ./...` never
// egresses to public infrastructure. Production code must construct
// engines with NewEngine and the library defaults.
func NewOfflineEngineForTests(cfg config.Torrent, net *netclient.Client, log *slog.Logger) *Engine {
	eng := NewEngine(cfg, net, log)
	eng.testNoExternal = true
	return eng
}

// probeTimeout reports the effective per-tracker probe budget.
func (e *Engine) probeTimeout() time.Duration {
	if e.probeTimeoutOverride > 0 {
		return e.probeTimeoutOverride
	}
	return trackerProbeTimeout
}

// AddLink ingests a magnet: URI (with optional dn= display name and
// x.pe peers), an http(s) URL whose response body parses as bencode
// metainfo (fetched through the shared netclient — the content check
// is the guard, the URL shape is not) or a bare 40-hex infohash. Links
// dedupe by infohash: adding a known one returns its current Release
// unchanged.
func (e *Engine) AddLink(ctx context.Context, link string) (Release, error) {
	link = strings.TrimSpace(link)
	switch {
	case strings.HasPrefix(link, "magnet:"):
		spec, err := torrent.TorrentSpecFromMagnetUri(link)
		if err != nil {
			return Release{}, fmt.Errorf("torrent: parse magnet: %w", err)
		}
		return e.addSpec(spec)
	case isInfoHashHex(link):
		// isInfoHashHex already validated the 40-hex form; the
		// library parse cannot fail here.
		ih := metainfo.NewHashFromHex(link)
		return e.addSpec(&torrent.TorrentSpec{InfoHash: ih})
	case strings.HasPrefix(link, "http://"), strings.HasPrefix(link, "https://"):
		if _, err := url.Parse(link); err != nil {
			return Release{}, fmt.Errorf("torrent: parse URL: %w", err)
		}
		if e.net == nil {
			return Release{}, fmt.Errorf("torrent: %s needs a transport but the engine has no netclient", link)
		}
		// The PR35 URL-suffix precheck (path must end in .torrent) is
		// gone as of PR38: real-world .torrent links rarely carry the
		// suffix (anirena.com/dl/N and friends), so the
		// guard is the fetched CONTENT — anything that is not bencode
		// metainfo fails loud below (topic pages, login walls).
		// This fetch rides the shared netclient — i.e.
		// network.proxy_url — NOT [torrent] proxy, which only covers
		// the library's own HTTP layer (announces, webseeds).
		resp, err := e.net.Get(ctx, link, nil)
		if err != nil {
			return Release{}, fmt.Errorf("torrent: fetch .torrent: %w", err)
		}
		mi, err := metainfo.Load(bytes.NewReader(resp.Body))
		if err != nil {
			return Release{}, fmt.Errorf("torrent: parse .torrent from %s: %w: %w", link, ErrNotMetainfo, err)
		}
		return e.AddMetaInfo(mi)
	default:
		return Release{}, fmt.Errorf("%w: %q (want magnet:, an http(s) metainfo URL or a bare infohash)",
			ErrUnsupportedLink, link)
	}
}

// AddMetaInfo ingests parsed .torrent metadata (the ingestion path for
// PR36/PR37 feed integrations and the seeding half of the E2E test).
func (e *Engine) AddMetaInfo(mi *metainfo.MetaInfo) (Release, error) {
	if mi == nil {
		return Release{}, fmt.Errorf("torrent: nil metainfo")
	}
	return e.addSpec(torrent.TorrentSpecFromMetaInfo(mi))
}

// HealthyTrackers returns the engine's current tracker pool as flat
// announce URLs for pool consumers (PR45): the configured
// [torrent] trackers plus the merged [torrent] tracker_lists entries
// (PR41), health-pruned once a check has run (fail-open
// before that), deduplicated, order-stable. An unconfigured engine
// returns nothing — nothing is invented.
func (e *Engine) HealthyTrackers() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	tiers := e.trackerTiersLocked()
	seen := make(map[string]struct{}, len(tiers))
	out := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		for _, tr := range tier {
			if _, dup := seen[tr]; dup {
				continue
			}
			seen[tr] = struct{}{}
			out = append(out, tr)
		}
	}
	return out
}

// addSpec validates the infohash (the library PANICS on zero —
// AddTorrentOpt calls panicif.Zero), dedupes and registers the release.
func (e *Engine) addSpec(spec *torrent.TorrentSpec) (Release, error) {
	if spec.InfoHash == (InfoHash{}) {
		return Release{}, fmt.Errorf("%w (magnet without a usable xt=urn:btih:)", ErrZeroInfoHash)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Enabled {
		return Release{}, ErrDisabled
	}
	if rel, ok := e.releases[spec.InfoHash]; ok {
		return *rel, nil
	}
	if err := e.startClientLocked(); err != nil {
		return Release{}, err
	}
	// User trackers ride along on every torrent (alive ones once the
	// health check has run; all of them before that — fail-open).
	spec.Trackers = append(spec.Trackers, e.trackerTiersLocked()...)
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return Release{}, fmt.Errorf("torrent: add %s: %w", spec.InfoHash.HexString(), err)
	}

	display := spec.DisplayName
	if display == "" {
		display = spec.InfoHash.HexString()
	}
	rel := &Release{
		InfoHash:    spec.InfoHash,
		DisplayName: display,
		Status:      StatusFetching,
		Quality:     ParseQuality(display),
	}
	e.releases[spec.InfoHash] = rel
	e.torrents[spec.InfoHash] = t

	if t.Info() != nil {
		e.refreshLocked(spec.InfoHash, t)
	} else {
		go e.awaitInfo(spec.InfoHash, t)
	}
	return *rel, nil
}

// awaitInfo waits for the metadata in the background (the UI never
// blocks on the network) and refreshes the release on arrival.
func (e *Engine) awaitInfo(ih InfoHash, t *torrent.Torrent) {
	timer := time.NewTimer(metadataTimeout)
	defer timer.Stop()
	select {
	case <-t.GotInfo():
	case <-timer.C:
		e.mu.Lock()
		if rel, ok := e.releases[ih]; ok {
			rel.Status = StatusError
			rel.Err = ErrMetadataTimeout.Error()
		}
		e.mu.Unlock()
		return
	case <-e.closed:
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshLocked(ih, t)
}

// refreshLocked fills the file list after the metadata became known.
func (e *Engine) refreshLocked(ih InfoHash, t *torrent.Torrent) {
	rel, ok := e.releases[ih]
	if !ok {
		return
	}
	rel.Status = StatusReady
	rel.Err = ""
	// A bare-infohash add shows hex first; the real name replaces it.
	if name := t.Name(); name != "" && name != ih.HexString() {
		rel.DisplayName = name
		rel.Quality = ParseQuality(name)
	}
	files := t.Files()
	rel.Files = make([]FileEntry, 0, len(files))
	for i, f := range files {
		rel.Files = append(rel.Files, FileEntry{
			Path:     f.Path(),
			Size:     f.Length(),
			Index:    i,
			Episodes: ParseQuality(f.Path()).Episodes,
		})
	}
	e.log.Info("torrent: metadata ready",
		"infohash", ih.HexString(), "name", rel.DisplayName, "files", len(rel.Files))
}

// Releases returns a snapshot of all known releases sorted by display
// name (stable list rendering).
func (e *Engine) Releases() []Release {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Release, 0, len(e.releases))
	for _, rel := range e.releases {
		out = append(out, *rel)
	}
	sortReleases(out)
	return out
}

// Release returns a snapshot of one release.
func (e *Engine) Release(ih InfoHash) (Release, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rel, ok := e.releases[ih]
	if !ok {
		return Release{}, false
	}
	return *rel, true
}

// sortReleases orders releases by display name.
func sortReleases(rels []Release) {
	sort.Slice(rels, func(i, j int) bool { return rels[i].DisplayName < rels[j].DisplayName })
}

// Resolve prepares one file for playback: it waits for the metadata
// (bounded by the caller's context — the TUI and the stream server
// never block without a deadline), prioritizes the chosen file's
// pieces over everything else and returns a streaming reader with the
// configured readahead window.
func (e *Engine) Resolve(ctx context.Context, ih InfoHash, fileIndex int) (StreamHandle, error) {
	// Lifecycle first: a closed engine never hands out a reader, even
	// for an already-ready release (its reads would fail on the dead
	// client).
	select {
	case <-e.closed:
		return StreamHandle{}, ErrClosed
	default:
	}
	e.mu.Lock()
	t, ok := e.torrents[ih]
	e.mu.Unlock()
	if !ok {
		return StreamHandle{}, fmt.Errorf("%w: %s", ErrUnknownRelease, ih.HexString())
	}
	// Unconditional metadata wait (PR82 final-round race fix): the
	// previous `if t.Info() == nil` guard raced the library's lazy
	// initFiles — metadata can land between the check and Files(),
	// whose slice initFiles writes. GotInfo() is an already-closed
	// channel once info is set (instant), and otherwise waits bounded
	// by ctx/closed exactly as before.
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return StreamHandle{}, fmt.Errorf("torrent: metadata for %s: %w", ih.HexString(), ctx.Err())
	case <-e.closed:
		return StreamHandle{}, ErrClosed
	}
	files := t.Files()
	if fileIndex < 0 || fileIndex >= len(files) {
		return StreamHandle{}, fmt.Errorf("%w: %d of %d files", ErrBadFileIndex, fileIndex, len(files))
	}
	// Streaming priority: only the chosen file downloads; everything
	// else is dropped to none so a 12-episode batch never pulls
	// behind the episode being watched.
	for i, f := range files {
		if i == fileIndex {
			f.SetPriority(torrent.PiecePriorityNormal)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	r := files[fileIndex].NewReader()
	if e.cfg.ReadaheadMB > 0 {
		r.SetReadahead(int64(e.cfg.ReadaheadMB) << 20)
	}
	path := files[fileIndex].Path()
	return StreamHandle{
		Reader:   r,
		Size:     files[fileIndex].Length(),
		Name:     filepath.Base(path),
		MimeType: MimeTypeOf(path),
	}, nil
}

// StreamURL is the loopback URL handed to the player. Empty while no
// stream server runs (engine idle or disabled).
func (e *Engine) StreamURL(ih InfoHash, fileIndex int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.httpPort == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/stream/%s/%d", e.httpPort, ih.HexString(), fileIndex)
}

// ListenPort reports the torrent client's TCP listen port (false when
// no client runs — the disabled/idle lifecycle proof).
func (e *Engine) ListenPort() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client == nil {
		return 0, false
	}
	if port := tcpListenPort(e.client); port != 0 {
		return port, true
	}
	return 0, false
}

// tcpListenPort reports the client's actual TCP listen port (0 when
// unknown): the ground truth after the busy-port fallback, where the
// configured port no longer describes the socket. The library announces
// this same port to trackers — client.LocalPort derives from the same
// listeners.
func tcpListenPort(cl *torrent.Client) int {
	for _, addr := range cl.ListenAddrs() {
		if ta, ok := addr.(*net.TCPAddr); ok && ta.Port != 0 {
			return ta.Port
		}
	}
	return 0
}

// Close tears down the stream server and the client; safe to call
// repeatedly and on a never-started engine.
func (e *Engine) Close() error {
	var firstErr error
	e.closeOnce.Do(func() {
		close(e.closed)
		e.mu.Lock()
		srv := e.httpSrv
		cl := e.client
		e.httpSrv = nil
		e.client = nil
		e.mu.Unlock()
		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if cl != nil {
			// v1.61 Close returns every subsystem error as a slice;
			// surface the first failure (fail loud, once).
			for _, err := range cl.Close() {
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
	})
	return firstErr
}

// startClientLocked starts the torrent client and the loopback stream
// server. Callers hold mu.
func (e *Engine) startClientLocked() error {
	if e.client != nil {
		return nil
	}
	dir := e.cfg.Dir
	if dir == "" {
		base, err := config.DataDir()
		if err != nil {
			return fmt.Errorf("torrent: resolve data dir: %w", err)
		}
		dir = filepath.Join(base, "torrents")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("torrent: create data dir %q: %w", dir, err)
	}
	e.dataDir = dir

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.DefaultStorage = storage.NewFile(dir)
	cfg.ListenPort = e.cfg.Port
	cfg.NoUpload = e.cfg.NoUpload
	// Seed follows no_upload: with upload allowed the client keeps
	// sharing completed torrents (and accepts inbound peers at all —
	// a completed seeder with Seed=false rejects every connection);
	// no_upload=true means leech-only, which is exactly Seed=false.
	cfg.Seed = !e.cfg.NoUpload
	// [torrent] proxy: the library threads the HTTP layer (http/https
	// tracker announces, webseeds, .torrent metadata fetches — and
	// ws trackers at the anacrolix/tracker layer) through it. PEER
	// data traffic and udp:// announces stay direct: documented
	// limitation of anacrolix v1.61, never faked.
	if e.cfg.Proxy != "" {
		proxyURL, err := url.Parse(e.cfg.Proxy)
		if err != nil {
			return fmt.Errorf("torrent: parse proxy: %w", err)
		}
		cfg.HTTPProxy = http.ProxyURL(proxyURL)
	}
	if e.testNoExternal {
		// Offline E2E: peers are wired explicitly via the magnet x.pe
		// parameter, so every external discovery channel goes off.
		cfg.DhtStartingNodes = func(string) dht.StartingNodesGetter {
			return func() ([]dht.Addr, error) { return nil, nil }
		}
		cfg.PeriodicallyAnnounceTorrentsToDht = false
		cfg.DisableWebtorrent = true
		cfg.UpnpID = ""
	}
	cl, err := e.newClientWithPortFallback(cfg)
	if err != nil {
		return err
	}
	if err := e.startStreamServerLocked(); err != nil {
		_ = cl.Close()
		return err
	}
	e.client = cl
	// PR41: external list feeds ride the same lazy start. They must
	// merge BEFORE the health check so the check prunes the merged
	// pool (static + lists) in one round; with no lists configured
	// the static-only kick stays as it was.
	if len(e.cfg.TrackerLists) > 0 {
		e.kickTrackerListFetch()
	} else if len(e.cfg.Trackers) > 0 {
		e.kickTrackerCheck()
	}
	// The started line logs the ACTUAL bound port: after the busy-port
	// fallback it is an OS-assigned ephemeral, not e.cfg.Port.
	e.log.Info("torrent: client started", "dir", dir, "port", tcpListenPort(cl))
	return nil
}

// maxClientBindAttempts bounds the busy-port fallback: the configured
// port once, then OS-assigned ephemeral retries (an ephemeral bind
// failing twice in a row is systemic — surface the error, don't loop).
const maxClientBindAttempts = 3

// newClientWithPortFallback starts the library client on the configured
// [torrent] port; when that bind fails (the owner's second anicli
// instance or any other app squatting the port) it retries on an
// OS-assigned ephemeral port with a LOUD WARN. Outgoing DHT/peer
// traffic works from any port — only inbound peer capacity is lost —
// so a degraded listen beats a hard failure of every torrent provider.
func (e *Engine) newClientWithPortFallback(cfg *torrent.ClientConfig) (*torrent.Client, error) {
	configured := cfg.ListenPort
	var lastErr error
	for attempt := range maxClientBindAttempts {
		if attempt > 0 {
			cfg.ListenPort = 0
		}
		cl, err := torrent.NewClient(cfg)
		if err == nil {
			if configured != 0 && cfg.ListenPort == 0 {
				// The actually bound port is what trackers see in
				// announces from now on — log it next to the WARN.
				e.log.Warn(fmt.Sprintf(
					"BT-порт %d занят — слушаем на случайном; входящие пиры ограничены (исходящие DHT/пиры работают с любого порта)",
					configured), "ephemeral_port", tcpListenPort(cl))
			}
			return cl, nil
		}
		lastErr = err
		e.log.Warn("torrent: client listen failed",
			"port", cfg.ListenPort,
			"attempt", fmt.Sprintf("%d/%d", attempt+1, maxClientBindAttempts),
			"err", err)
	}
	return nil, fmt.Errorf("torrent: start client: %w", lastErr)
}

// kickTrackerCheck runs one CheckTrackers pass on a context derived
// from the engine lifecycle: engine shutdown cancels in-flight
// probes instead of letting them ride out their timeouts.
func (e *Engine) kickTrackerCheck() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		<-e.closed
	}()
	go e.CheckTrackers(ctx)
}

// isInfoHashHex reports whether s is a bare 40-hex-char infohash.
func isInfoHashHex(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
