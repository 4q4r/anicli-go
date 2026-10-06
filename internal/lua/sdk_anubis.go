package lua

import (
	"encoding/json"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/an0nx/anicli-go/internal/anubis"
)

// The PR141 Anubis SDK: anicli.solve_anubis(challenge_json) →
// solution_json — the pure-Go Anubis proof-of-work ladder the rezka
// family's gate serves, surfaced from the sandbox (the hdrezka hybrid:
// the script does the scraping, Go does the sha256 PoW — porting the
// hash loop into a sandbox with no bit library would be neither honest
// nor fast). The contract:
//
//   - input: the exact JSON blob the `<script id="anubis_challenge"
//     type="application/json">` tag embeds (the script extracts it
//     with anicli.regexp.match);
//   - output: the solution JSON `{"nonce":N,"digest":"<hex>",
//     "elapsed_ms":M}` — the nonce whose sha256(randomData+nonce) hex
//     digest carries the difficulty's leading zeros, plus the honest
//     solve duration the pass-challenge round-trip reports back as
//     elapsedTime (the server only logs it);
//   - failures are typed markers: anicli:invalid_input: for input
//     that does not decode as the challenge JSON (a script bug), and
//     anicli:provider_403: for an unsupported algorithm or an
//     exhausted ladder (the gate refusing — classifyVMError
//     re-attaches contracts.ErrProvider403, the same wall the
//     compiled provider raised).

// solutionJSON is the script-facing solution shape.
type solutionJSON struct {
	Nonce     int    `json:"nonce"`
	Digest    string `json:"digest"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// openSDKAnubis registers anicli.solve_anubis onto the SDK module
// table.
func openSDKAnubis(ls *lua.LState, mod *lua.LTable) {
	mod.RawSetString("solve_anubis", ls.NewFunction(sdkSolveAnubis))
}

// sdkSolveAnubis implements anicli.solve_anubis(challenge_json).
func sdkSolveAnubis(ls *lua.LState) int {
	var challenge anubis.Challenge
	if err := json.Unmarshal([]byte(ls.CheckString(1)), &challenge); err != nil {
		ls.RaiseError("anicli:invalid_input:solve_anubis: decode anubis challenge: %v", err)
		return 0
	}
	start := time.Now()
	nonce, digest, err := anubis.Solve(challenge)
	if err != nil {
		ls.RaiseError("anicli:provider_403:solve_anubis: %v", err)
		return 0
	}
	encoded, err := json.Marshal(solutionJSON{
		Nonce:     nonce,
		Digest:    digest,
		ElapsedMS: time.Since(start).Milliseconds(),
	})
	if err != nil {
		// Unreachable today: three plain JSON fields always marshal.
		// Kept loud — silent failure paths are not allowed.
		ls.RaiseError("anicli:invalid_input:solve_anubis: encode solution: %v", err)
		return 0
	}
	ls.Push(lua.LString(encoded))
	return 1
}
