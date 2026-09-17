package providers

// AllAnime v3 bootstrap client: fetches and caches the per-lane key
// material from GET {api}/client-crypto/v1/bootstrap with the x-aa-boot
// HMAC chain, trying the bT epoch candidates [mT(), gy()]. The manager
// caches material until the server-declared switchAt (plus refresh
// semantics for the AA_CRYPTO retry path) and hands over bridge-derived
// material when the pure-Go derivation fails (allanime_bridge.go).
//
// [LIVE-VERIFIED 2026-09-13]: URL shape, headers, response fields
// {epoch, epochMs, graceMs, switchAt, partB, k} captured from the live
// bootstrap; see .sdd/ledger.md "ALLANIME v3 PROTOCOL DOSSIER".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/netclient"
)

// aaDefaultBuildID is the buildId embedded in the live player chunk
// (obfuscated const cd, decoded via the sandbox as gy()). The site
// rotates it rarely; the cache (newAABuildIDCache) persists a
// discovered replacement and the bridge re-discovers it on rotation.
// [LIVE-VERIFIED 2026-09-17]: page bundles "173" (was "168" on
// 2026-09-13 — the live bootstrap now answers unknown_build_id 404 for
// the old value).
const aaDefaultBuildID = "173"

// Typed crypto failures for the AllAnime resolve chain. Callers
// classify with isAACryptoFailure; ResolveStream routes the
// rotation-class failures to the browser bridge.
var (
	// errAAGCMAuth marks a tobeparsed blob that failed GCM
	// authentication with every known key (wrong material — the
	// refresh-and-retry trigger).
	errAAGCMAuth = errors.New("allanime: tobeparsed gcm authentication failed")
	// errAADecryptFailed marks a blob still failing GCM after one
	// forced material refresh.
	errAADecryptFailed = errors.New("allanime: tobeparsed decrypt failed after key refresh")
	// errAACryptoRotated marks server-side staleness (AA_CRYPTO_STALE /
	// AA_CRYPTO_EXPIRED) that survived one refresh — the bridge trigger.
	errAACryptoRotated = errors.New("allanime: crypto material rotated (stale/expired after refresh)")
	// errAABuildUnknown marks an unknown buildId (bootstrap rejected it
	// with a 4xx / AA_CRYPTO_MISSING_BUILD) — the bridge trigger.
	errAABuildUnknown = errors.New("allanime: buildId rejected by bootstrap")
	// errAALaneMismatch marks bootstrap material served for another
	// lane (k field check).
	errAALaneMismatch = errors.New("allanime: bootstrap lane mismatch")
	// errAAForbidden marks a 403 bootstrap answer — endpoint access
	// denied. Deliberately NOT a build-unknown: a bridge session must
	// not be burned on a wall the bridge cannot change.
	errAAForbidden = errors.New("allanime: bootstrap forbidden")
	// errAARateLimited marks a 429 bootstrap answer — the endpoint is
	// throttling. Not a build verdict; retry later, never bridge.
	errAARateLimited = errors.New("allanime: bootstrap rate limited")
	// errAACaptcha marks a GraphQL NEED_CAPTCHA verdict on the episode
	// query. Not a crypto failure — the bridge cannot clear it; the
	// resolve must fail loudly instead of an empty stream.
	errAACaptcha = errors.New("allanime: captcha required (NEED_CAPTCHA)")
	// errAAInvalidated marks a refresh whose result was invalidated by
	// a concurrent mask-only bridge adoption before the waiter read
	// it (adoptBridge drops the cache mid-flight). Transient — the next
	// get re-bootstraps with the adopted mask.
	errAAInvalidated = errors.New("allanime: bootstrap settled without material (invalidated by concurrent bridge adoption)")
)

// isAACryptoFailure reports whether err belongs to the typed AllAnime
// crypto family (used by tests and the provider error surface).
func isAACryptoFailure(err error) bool {
	return errors.Is(err, errAACryptoRotated) ||
		errors.Is(err, errAABuildUnknown) ||
		errors.Is(err, errAALaneMismatch) ||
		errors.Is(err, errAAGCMAuth) ||
		errors.Is(err, errAADecryptFailed)
}

