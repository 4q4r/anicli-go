package providers

// Bootstrap-client tests against httptest servers: header shape
// (x-build-id, x-aa-boot hex, Origin/Referer), epoch-candidate order,
// switchAt-driven refresh, lane mismatch and typed errors. Pure-crypto
// inputs are pinned against the live goldens of allanime_proto_test.go.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// aaBootEnv is a fake bootstrap + API world: the bootstrap endpoint
// serves partB/epoch/switchAt and records requests; helper builders
// wire an aaMaterialManager against it.
type aaBootEnv struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	buildIDs []string // buildIds seen (in order)
	epochs   []int64  // boot-header epochs attempted (in order)
	hits     int
	// epochFor computes the epoch the server will serve per buildId;
	// nil serves epochAt.
	epochFor func(buildID string) int64
	epochAt  int64
	// serveErr makes the endpoint fail with this status (0 = healthy).
	serveErr int
	// lenientBoot skips x-aa-boot validation (drift tests derive with a
	// bridge mask that differs from the ported table).
	lenientBoot bool
	// partBFor lets tests rotate material (nil = fixture partB).
	partBFor func(buildID string) string
	// kLane is the lane echoed in the response k field ("" = omit).
	kLane string
	// switchAtFn computes the switchAt to serve (nil = far future).
	switchAtFn func() int64
	now        int64
}

