package anubis

import (
	"encoding/json"
	"testing"
)

// The PoW bench rode the compiled hdrezka provider until the PR141
// migration moved the solver here; the difficulty-2 shape is the
// per-page-fetch cost when the rezka family's gate engages
// (live-verified 2026-09-19 and 2026-10-06).

// buildBenchChallenge carries the real capture's randomData shape with
// the difficulty clamped to 2 (bounded: ~256 sha256 hashes on average
// — microsecond scale, deterministic enough for a bench).
func buildBenchChallenge(b *testing.B) Challenge {
	b.Helper()

	const blob = `{"rules":{"algorithm":"fast","difficulty":2},` +
		`"challenge":{"id":"01a0b998-c0b3-736a-9d4c-2d3cfd35d73f","method":"fast",` +
		`"randomData":"7bdd63921d5abd2fa71a8b72147128bd8a55279d0fb3d4f7a41544fa477c0a88c52a5e3736c94d806d8100ee131c588bcd06bc1a7a7585f8cbb447bc43f0e9b4"}}`
	var ch Challenge
	if err := json.Unmarshal([]byte(blob), &ch); err != nil {
		b.Fatalf("decode bench challenge: %v", err)
	}
	return ch
}

// BenchmarkSolveD2 solves a difficulty-2 anubis proof of work.
func BenchmarkSolveD2(b *testing.B) {
	b.ReportAllocs()
	ch := buildBenchChallenge(b)
	sink := 0
	for b.Loop() {
		nonce, digest, err := Solve(ch)
		if err != nil {
			b.Fatalf("solve: %v", err)
		}
		sink += nonce + len(digest)
	}
	if sink == -1 {
		b.Fatal("unreachable sink")
	}
}