// aaBridgeMaterial is the browser-bridge handoff: live-derived crypto
// material extracted from the CURRENT player chunk inside a real
// mkissa.to page. Mask is optional (nil = use the ported table); PartB
// is optional (empty = manager bootstraps with the given mask).
type aaBridgeMaterial struct {
	BuildID string
	Epoch   int64
	PartB   string
	Mask    []byte
}

// aaMaterial is the per-lane secret material: the AES key, its epoch
// and the switchAt instant after which it must be re-bootstrapped.
type aaMaterial struct {
	Key      []byte
	Epoch    int64
	BuildID  string
	switchAt time.Time
}

// aaMaterialDeps parameterizes the manager for tests: endpoints, the
// buildId source, the HTTP client and the clock.
type aaMaterialDeps struct {
	// BootstrapBase is the full bootstrap endpoint root
	// (…/client-crypto/v1/bootstrap); buildId and k are appended.
	BootstrapBase string
	// Referer is the page origin sent as Origin/Referer.
	Referer string
	// RefererHost is the host folded into the x-aa-boot params (gT).
	RefererHost string
	// Lane is the content lane ("k7" for episodes).
	Lane string
	// BuildID resolves the current buildId (cache-backed).
	BuildID func() (string, error)
	// HTTP performs the bootstrap GET.
	HTTP *netclient.Client
	// Now is the clock (switchAt comparisons).
	Now func() time.Time
}

// aaMaterialManager caches the per-lane key material with
// singleflight-style refresh (one refresher, waiters join), expiry at
// the server-declared switchAt, and a bridge handoff override.
type aaMaterialManager struct {
	deps aaMaterialDeps

	mu           sync.Mutex
	material     *aaMaterial
	maskOverride []byte // bridge-provided mask (formula drift)
	buildID      string
	fetching     bool
	done         chan struct{}
	err          error
}

// newAAMaterialManager wires the deps.
func newAAMaterialManager(deps aaMaterialDeps) *aaMaterialManager {
	return &aaMaterialManager{deps: deps}
}

// get returns cached material while it holds, otherwise bootstraps.
func (m *aaMaterialManager) get(ctx context.Context) (*aaMaterial, error) {
	m.mu.Lock()
	if m.material != nil && m.deps.Now().Before(m.material.switchAt) {
		mat := m.material
		m.mu.Unlock()
		return mat, nil
	}
	ch := m.beginLocked()
	m.mu.Unlock()
	return m.await(ctx, ch)
}

// refresh forces a fresh bootstrap even mid-window (the decrypt retry
// path). An in-flight refresh is joined.
func (m *aaMaterialManager) refresh(ctx context.Context) (*aaMaterial, error) {
	m.mu.Lock()
	m.material = nil
	ch := m.beginLocked()
	m.mu.Unlock()
	return m.await(ctx, ch)
}

// beginLocked starts (or returns) the in-flight bootstrap channel.
func (m *aaMaterialManager) beginLocked() chan struct{} {
	if m.fetching {
		return m.done
	}
	m.fetching = true
	m.done = make(chan struct{})
	go m.runBootstrap(m.done)
	return m.done
}

// runBootstrap derives fresh material and publishes it to waiters.
func (m *aaMaterialManager) runBootstrap(done chan struct{}) {
	// The refresh runs detached so one abandoned caller cannot starve
	// the rest; the client's own timeouts bound the request.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	mat, err := m.bootstrap(ctx)

	m.mu.Lock()
	if err != nil {
		m.err = err
	} else {
		m.material = mat
		m.err = nil
	}
	m.fetching = false
	close(done)
	m.mu.Unlock()
}

// await blocks for the refresh backing done and returns its result.
func (m *aaMaterialManager) await(ctx context.Context, done chan struct{}) (*aaMaterial, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if m.material == nil {
		return nil, errAAInvalidated
	}
	return m.material, nil
}