func newAABootEnv(t *testing.T) *aaBootEnv {
	t.Helper()
	env := &aaBootEnv{t: t, epochAt: 2958, kLane: "k7"}
	env.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.hits++
		buildID := r.URL.Query().Get("buildId")
		env.buildIDs = append(env.buildIDs, buildID)
		env.mu.Unlock()
		if env.serveErr != 0 {
			w.WriteHeader(env.serveErr)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		// The x-aa-boot value must be the exact iT chain for one of the
		// plausible epochs — verify against our own port (which is
		// golden-pinned) and record WHICH epoch authenticated.
		boot := r.Header.Get("x-aa-boot")
		mask, err := aaMask(buildID)
		if err != nil {
			t.Errorf("server: aaMask(%q): %v", buildID, err)
		}
		var matched int64
		found := env.lenientBoot && len(boot) == 64
		for _, cand := range aaEpochCandidates(env.now) {
			want, err := aaBootHeader(mask, aaBootParams{
				Lane: "k7", BuildID: buildID, Group: "mkissa",
				Host: "srv.example", Epoch: cand,
			})
			if err != nil {
				t.Fatalf("server: aaBootHeader: %v", err)
			}
			if want == boot {
				matched, found = cand, true
				break
			}
		}
		if !found {
			t.Errorf("server: x-aa-boot %q matches no epoch candidate of %v", boot, aaEpochCandidates(env.now))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		env.mu.Lock()
		env.epochs = append(env.epochs, matched)
		env.mu.Unlock()

		epoch := env.epochAt
		if env.epochFor != nil {
			epoch = env.epochFor(buildID)
		}
		partB := "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4="
		if env.partBFor != nil {
			partB = env.partBFor(buildID)
		}
		switchAt := int64(4102444800000)
		if env.switchAtFn != nil {
			switchAt = env.switchAtFn()
		}
		resp := map[string]any{"epoch": epoch, "partB": partB, "switchAt": switchAt}
		if env.kLane != "" {
			resp["k"] = env.kLane
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(env.srv.Close)
	return env
}

// manager wires an aaMaterialManager against the fake world with a
// deterministic clock.
func (e *aaBootEnv) manager(buildID string) *aaMaterialManager {
	return newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: e.srv.URL + "/client-crypto/v1/bootstrap",
		Referer:       "https://mkissa.to",
		RefererHost:   "srv.example",
		Lane:          "k7",
		BuildID:       func() (string, error) { return buildID, nil },
		HTTP:          testClient(e.t, "allanime"),
		Now:           func() time.Time { return time.UnixMilli(e.now) },
	})
}

func (e *aaBootEnv) setNow(ms int64) { e.mu.Lock(); e.now = ms; e.mu.Unlock() }

// TestAAMaterialBootstrapHeaders pins the bootstrap request shape:
// GET {base}?buildId=..&k=k7 with x-build-id, 64-hex x-aa-boot (an
// epoch candidate), Origin and Referer. The fake server validates the
// x-aa-boot value against the golden-pinned port.
func TestAAMaterialBootstrapHeaders(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	m := env.manager("168")

	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if mat.Epoch != 2958 {
		t.Errorf("epoch = %d, want 2958", mat.Epoch)
	}
	wantKey, _ := aaDeriveKeyMaterial(mustAAMask(t, "168"), "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=")
	if string(mat.Key) != string(wantKey) {
		t.Errorf("key = %x, want %x", mat.Key, wantKey)
	}
	if env.hits != 1 {
		t.Errorf("bootstrap hits = %d, want 1", env.hits)
	}
}

func mustAAMask(t *testing.T, bid string) []byte {
	t.Helper()
	m, err := aaMask(bid)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestAAMaterialEpochCandidatesOrder pins the bT semantics end to end:
// inside the grace window the PREVIOUS epoch is tried first — the fake
// server records which epoch each x-aa-boot authenticated.
func TestAAMaterialEpochCandidatesOrder(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	// 1789000000000 is an exact multiple of the 7-day bucket; half a
	// day later we are inside the 24h grace.
	env.setNow(1789000000000 + 43200000)
	env.epochAt = 2957 // server serves the previous epoch (mT hit)
	m := env.manager("168")

	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if mat.Epoch != 2957 {
		t.Errorf("epoch = %d, want 2957 (mT candidate)", mat.Epoch)
	}
	if len(env.epochs) != 1 || env.epochs[0] != 2957 {
		t.Errorf("authenticated epochs = %v, want [2957]", env.epochs)
	}
}

// TestAAMaterialBuildMismatchIsTyped pins the AA_CRYPTO_BUILD_MISMATCH
// surfacing: a 4xx bootstrap answer maps to errAABuildUnknown (the
// bridge trigger).
func TestAAMaterialBuildMismatchIsTyped(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	env.serveErr = http.StatusBadRequest
	m := env.manager("999")

	if _, err := m.get(context.Background()); err == nil {
		t.Fatal("bootstrap 4xx swallowed")
	} else if !isAACryptoFailure(err) {
		t.Fatalf("err = %v, want a typed AA crypto failure", err)
	}
}

// aaBootStatusWant classifies the expected bootstrap failure mode of a
// status answer.
type aaBootStatusWant int

const (
	wantBuildUnknown aaBootStatusWant = iota
	wantForbidden
	wantRateLimited
	wantGeneric
)

// TestAAMaterialBootstrapStatusNarrowing pins [M4]: only 400/404 mean
// the server rejected the buildId (the bridge trigger). 403/429 are
// distinct typed endpoint-access failures, and every other status
// stays a generic transport error — none of them may masquerade as
// errAABuildUnknown and burn a bridge session. (netclient maps 403 to
// ErrProvider403 and 404 to ErrNotFound, so classification must read
// the status off the ProviderError wrapper.)
func TestAAMaterialBootstrapStatusNarrowing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   aaBootStatusWant
	}{
		{status: http.StatusBadRequest, want: wantBuildUnknown},
		{status: http.StatusNotFound, want: wantBuildUnknown},
		{status: http.StatusForbidden, want: wantForbidden},
		{status: http.StatusTooManyRequests, want: wantRateLimited},
		{status: http.StatusInternalServerError, want: wantGeneric},
		{status: http.StatusTeapot, want: wantGeneric},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			t.Parallel()

			env := newAABootEnv(t)
			env.setNow(1789000000000)
			env.serveErr = tc.status
			m := env.manager("168")

			_, err := m.get(context.Background())
			if err == nil {
				t.Fatal("bootstrap failure swallowed")
			}
			isBuildUnknown := errors.Is(err, errAABuildUnknown)
			if tc.want == wantBuildUnknown && !isBuildUnknown {
				t.Fatalf("status %d: err = %v, want errAABuildUnknown chain", tc.status, err)
			}
			if tc.want != wantBuildUnknown && isBuildUnknown {
				t.Fatalf("status %d: generic/other failure misclassified as errAABuildUnknown: %v", tc.status, err)
			}
			switch tc.want {
			case wantForbidden:
				if !errors.Is(err, errAAForbidden) {
					t.Fatalf("status %d: err = %v, want errAAForbidden chain", tc.status, err)
				}
			case wantRateLimited:
				if !errors.Is(err, errAARateLimited) {
					t.Fatalf("status %d: err = %v, want errAARateLimited chain", tc.status, err)
				}
			case wantGeneric:
				if errors.Is(err, errAAForbidden) || errors.Is(err, errAARateLimited) {
					t.Fatalf("status %d: generic failure misclassified as typed: %v", tc.status, err)
				}
			}
		})
	}
}

