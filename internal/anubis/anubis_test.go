package anubis

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// testChallenge builds a fixture-shaped challenge (the real Anubis
// capture's shape; the randomData and ids are sanitized).
func testChallenge(algo string, difficulty int, randomData string) Challenge {
	var c Challenge
	c.Rules.Algorithm = algo
	c.Rules.Difficulty = difficulty
	c.Challenge.ID = "01a0b998-c0b3-736a-9d4c-2d3cfd35d73f"
	c.Challenge.RandomData = randomData
	c.Challenge.Method = algo
	return c
}

// TestSolveFindsTheLeadingZeroNonce pins the PoW contract: the solved
// digest is hex(sha256(randomData+nonce)) carrying exactly the
// difficulty's leading zero hex digits, and the returned nonce is the
// SMALLEST one satisfying it (the ladder iterates from 0).
func TestSolveFindsTheLeadingZeroNonce(t *testing.T) {
	t.Parallel()

	const randomData = "7bdd63921d5abd2f"
	nonce, digest, err := Solve(testChallenge("fast", 2, randomData))
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	sum := sha256.Sum256([]byte(randomData + strconv.Itoa(nonce)))
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest = %q, want sha256(%q+%d) = %q", digest, randomData, nonce, want)
	}
	if !strings.HasPrefix(digest, "00") {
		t.Fatalf("digest %q lacks the difficulty-2 leading zeros", digest)
	}
	for smaller := range nonce {
		s := sha256.Sum256([]byte(randomData + strconv.Itoa(smaller)))
		if strings.HasPrefix(hex.EncodeToString(s[:]), "00") {
			t.Fatalf("nonce %d is not the smallest solution: %d also satisfies", nonce, smaller)
		}
	}
}

// TestSolveAcceptsBothAlgorithmNames pins the "fast"/"slow" parity:
// both register the same sha256 validation server-side (the Anubis
// challenge library registers one Impl under both names), so both
// solve identically.
func TestSolveAcceptsBothAlgorithmNames(t *testing.T) {
	t.Parallel()

	for _, algo := range []string{"fast", "slow"} {
		nonce, digest, err := Solve(testChallenge(algo, 2, "7bdd63921d5abd2f"))
		if err != nil {
			t.Fatalf("Solve(%s): %v", algo, err)
		}
		if !strings.HasPrefix(digest, "00") || nonce < 0 {
			t.Fatalf("Solve(%s) = %d/%q, want a leading-zero digest", algo, nonce, digest)
		}
	}
}

// TestSolveRejectsUnsupportedAlgorithm pins the loud refusal: anything
// but fast/slow is a typed 403-class failure naming the algorithm,
// never a silent pass-through.
func TestSolveRejectsUnsupportedAlgorithm(t *testing.T) {
	t.Parallel()

	_, _, err := Solve(testChallenge("keccak-ridiculous", 2, "aa"))
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("err = %v, want ErrProvider403", err)
	}
	if !strings.Contains(err.Error(), "keccak-ridiculous") {
		t.Fatalf("err = %v, must name the unsupported algorithm", err)
	}
}

// TestSolveFailsLoudOnExhaustion pins the bounded ladder: a difficulty
// no honest solve reaches within MaxPoWIterations (10 zero hex digits
// — 16^10 ≈ 10¹² expected hashes against a 2²⁴ cap) exhausts into the
// typed 403-class failure naming the cap, instead of looping forever.
func TestSolveFailsLoudOnExhaustion(t *testing.T) {
	t.Parallel()

	_, _, err := Solve(testChallenge("fast", 10, "aa"))
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("err = %v, want ErrProvider403", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(MaxPoWIterations)) {
		t.Fatalf("err = %v, must name the iteration cap", err)
	}
}

// TestSolveOddDifficulty pins the odd-difficulty semantics: three zero
// HEX digits means one zero byte plus a zero high nibble on the next —
// verified against the hex digest recompute.
func TestSolveOddDifficulty(t *testing.T) {
	t.Parallel()

	const randomData = "cafe"
	nonce, digest, err := Solve(testChallenge("fast", 3, randomData))
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	sum := sha256.Sum256([]byte(randomData + strconv.Itoa(nonce)))
	if want := hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}
	if !strings.HasPrefix(digest, "000") || strings.HasPrefix(digest, "0000") {
		t.Fatalf("digest %q is not the smallest difficulty-3 solution shape", digest)
	}
}

// TestSolveImpossibleDifficultyFailsLoud pins the hostile-gate guard:
// a difficulty no 32-byte digest can ever satisfy (65 zero hex digits)
// exhausts into the typed failure without panicking on the byte checks.
func TestSolveImpossibleDifficultyFailsLoud(t *testing.T) {
	t.Parallel()

	_, _, err := Solve(testChallenge("fast", 65, "aa"))
	if !errors.Is(err, contracts.ErrProvider403) {
		t.Fatalf("err = %v, want ErrProvider403", err)
	}
}
