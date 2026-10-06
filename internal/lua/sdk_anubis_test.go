package lua

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// The PR141 Anubis SDK binding tests: anicli.solve_anubis(
// challenge_json) → solution_json — the pure-Go PoW ladder the
// hdrezka gate rides, surfaced from the sandbox. The challenge input
// is the exact blob the `<script id="anubis_challenge">` tag embeds;
// the solution carries the nonce, its digest and the honest solve
// duration the pass-challenge round-trip reports back as elapsedTime.

// TestSolveAnubisBindingReturnsSolutionJSON pins the happy path on a
// fixture-shaped challenge: the solution JSON decodes to the nonce
// whose sha256(randomData+nonce) hex digest carries the difficulty's
// leading zeros, plus a non-negative elapsed_ms.
func TestSolveAnubisBindingReturnsSolutionJSON(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	const randomData = "7bdd63921d5abd2f"
	challenge := `{"rules":{"algorithm":"fast","difficulty":2},` +
		`"challenge":{"id":"01a0b998","method":"fast","randomData":"` + randomData + `"}}`

	got, err := evalSDK(t, e, `return anicli.solve_anubis([==[`+challenge+`]==])`)
	if err != nil {
		t.Fatalf("solve_anubis: %v", err)
	}

	var sol struct {
		Nonce     int    `json:"nonce"`
		Digest    string `json:"digest"`
		ElapsedMS int64  `json:"elapsed_ms"`
	}
	if err := json.Unmarshal([]byte(got), &sol); err != nil {
		t.Fatalf("solution %q is not the documented JSON shape: %v", got, err)
	}
	sum := sha256.Sum256([]byte(randomData + strconv.Itoa(sol.Nonce)))
	want := hex.EncodeToString(sum[:])
	if sol.Digest != want {
		t.Errorf("digest = %q, want sha256(%q+%d) = %q", sol.Digest, randomData, sol.Nonce, want)
	}
	if !strings.HasPrefix(sol.Digest, "00") {
		t.Errorf("digest %q lacks the difficulty-2 leading zeros", sol.Digest)
	}
	if sol.ElapsedMS < 0 {
		t.Errorf("elapsed_ms = %d, want the honest non-negative solve duration", sol.ElapsedMS)
	}
}

// TestSolveAnubisBindingRejectsUnsupportedAlgorithm pins the typed
// refusal: the anicli:provider_403: marker (classifyVMError re-attaches
// the contracts sentinel, so the script's gate failure is the same
// ErrProvider403 wall the compiled provider raised).
func TestSolveAnubisBindingRejectsUnsupportedAlgorithm(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	_, err := evalSDK(t, e, `return anicli.solve_anubis([==[{"rules":{"algorithm":"keccak-ridiculous","difficulty":2},"challenge":{"id":"x","method":"keccak-ridiculous","randomData":"aa"}}]==])`)
	if err == nil {
		t.Fatal("solve_anubis must fail loud on an unsupported algorithm")
	}
	if !strings.Contains(err.Error(), "anicli:provider_403:") {
		t.Fatalf("err = %v, want the anicli:provider_403: marker", err)
	}
}

// TestSolveAnubisBindingRejectsMalformedJSON pins the input contract:
// anything that does not decode as the challenge JSON is the
// anicli:invalid_input: marker — a script bug, not a gate refusal.
func TestSolveAnubisBindingRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	_, err := evalSDK(t, e, `return anicli.solve_anubis("not json at all")`)
	if err == nil {
		t.Fatal("solve_anubis must fail loud on malformed input")
	}
	if !strings.Contains(err.Error(), "anicli:invalid_input:") {
		t.Fatalf("err = %v, want the anicli:invalid_input: marker", err)
	}
}