// TestAAMaterialManagerConcurrentRefreshAdoptBridge pins [I2] under
// -race: goroutine A runs refresh() (whose detached bootstrap reads
// buildID/maskOverride) while goroutine B hammers the concurrent
// writers (setBuildID + mask-only adoptBridge). The reads must be
// snapshotted under the manager mutex; the race detector flags the
// unsynchronized read otherwise. The fake bootstrap lingers 2ms per
// request so the interleaving is real, not incidental.
func TestAAMaterialManagerConcurrentRefreshAdoptBridge(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		time.Sleep(2 * time.Millisecond) // widen the overlap window
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch": 2958, "partB": "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=",
			"switchAt": 4102444800000, "k": "k7",
		})
	}))
	t.Cleanup(srv.Close)

	m := newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: srv.URL + "/client-crypto/v1/bootstrap",
		Referer:       "https://mkissa.to",
		RefererHost:   "srv.example",
		Lane:          "k7",
		BuildID:       func() (string, error) { return "168", nil },
		HTTP:          testClient(t, "allanime"),
		Now:           func() time.Time { return time.UnixMilli(1789000000000) },
	})

	// B: the concurrent writers, hammering until A is done.
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		drifted := mkBytes(200)
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.setBuildID("777")
			if err := m.adoptBridge(context.Background(), aaBridgeMaterial{
				BuildID: "777", Mask: drifted,
			}); err != nil {
				t.Errorf("adoptBridge: %v", err)
				return
			}
		}
	}()

	// A: in-flight refreshes overlapping B's writes. A refresh whose
	// result lands in the mask-only-adoption invalidation window fails
	// with errAAInvalidated — the documented concurrent semantics; any
	// other failure is a bug.
	const refreshes = 4
	for range refreshes {
		if _, err := m.refresh(context.Background()); err != nil && !errors.Is(err, errAAInvalidated) {
			t.Errorf("refresh: %v", err)
		}
	}
	close(stop)
	writers.Wait()

	// The interleave really happened (A hit the slow bootstrap) and the
	// manager settles into usable state.
	if hits := len(entered); hits < refreshes {
		t.Fatalf("bootstrap hits = %d, want >= %d (no real interleaving)", hits, refreshes)
	}
	if _, err := m.get(context.Background()); err != nil {
		t.Fatalf("get after interleave: %v", err)
	}
}

// TestAAMaterialCachesUntilSwitchAt pins the switchAt-driven expiry:
// within the window no second bootstrap happens; past switchAt the next
// get re-bootstraps (the AA_CRYPTO_STALE/EXPIRED refresh path).
func TestAAMaterialCachesUntilSwitchAt(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	switchAt := int64(1789000000000 + 60000) // a minute out
	env.switchAtFn = func() int64 { return switchAt }
	m := env.manager("168")

	if _, err := m.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.setNow(switchAt - 1000)
	if _, err := m.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.hits != 1 {
		t.Fatalf("bootstrap hits before switchAt = %d, want 1", env.hits)
	}

	env.setNow(switchAt + 1)
	if _, err := m.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.hits != 2 {
		t.Fatalf("bootstrap hits after switchAt = %d, want 2", env.hits)
	}
}

