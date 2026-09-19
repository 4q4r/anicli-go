package providers

// PR70 tests: the chunk-parser recovers the build-id material from BOTH
// live chunk generations, the derivation ladder prefers auto-derived
// material and falls back to the pinned tables, and the param-string
// order follows the parsed parts (the 174 rotation reordered it).
//
// Fixtures (verbatim live captures, see testdata/README.md):
//   - crypto_chunk_173_DhCxOiZl.js — the PR45-era generation (ground
//     truth: the parser must reproduce the pinned Go constants).
//   - crypto_chunk_174_BYlv1dKC.js — the live generation on 2026-09-19
//     (validated against the live bootstrap oracle: HTTP 200).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func aaTestChunk(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/allanime/" + name) //nolint:gosec // trusted testdata path
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// TestAAParseChunk173 reproduces the pinned PR45 constants from the
// 173 chunk verbatim — the parser is pinned to the known-good
// generation before it is trusted on new ones.
func TestAAParseChunk173(t *testing.T) {
	t.Parallel()

	prof, err := aaParseChunkMaterial(aaTestChunk(t, "crypto_chunk_173_DhCxOiZl.js"))
	if err != nil {
		t.Fatalf("parse 173 chunk: %v", err)
	}
	if prof.BuildID != "173" {
		t.Errorf("BuildID = %q, want 173", prof.BuildID)
	}
	if prof.SaltMul != aaSaltMul || prof.SaltAdd != aaSaltAdd {
		t.Errorf("salt = %d/%d, want %d/%d", prof.SaltMul, prof.SaltAdd, aaSaltMul, aaSaltAdd)
	}
	if prof.FragMul != aaFragMul || prof.FragAdd != aaFragAdd {
		t.Errorf("frag = %d/%d, want %d/%d", prof.FragMul, prof.FragAdd, aaFragMul, aaFragAdd)
	}
	if prof.BootPrefix != aaBootPrefix {
		t.Errorf("bootPrefix = %q, want %q", prof.BootPrefix, aaBootPrefix)
	}
	wantParts := [5]string{"lane", "epoch", "host", "group", "buildId"}
	if prof.Parts != wantParts {
		t.Errorf("parts = %v, want %v", prof.Parts, wantParts)
	}
	if prof.Join != "+" {
		t.Errorf("join = %q, want +", prof.Join)
	}
	for i := range prof.DM {
		if !bytes.Equal(prof.DM[i][:], aaDMDelayed[i][:]) {
			t.Errorf("dm[%d] = %x, want %x", i, prof.DM[i], aaDMDelayed[i])
		}
	}
	// The parsed profile's mask must byte-match the pinned aaMask
	// implementation (one formula, two constants sources).
	mask, err := prof.aaProfileMask()
	if err != nil {
		t.Fatalf("profile mask: %v", err)
	}
	pinned, err := aaMask("173")
	if err != nil {
		t.Fatalf("pinned mask: %v", err)
	}
	if !bytes.Equal(mask, pinned) {
		t.Errorf("parsed 173 mask %x != pinned aaMask %x", mask, pinned)
	}
	// Param string agrees with the pinned aaParamString.
	ps, err := prof.aaProfileParamString("k7", "2958", "mkissa.to", "mkissa")
	if err != nil {
		t.Fatalf("param string: %v", err)
	}
	if want := aaParamString(aaBootParams{Lane: "k7", BuildID: "173", Group: "mkissa", Host: "mkissa.to", Epoch: 2958}); ps != want {
		t.Errorf("param string = %q, want %q", ps, want)
	}
}

