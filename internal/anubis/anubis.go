// Package anubis solves the Anubis proof-of-work gate (TecharoHQ)
// the rezka mirror family fronts every path with. The PoW ladder is
// pure stdlib crypto with zero transport coupling, so it lives in its
// own package and serves two consumers: the compiled hdrezka provider
// (internal/providers/hdrezka.go) and the sandboxed Lua scripts via
// the anicli.solve_anubis SDK binding (internal/lua/sdk_anubis.go) —
// the PR141 hybrid's load-bearing leg.
package anubis

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// MaxPoWIterations bounds the proof-of-work search. The observed
// difficulty is 2 (leading zero hex digits — ~10² hashes); difficulty
// 8 would still fit the cap (~4·10⁹ is out, 1.6·10⁷ hashes ≈
// seconds). Anything beyond is treated as a hostile gate.
const MaxPoWIterations = 1 << 24

// Challenge mirrors the anubis_challenge JSON the gate embeds in the
// page (the exact blob inside <script id="anubis_challenge"
// type="application/json">).
type Challenge struct {
	Rules struct {
		Algorithm  string `json:"algorithm"`
		Difficulty int    `json:"difficulty"`
	} `json:"rules"`
	Challenge struct {
		ID         string `json:"id"`
		RandomData string `json:"randomData"`
		Method     string `json:"method"`
	} `json:"challenge"`
}

// Solve ports the worker contract of Anubis (sha256-purejs.mjs +
// lib/challenge/proofofwork): the nonce N makes
// hex(sha256(randomData + strconv.Itoa(N))) start with `difficulty`
// zero hex digits. "fast" and "slow" share the same sha256 validation
// server-side (both register the same Impl); anything else fails loud.
// The wire contract is stable across the Anubis versions the family
// served (1.25.0 on the 2026-09-19 capture, 1.27.0 live 2026-10-06).
func Solve(challenge Challenge) (int, string, error) {
	switch challenge.Rules.Algorithm {
	case "fast", "slow":
		// same sha256 proof-of-work on both (anubis lib/challenge:
		// chall.Register("fast"/"slow", same Impl))
	default:
		return 0, "", fmt.Errorf("%w: unsupported anubis algorithm %q",
			contracts.ErrProvider403, challenge.Rules.Algorithm)
	}
	// The difficulty counts leading ZERO HEX DIGITS: floor(d/2) zero
	// bytes plus a zero high nibble on an odd d — checked on the raw
	// digest bytes, since hex-encoding every candidate dominated the
	// loop (8s for a difficulty-10 exhaustion on the extraction day;
	// the digest string is encoded once for the winner). A difficulty
	// beyond what a 32-byte digest can express never matches — it
	// exhausts through the same loud path (a hostile gate, not a
	// panic).
	zeroBytes := challenge.Rules.Difficulty / 2
	halfZero := challenge.Rules.Difficulty%2 == 1
	required := zeroBytes
	if halfZero {
		required++
	}
	if required > sha256.Size {
		return 0, "", fmt.Errorf("%w: anubis proof-of-work exceeded %d iterations (difficulty %d)",
			contracts.ErrProvider403, MaxPoWIterations, challenge.Rules.Difficulty)
	}
	for nonce := 0; nonce <= MaxPoWIterations; nonce++ {
		sum := sha256.Sum256([]byte(challenge.Challenge.RandomData + strconv.Itoa(nonce)))
		if !leadingZeroHex(sum[:], zeroBytes, halfZero) {
			continue
		}
		return nonce, hex.EncodeToString(sum[:]), nil
	}
	return 0, "", fmt.Errorf("%w: anubis proof-of-work exceeded %d iterations (difficulty %d)",
		contracts.ErrProvider403, MaxPoWIterations, challenge.Rules.Difficulty)
}

// leadingZeroHex reports whether the raw digest carries zeroBytes
// leading zero bytes plus, on halfZero, a zero high nibble on the
// next — the byte view of a difficulty-digit zero-hex prefix.
func leadingZeroHex(digest []byte, zeroBytes int, halfZero bool) bool {
	for _, b := range digest[:zeroBytes] {
		if b != 0 {
			return false
		}
	}
	if halfZero && digest[zeroBytes] > 0x0f {
		return false
	}
	return true
}