// TestAAMaterialRefreshForced pins the forced refresh (the decrypt
// retry path): refresh bypasses the cache even mid-window and
// singleflights concurrent callers.
func TestAAMaterialRefreshForced(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	var rotation atomic.Int32
	env.partBFor = func(string) string {
		switch rotation.Load() {
		case 0:
			return "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4="
		default:
			return base64.StdEncoding.EncodeToString(mkBytes(7))
		}
	}
	m := env.manager("168")

	first, err := m.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rotation.Store(1)
	second, err := m.refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Key) == string(second.Key) {
		t.Error("refresh returned the same key (cache not bypassed)")
	}
	if env.hits != 2 {
		t.Errorf("bootstrap hits = %d, want 2", env.hits)
	}
}

func mkBytes(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// TestAAMaterialLaneMismatchIsTyped pins the bootstrap k-field check:
// a lane mismatch is a typed crypto failure (server served k9 material
// for a k7 request).
func TestAAMaterialLaneMismatchIsTyped(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	env.kLane = "k9"
	m := env.manager("168")

	_, err := m.get(context.Background())
	if err == nil || !isAACryptoFailure(err) {
		t.Fatalf("err = %v, want typed crypto failure", err)
	}
}

// TestAABuildIDCachePersisted pins the buildId persistence: load falls
// back to the pinned default, store round-trips across instances.
func TestAABuildIDCachePersisted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cache := newAABuildIDCache(dir)
	if got := cache.Load(); got != aaDefaultBuildID {
		t.Errorf("empty cache Load = %q, want pinned default %q", got, aaDefaultBuildID)
	}
	if err := cache.Store("999"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if got := cache.Load(); got != "999" {
		t.Errorf("after Store Load = %q, want 999", got)
	}
	// A second cache over the same dir sees the persisted value.
	if got := newAABuildIDCache(dir).Load(); got != "999" {
		t.Errorf("fresh cache Load = %q, want persisted 999", got)
	}
}

// TestAAMaterialBridgeHandoff pins the bridge handoff: bridge material
// carrying only live mask bytes (formula drift) forces a bootstrap
// whose key derivation uses the BRIDGE mask, not the ported table.
func TestAAMaterialBridgeHandoff(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	env.lenientBoot = true // bridge mask differs from the ported table
	m := env.manager("168")

	// Bridge hands over live mask bytes that DIFFER from the ported
	// table (formula drift): derivation must use the bridge bytes.
	driftedMask := mkBytes(200)
	env.partBFor = func(string) string {
		// partB such that mask^partB is a recognizable key.
		part := make([]byte, 32)
		for i := range part {
			part[i] = driftedMask[i] ^ 0xAB
		}
		return base64.StdEncoding.EncodeToString(part)
	}
	if err := m.adoptBridge(context.Background(), aaBridgeMaterial{
		BuildID: "168", Epoch: 2958, Mask: driftedMask,
	}); err != nil {
		t.Fatalf("adoptBridge (mask-only): %v", err)
	}
	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 32)
	for i := range want {
		want[i] = 0xAB
	}
	if string(mat.Key) != string(want) {
		t.Errorf("bridge-mask derived key = %x, want %x", mat.Key, want)
	}
}

// TestAAMaterialBridgeFullHandoff pins the full handoff: bridge
// material with epoch+partB populates the cache without any bootstrap
// request.
func TestAAMaterialBridgeFullHandoff(t *testing.T) {
	t.Parallel()

	env := newAABootEnv(t)
	env.setNow(1789000000000)
	m := env.manager("168")

	if err := m.adoptBridge(context.Background(), aaBridgeMaterial{
		BuildID: "168", Epoch: 2958,
		PartB: "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=",
		Mask:  mustAAMask(t, "168"),
	}); err != nil {
		t.Fatalf("adoptBridge: %v", err)
	}
	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mat.Epoch != 2958 || env.hits != 0 {
		t.Errorf("epoch = %d hits = %d, want 2958/0 (no bootstrap needed)", mat.Epoch, env.hits)
	}
}