// adoptBridge publishes bridge-derived material: a full handoff
// (epoch+partB) fills the cache directly; a mask-only handoff (formula
// drift) pins the mask override and invalidates the cache so the next
// bootstrap derives with the live bytes.
func (m *aaMaterialManager) adoptBridge(ctx context.Context, bm aaBridgeMaterial) error {
	if bm.PartB != "" {
		mask := bm.Mask
		if mask == nil {
			ported, err := aaMask(bm.BuildID)
			if err != nil {
				return err
			}
			mask = ported
		}
		key, err := aaDeriveKeyMaterial(mask, bm.PartB)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.maskOverride = mask
		m.buildID = bm.BuildID
		m.material = &aaMaterial{
			Key:      key,
			Epoch:    bm.Epoch,
			BuildID:  bm.BuildID,
			switchAt: m.deps.Now().Add(30 * time.Minute), // conservative until a bootstrap reveals switchAt
		}
		m.err = nil
		m.mu.Unlock()
		return nil
	}
	m.mu.Lock()
	m.maskOverride = bm.Mask
	m.buildID = bm.BuildID
	m.material = nil
	m.mu.Unlock()
	return nil
}

// bootstrap runs the bT strategy: for each epoch candidate compute the
// x-aa-boot chain, GET the bootstrap endpoint and on success derive the
// lane key. First successful candidate wins; exhausted candidates
// surface the last error (typed where the server said so).
func (m *aaMaterialManager) bootstrap(ctx context.Context) (*aaMaterial, error) {
	// Snapshot the mutable fields under the mutex [I2]: bootstrap runs
	// on a detached goroutine (runBootstrap) while adoptBridge and
	// setBuildID may write both fields concurrently.
	m.mu.Lock()
	buildID := m.buildID
	maskOverride := m.maskOverride
	m.mu.Unlock()
	if buildID == "" {
		bid, err := m.deps.BuildID()
		if err != nil {
			return nil, fmt.Errorf("allanime: buildId: %w", err)
		}
		buildID = bid
	}
	mask := maskOverride
	if mask == nil {
		ported, err := aaMask(buildID)
		if err != nil {
			return nil, err
		}
		mask = ported
	}

	nowMs := m.deps.Now().UnixMilli()
	var lastErr error
	for _, epoch := range aaEpochCandidates(nowMs) {
		boot, err := aaBootHeader(mask, aaBootParams{
			Lane:    m.deps.Lane,
			BuildID: buildID,
			Group:   aaKeyGroup(m.deps.RefererHost),
			Host:    m.deps.RefererHost,
			Epoch:   epoch,
		})
		if err != nil {
			return nil, err
		}
		mat, err := m.bootstrapOnce(ctx, buildID, mask, boot)
		if err == nil {
			return mat, nil
		}
		lastErr = err
		if errors.Is(err, errAABuildUnknown) || errors.Is(err, errAALaneMismatch) {
			// The server rejected the build/lane outright — later
			// epochs cannot change that verdict.
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("allanime: bootstrap: no epoch candidates")
	}
	return nil, lastErr
}

// aaBootstrapResponse is the wire shape of the bootstrap endpoint.
// [LIVE-VERIFIED 2026-09-13].
type aaBootstrapResponse struct {
	Epoch    int64  `json:"epoch"`
	PartB    string `json:"partB"`
	SwitchAt int64  `json:"switchAt"`
	K        string `json:"k"`
}

// bootstrapOnce issues one bootstrap GET and derives the key.
func (m *aaMaterialManager) bootstrapOnce(ctx context.Context, buildID string, mask []byte, boot string) (*aaMaterial, error) {
	endpoint := m.deps.BootstrapBase + "?buildId=" + url.QueryEscape(buildID) + "&k=" + url.QueryEscape(m.deps.Lane)
	headers := map[string]string{
		"x-build-id": buildID,
		"x-aa-boot":  boot,
		"Origin":     m.deps.Referer,
		"Referer":    m.deps.Referer + "/",
	}
	resp, err := m.deps.HTTP.Get(ctx, endpoint, headers)
	if err != nil {
		// Narrowed status classification [M4]: the netclient maps every
		// final failure onto *contracts.ProviderError (403 additionally
		// onto ErrProvider403, 404 onto ErrNotFound — a bare
		// *netclient.StatusError only exists for unmapped statuses), so
		// the code is read off the ProviderError wrapper. Only 400/404
		// mean the server rejected the buildId (the live endpoint
		// answers 400 there) — the bridge trigger. 403/429 are distinct
		// endpoint-access failures whose bridge session must not be
		// burned; everything else stays a generic transport error.
		var perr *contracts.ProviderError
		if errors.As(err, &perr) {
			switch perr.StatusCode {
			case http.StatusBadRequest, http.StatusNotFound:
				return nil, fmt.Errorf("%w: bootstrap status %d", errAABuildUnknown, perr.StatusCode)
			case http.StatusForbidden:
				return nil, fmt.Errorf("%w: bootstrap status %d", errAAForbidden, perr.StatusCode)
			case http.StatusTooManyRequests:
				return nil, fmt.Errorf("%w: bootstrap status %d", errAARateLimited, perr.StatusCode)
			}
		}
		return nil, fmt.Errorf("allanime bootstrap: %w", err)
	}

	var parsed aaBootstrapResponse
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return nil, fmt.Errorf("allanime bootstrap decode: %w", err)
	}
	if parsed.PartB == "" {
		return nil, errors.New("allanime bootstrap: empty partB")
	}
	if parsed.K != "" && parsed.K != m.deps.Lane {
		return nil, fmt.Errorf("%w: got %q want %q", errAALaneMismatch, parsed.K, m.deps.Lane)
	}
	key, err := aaDeriveKeyMaterial(mask, parsed.PartB)
	if err != nil {
		return nil, err
	}
	mat := &aaMaterial{
		Key:     key,
		Epoch:   parsed.Epoch,
		BuildID: buildID,
	}
	if parsed.SwitchAt > 0 {
		mat.switchAt = time.UnixMilli(parsed.SwitchAt)
	} else {
		mat.switchAt = m.deps.Now().Add(3 * time.Minute)
	}
	return mat, nil
}