// TestAAParseChunk174 pins the live 2026-09-19 generation: new scalars,
// new dm table, new bootPrefix, a ROTATED param order, buildId 174 —
// and a mask that differs from the pinned 173 mask (the rotation that
// killed the pinned tables).
func TestAAParseChunk174(t *testing.T) {
	t.Parallel()

	prof, err := aaParseChunkMaterial(aaTestChunk(t, "crypto_chunk_174_BYlv1dKC.js"))
	if err != nil {
		t.Fatalf("parse 174 chunk: %v", err)
	}
	if prof.BuildID != "174" {
		t.Errorf("BuildID = %q, want 174", prof.BuildID)
	}
	if prof.SaltMul != 78 || prof.SaltAdd != 11 {
		t.Errorf("salt = %d/%d, want 78/11", prof.SaltMul, prof.SaltAdd)
	}
	if prof.FragMul != 100 || prof.FragAdd != 58 {
		t.Errorf("frag = %d/%d, want 100/58", prof.FragMul, prof.FragAdd)
	}
	if prof.BootPrefix != "3HZDdfe:" {
		t.Errorf("bootPrefix = %q, want %q", prof.BootPrefix, "3HZDdfe:")
	}
	// The 174 generation reordered the x-aa-boot param string — the
	// rotation that silently broke every pinned-order token.
	wantParts := [5]string{"buildId", "lane", "epoch", "host", "group"}
	if prof.Parts != wantParts {
		t.Errorf("parts = %v, want %v", prof.Parts, wantParts)
	}
	if prof.Join != "+" {
		t.Errorf("join = %q, want +", prof.Join)
	}
	wantDM := [4]string{"DSM46/+3XT4=", "Ij0CoXkkkN4=", "DDX5aP75hwU=", "2LRsfEUybsk="}
	for i, want := range wantDM {
		got, err := base64.StdEncoding.DecodeString(want)
		if err != nil {
			t.Fatalf("want dm decode: %v", err)
		}
		if !bytes.Equal(prof.DM[i][:], got) {
			t.Errorf("dm[%d] = %x, want %x", i, prof.DM[i], got)
		}
	}
	// The derived mask differs from the pinned 173 mask (else the whole
	// rotation would be invisible) and matches the live-oracle golden
	// (the byte string that bootstrapped HTTP 200 on 2026-09-19).
	mask, err := prof.aaProfileMask()
	if err != nil {
		t.Fatalf("profile mask: %v", err)
	}
	got := hexString(mask)
	const wantMask = "3777df816330efb2095bfae2b7942b88183a73fc59532b629e4c0fd2f5a9f841"
	if got != wantMask {
		t.Errorf("174 mask = %s, want %s", got, wantMask)
	}
	pinned, err := aaMask("173")
	if err != nil {
		t.Fatalf("pinned mask: %v", err)
	}
	if bytes.Equal(mask, pinned) {
		t.Error("174 mask must differ from the pinned 173 mask")
	}
	// The 174 param string follows the parsed order — not the pinned one.
	ps, err := prof.aaProfileParamString("k7", "2959", "mkissa.to", "mkissa")
	if err != nil {
		t.Fatalf("param string: %v", err)
	}
	if want := "174+k7+2959+mkissa.to+mkissa"; ps != want {
		t.Errorf("174 param string = %q, want %q", ps, want)
	}
}

// TestAAParseChunkRejectsGarbage: a non-chunk source fails loudly with
// the pipeline error, never a silent zero-value profile.
func TestAAParseChunkRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := aaParseChunkMaterial("this is not a chunk"); err == nil {
		t.Fatal("garbage source must fail")
	}
}

func hexString(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, hexDigits[v>>4], hexDigits[v&0x0f])
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// Material-manager ladder tests (fake chunk source + fake bootstrap API).
// ---------------------------------------------------------------------------

// aaFakeChunkSource is the injectable aaChunkSource for the ladder tests.
type aaFakeChunkSource struct {
	chunk string
	err   error
	calls int
}

func (f *aaFakeChunkSource) FetchChunk(context.Context) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.chunk, nil
}

func TestAABootstrapLadderPrefersChunkMaterial(t *testing.T) {
	t.Parallel()

	prof174, err := aaParseChunkMaterial(aaTestChunk(t, "crypto_chunk_174_BYlv1dKC.js"))
	if err != nil {
		t.Fatalf("parse 174: %v", err)
	}

	seen := make(chan [2]string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- [2]string{r.Header.Get("x-build-id"), r.Header.Get("x-aa-boot")}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch":    2959,
			"partB":    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
			"switchAt": time.Now().Add(10 * time.Minute).UnixMilli(),
			"k":        "k7",
		})
	}))
	defer srv.Close()

	chunks := &aaFakeChunkSource{chunk: aaTestChunk(t, "crypto_chunk_174_BYlv1dKC.js")}
	client := testClient(t, "allanime")
	m := newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: srv.URL,
		Referer:       "https://mkissa.to",
		RefererHost:   "mkissa.to",
		Lane:          "k7",
		BuildID: func() (string, error) {
			return aaDefaultBuildID, nil // the pinned value; must NOT be used
		},
		HTTP:   client,
		Now:    time.Now,
		Chunks: chunks,
	})
	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if mat.BuildID != "174" {
		t.Errorf("material BuildID = %q, want 174 (auto-derived)", mat.BuildID)
	}
	if chunks.calls != 1 {
		t.Errorf("chunk fetches = %d, want 1", chunks.calls)
	}
	select {
	case got := <-seen:
		if got[0] != "174" {
			t.Errorf("bootstrap x-build-id = %q, want 174", got[0])
		}
		// The boot header must equal the one derived from the PARSED
		// 174 material (mask + prefix + rotated param order).
		mask, err := prof174.aaProfileMask()
		if err != nil {
			t.Fatalf("mask: %v", err)
		}
		ps, err := prof174.aaProfileParamString("k7", "2959", "mkissa.to", "mkissa")
		if err != nil {
			t.Fatalf("params: %v", err)
		}
		want, err := aaBootHeaderFor(mask, prof174.aaProfileBootMessage(), ps)
		if err != nil {
			t.Fatalf("boot header: %v", err)
		}
		if got[1] != want {
			t.Errorf("x-aa-boot = %q, want %q (parsed-174 derivation)", got[1], want)
		}
	default:
		t.Fatal("no bootstrap request observed")
	}
}