// jsonUnmarshalLoose removed: the bootstrap endpoint answers plain
// JSON; encoding/json handles it directly.

// aaBuildIDCache persists the discovered buildId under the data dir so
// rotations survive restarts. An empty path (tests) keeps the cache in
// memory only; a missing/unreadable file falls back to the pinned
// default — never an error surface (the bridge re-discovers).
type aaBuildIDCache struct {
	path  string
	mu    sync.Mutex
	inMem string
}

// newAABuildIDCache builds the cache rooted at dir ("" = memory only).
func newAABuildIDCache(dir string) *aaBuildIDCache {
	if dir == "" {
		return &aaBuildIDCache{}
	}
	return &aaBuildIDCache{path: filepath.Join(dir, "allanime_build_id")}
}

// Load returns the persisted buildId or the pinned default.
func (c *aaBuildIDCache) Load() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inMem != "" {
		return c.inMem
	}
	if c.path == "" {
		return aaDefaultBuildID
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return aaDefaultBuildID
	}
	s := string(trimTrailingNewline(b))
	if s == "" {
		return aaDefaultBuildID
	}
	return s
}

// Store persists bid (write failures surface to the caller; a failed
// persistence never breaks resolution).
func (c *aaBuildIDCache) Store(bid string) error {
	if bid == "" {
		return nil
	}
	c.mu.Lock()
	c.inMem = bid
	path := c.path
	c.mu.Unlock()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(bid+"\n"), 0o600)
}

// trimTrailingNewline strips one trailing \n/\r\n from byte input
// (cache-file reads).
func trimTrailingNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n = len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// setBuildID pins a bridge-discovered buildId for the next bootstrap
// without going through the cache (the manager consults this first).
func (m *aaMaterialManager) setBuildID(bid string) {
	m.mu.Lock()
	m.buildID = bid
	m.mu.Unlock()
}