// TestAABootstrapLadderFallsBackToPinned: the chunk fetch fails → the
// pinned tables still serve (while in their grace window).
func TestAABootstrapLadderFallsBackToPinned(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-build-id") != aaDefaultBuildID {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch":    2959,
			"partB":    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
			"switchAt": time.Now().Add(10 * time.Minute).UnixMilli(),
			"k":        "k7",
		})
	}))
	defer srv.Close()

	chunks := &aaFakeChunkSource{err: errors.New("cdn unreachable")}
	client := testClient(t, "allanime")
	m := newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: srv.URL,
		Referer:       "https://mkissa.to",
		RefererHost:   "mkissa.to",
		Lane:          "k7",
		BuildID:       func() (string, error) { return aaDefaultBuildID, nil },
		HTTP:          client,
		Now:           time.Now,
		Chunks:        chunks,
	})
	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatalf("get via pinned fallback: %v", err)
	}
	if mat.BuildID != aaDefaultBuildID {
		t.Errorf("material BuildID = %q, want pinned %q", mat.BuildID, aaDefaultBuildID)
	}
	if chunks.calls == 0 {
		t.Error("the chunk tier must have been attempted before the pinned fallback")
	}
}

// TestAABootstrapLadderLoudError: every tier exhausted → a typed,
// loud error naming the failures — never an empty success.
func TestAABootstrapLadderLoudError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // unknown_build_id for everything
	}))
	defer srv.Close()

	chunks := &aaFakeChunkSource{err: errors.New("cdn unreachable")}
	client := testClient(t, "allanime")
	m := newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: srv.URL,
		Referer:       "https://mkissa.to",
		RefererHost:   "mkissa.to",
		Lane:          "k7",
		BuildID:       func() (string, error) { return aaDefaultBuildID, nil },
		HTTP:          client,
		Now:           time.Now,
		Chunks:        chunks,
	})
	_, err := m.get(context.Background())
	if err == nil {
		t.Fatal("all tiers exhausted must fail loudly")
	}
	if !errors.Is(err, errAABuildUnknown) {
		t.Errorf("error %v must wrap the typed errAABuildUnknown", err)
	}
	if !strings.Contains(err.Error(), "cdn unreachable") {
		t.Errorf("error %v must name the chunk-tier failure", err)
	}
}

// TestAABootstrapLadderReDerivesOnBuildUnknown: a 400 from the bootstrap
// while holding cached chunk material forces a chunk RE-FETCH (rotation
// mid-flight) before the pinned fallback.
func TestAABootstrapLadderReDerivesOnBuildUnknown(t *testing.T) {
	t.Parallel()

	chunk173 := aaTestChunk(t, "crypto_chunk_173_DhCxOiZl.js")
	chunk174 := aaTestChunk(t, "crypto_chunk_174_BYlv1dKC.js")

	var rotated atomic.Bool
	seen := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bid := r.Header.Get("x-build-id")
		seen <- bid
		// The server only accepts what the CURRENT chunk generation
		// provides; older buildIds are rejected as unknown.
		current := "173"
		if rotated.Load() {
			current = "174"
		}
		if bid != current {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"epoch":    2959,
			"partB":    base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)),
			"switchAt": time.Now().Add(10 * time.Minute).UnixMilli(),
			"k":        "k7",
		})
	}))
	defer srv.Close()

	chunks := &aaFakeChunkSource{chunk: chunk173}
	client := testClient(t, "allanime")
	m := newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: srv.URL,
		Referer:       "https://mkissa.to",
		RefererHost:   "mkissa.to",
		Lane:          "k7",
		BuildID:       func() (string, error) { return aaDefaultBuildID, nil },
		HTTP:          client,
		Now:           time.Now,
		Chunks:        chunks,
	})
	// First bootstrap succeeds on the (still-live) 173 chunk material.
	mat, err := m.get(context.Background())
	if err != nil {
		t.Fatalf("first get: %v", err)
	}
	if mat.BuildID != "173" {
		t.Fatalf("first BuildID = %q, want 173", mat.BuildID)
	}
	// The generation rotates under our feet; the cached 173 material
	// is rejected → the manager must re-fetch (now serving 174) and
	// re-derive, NOT serve stale material.
	chunks.chunk = chunk174
	rotated.Store(true)
	mat2, err := m.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh after rotation: %v", err)
	}
	if mat2.BuildID != "174" {
		t.Errorf("refreshed BuildID = %q, want 174 (re-derived)", mat2.BuildID)
	}
	if chunks.calls < 2 {
		t.Errorf("chunk fetches = %d, want >=2 (re-derive after unknown_build_id)", chunks.calls)
	}
}
