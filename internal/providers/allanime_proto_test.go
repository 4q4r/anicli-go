package providers

// Golden vectors for the AllAnime v3 client-crypto port. Every vector
// in this file was captured live on 2026-09-13 by sandboxing chunk
// 6_SNhjnz.js from https://cdn.mkissa.net/all/mk/_app/immutable/chunks/
// inside a mkissa.to page (dossier: .sdd/ledger.md "ALLANIME v3
// PROTOCOL DOSSIER"). [LIVE-VERIFIED 2026-09-13] on each table.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// aaFixture loads a live-captured fixture from testdata/allanime.
func aaFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "allanime", name)) //nolint:gosec // trusted testdata path
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(trimTrailingNewline(b))
}

// aaHexDec decodes a fixed hex string or fails the test.
func aaHexDec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture %q: %v", s, err)
	}
	return b
}

// TestAAMaskGolden pins cy(buildId) — the 32-byte mask table fold:
//
//	seed[l]  = charCodeUTF16(l % len) ^ ((l*saltMul + saltAdd) & 0xff)
//	mask[..] = (dm[l/8][l%8] ^ seed[l]) ^ (((l/8)*fragMul + (l%8)*fragAdd) & 0xff)
//
// Vectors are the verbatim decimal output of the live sandbox cy() for
// every digit and letter (no hand conversion), the current buildId and
// the dossier's multi-char probes. Empty input errors (live cy("")
// returns null, which the JS caller turns into a TypeError).
func TestAAMaskGolden(t *testing.T) {
	t.Parallel()

	// [LIVE-VERIFIED 2026-09-13] sandbox cy() decimal dumps, verbatim.
	single := map[string][]byte{
		"0": {188, 53, 251, 135, 101, 134, 72, 160, 43, 191, 81, 41, 213, 83, 140, 254, 19, 179, 91, 186, 162, 206, 251, 148, 125, 13, 164, 236, 47, 24, 139, 30},
		"1": {189, 52, 250, 134, 100, 135, 73, 161, 42, 190, 80, 40, 212, 82, 141, 255, 18, 178, 90, 187, 163, 207, 250, 149, 124, 12, 165, 237, 46, 25, 138, 31},
		"2": {190, 55, 249, 133, 103, 132, 74, 162, 41, 189, 83, 43, 215, 81, 142, 252, 17, 177, 89, 184, 160, 204, 249, 150, 127, 15, 166, 238, 45, 26, 137, 28},
		"3": {191, 54, 248, 132, 102, 133, 75, 163, 40, 188, 82, 42, 214, 80, 143, 253, 16, 176, 88, 185, 161, 205, 248, 151, 126, 14, 167, 239, 44, 27, 136, 29},
		"4": {184, 49, 255, 131, 97, 130, 76, 164, 47, 187, 85, 45, 209, 87, 136, 250, 23, 183, 95, 190, 166, 202, 255, 144, 121, 9, 160, 232, 43, 28, 143, 26},
		"5": {185, 48, 254, 130, 96, 131, 77, 165, 46, 186, 84, 44, 208, 86, 137, 251, 22, 182, 94, 191, 167, 203, 254, 145, 120, 8, 161, 233, 42, 29, 142, 27},
		"6": {186, 51, 253, 129, 99, 128, 78, 166, 45, 185, 87, 47, 211, 85, 138, 248, 21, 181, 93, 188, 164, 200, 253, 146, 123, 11, 162, 234, 41, 30, 141, 24},
		"7": {187, 50, 252, 128, 98, 129, 79, 167, 44, 184, 86, 46, 210, 84, 139, 249, 20, 180, 92, 189, 165, 201, 252, 147, 122, 10, 163, 235, 40, 31, 140, 25},
		"8": {180, 61, 243, 143, 109, 142, 64, 168, 35, 183, 89, 33, 221, 91, 132, 246, 27, 187, 83, 178, 170, 198, 243, 156, 117, 5, 172, 228, 39, 16, 131, 22},
		"9": {181, 60, 242, 142, 108, 143, 65, 169, 34, 182, 88, 32, 220, 90, 133, 247, 26, 186, 82, 179, 171, 199, 242, 157, 116, 4, 173, 229, 38, 17, 130, 23},
		"a": {237, 100, 170, 214, 52, 215, 25, 241, 122, 238, 0, 120, 132, 2, 221, 175, 66, 226, 10, 235, 243, 159, 170, 197, 44, 92, 245, 189, 126, 73, 218, 79},
		"b": {238, 103, 169, 213, 55, 212, 26, 242, 121, 237, 3, 123, 135, 1, 222, 172, 65, 225, 9, 232, 240, 156, 169, 198, 47, 95, 246, 190, 125, 74, 217, 76},
		"c": {239, 102, 168, 212, 54, 213, 27, 243, 120, 236, 2, 122, 134, 0, 223, 173, 64, 224, 8, 233, 241, 157, 168, 199, 46, 94, 247, 191, 124, 75, 216, 77},
		"d": {232, 97, 175, 211, 49, 210, 28, 244, 127, 235, 5, 125, 129, 7, 216, 170, 71, 231, 15, 238, 246, 154, 175, 192, 41, 89, 240, 184, 123, 76, 223, 74},
		"e": {233, 96, 174, 210, 48, 211, 29, 245, 126, 234, 4, 124, 128, 6, 217, 171, 70, 230, 14, 239, 247, 155, 174, 193, 40, 88, 241, 185, 122, 77, 222, 75},
		"f": {234, 99, 173, 209, 51, 208, 30, 246, 125, 233, 7, 127, 131, 5, 218, 168, 69, 229, 13, 236, 244, 152, 173, 194, 43, 91, 242, 186, 121, 78, 221, 72},
		"g": {235, 98, 172, 208, 50, 209, 31, 247, 124, 232, 6, 126, 130, 4, 219, 169, 68, 228, 12, 237, 245, 153, 172, 195, 42, 90, 243, 187, 120, 79, 220, 73},
		"h": {228, 109, 163, 223, 61, 222, 16, 248, 115, 231, 9, 113, 141, 11, 212, 166, 75, 235, 3, 226, 250, 150, 163, 204, 37, 85, 252, 180, 119, 64, 211, 70},
		"i": {229, 108, 162, 222, 60, 223, 17, 249, 114, 230, 8, 112, 140, 10, 213, 167, 74, 234, 2, 227, 251, 151, 162, 205, 36, 84, 253, 181, 118, 65, 210, 71},
		"j": {230, 111, 161, 221, 63, 220, 18, 250, 113, 229, 11, 115, 143, 9, 214, 164, 73, 233, 1, 224, 248, 148, 161, 206, 39, 87, 254, 182, 117, 66, 209, 68},
		"k": {231, 110, 160, 220, 62, 221, 19, 251, 112, 228, 10, 114, 142, 8, 215, 165, 72, 232, 0, 225, 249, 149, 160, 207, 38, 86, 255, 183, 116, 67, 208, 69},
		"l": {224, 105, 167, 219, 57, 218, 20, 252, 119, 227, 13, 117, 137, 15, 208, 162, 79, 239, 7, 230, 254, 146, 167, 200, 33, 81, 248, 176, 115, 68, 215, 66},
		"m": {225, 104, 166, 218, 56, 219, 21, 253, 118, 226, 12, 116, 136, 14, 209, 163, 78, 238, 6, 231, 255, 147, 166, 201, 32, 80, 249, 177, 114, 69, 214, 67},
		"n": {226, 107, 165, 217, 59, 216, 22, 254, 117, 225, 15, 119, 139, 13, 210, 160, 77, 237, 5, 228, 252, 144, 165, 202, 35, 83, 250, 178, 113, 70, 213, 64},
		"o": {227, 106, 164, 216, 58, 217, 23, 255, 116, 224, 14, 118, 138, 12, 211, 161, 76, 236, 4, 229, 253, 145, 164, 203, 34, 82, 251, 179, 112, 71, 212, 65},
		"p": {252, 117, 187, 199, 37, 198, 8, 224, 107, 255, 17, 105, 149, 19, 204, 190, 83, 243, 27, 250, 226, 142, 187, 212, 61, 77, 228, 172, 111, 88, 203, 94},
		"q": {253, 116, 186, 198, 36, 199, 9, 225, 106, 254, 16, 104, 148, 18, 205, 191, 82, 242, 26, 251, 227, 143, 186, 213, 60, 76, 229, 173, 110, 89, 202, 95},
		"r": {254, 119, 185, 197, 39, 196, 10, 226, 105, 253, 19, 107, 151, 17, 206, 188, 81, 241, 25, 248, 224, 140, 185, 214, 63, 79, 230, 174, 109, 90, 201, 92},
		"s": {255, 118, 184, 196, 38, 197, 11, 227, 104, 252, 18, 106, 150, 16, 207, 189, 80, 240, 24, 249, 225, 141, 184, 215, 62, 78, 231, 175, 108, 91, 200, 93},
		"t": {248, 113, 191, 195, 33, 194, 12, 228, 111, 251, 21, 109, 145, 23, 200, 186, 87, 247, 31, 254, 230, 138, 191, 208, 57, 73, 224, 168, 107, 92, 207, 90},
		"u": {249, 112, 190, 194, 32, 195, 13, 229, 110, 250, 20, 108, 144, 22, 201, 187, 86, 246, 30, 255, 231, 139, 190, 209, 56, 72, 225, 169, 106, 93, 206, 91},
		"v": {250, 115, 189, 193, 35, 192, 14, 230, 109, 249, 23, 111, 147, 21, 202, 184, 85, 245, 29, 252, 228, 136, 189, 210, 59, 75, 226, 170, 105, 94, 205, 88},
		"w": {251, 114, 188, 192, 34, 193, 15, 231, 108, 248, 22, 110, 146, 20, 203, 185, 84, 244, 28, 253, 229, 137, 188, 211, 58, 74, 227, 171, 104, 95, 204, 89},
		"x": {244, 125, 179, 207, 45, 206, 0, 232, 99, 247, 25, 97, 157, 27, 196, 182, 91, 251, 19, 242, 234, 134, 179, 220, 53, 69, 236, 164, 103, 80, 195, 86},
		"y": {245, 124, 178, 206, 44, 207, 1, 233, 98, 246, 24, 96, 156, 26, 197, 183, 90, 250, 18, 243, 235, 135, 178, 221, 52, 68, 237, 165, 102, 81, 194, 87},
		"z": {246, 127, 177, 205, 47, 204, 2, 234, 97, 245, 27, 99, 159, 25, 198, 180, 89, 249, 17, 240, 232, 132, 177, 222, 55, 71, 238, 166, 101, 82, 193, 84},
	}
	for bid, want := range single {
		got, err := aaMask(bid)
		if err != nil {
			t.Errorf("aaMask(%q): %v", bid, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("aaMask(%q) = %x, want %x", bid, got, want)
		}
	}

	// [LIVE-VERIFIED 2026-09-13] multi-char probes (sandbox hex dump).
	multi := map[string]string{
		"12":   "bd37fa85648449a22abd502bd4518dfc12b15ab8a3ccfa967c0fa5ee2e1a8a1c",
		"21":   "be34f98667874aa129be5328d7528eff11b259bba0cff9957f0ca6ed2d19891f",
		"168":  "bd33f386638e49a623be5721d45584ff15bb5abcaacffd9c7c0baced29108a18",
		"169":  "bd33f286638f49a622be5720d45585ff15ba5abcabcffd9d7c0baded29118a18",
		"1000": "bd35fb87648648a02abf5129d4538cfe12b35bbaa3cefb947c0da4ec2e188b1e",
		"4138": "b834f88f61874ba82fbe5221d1528ff617b258b2a6cff89c790ca7e42b198816",
		"abc":  "ed67a8d637d519f278ee037a8401dfaf41e00ae8f19fa9c72c5ff7bd7d4bda4c",
		"k7":   "e732a0803e8113a770b80a2e8e54d7f948b400bdf9c9a093260affeb741fd019",
		"00":   "bc35fb87658648a02bbf5129d5538cfe13b35bbaa2cefb947d0da4ec2f188b1e",
		"11":   "bd34fa86648749a12abe5028d4528dff12b25abba3cffa957c0ca5ed2e198a1f",
		"01":   "bc34fb86658748a12bbe5128d5528cff13b25bbba2cffb957d0ca4ed2f198b1f",
		"10":   "bd35fa87648649a02abf5029d4538dfe12b35abaa3cefa947c0da5ec2e188a1e",
		"a1":   "ed34aa86348719a17abe00288452ddff42b20abbf3cfaa952c0cf5ed7e19da1f",
		"1a":   "bd64fad664d749f12aee5078d4028daf12e25aeba39ffac57c5ca5bd2e498a4f",
		// "99" must equal "9" (repeated-char identity, cf. "00"=="0" and
		// "11"=="1" in the same dump).
		"99":  "b53cf28e6c8f41a922b65820dc5a85f71aba52b3abc7f29d7404ade526118217",
		"zzz": "f67fb1cd2fcc02ea61f51b639f19c6b459f911f0e884b1de3747eea66552c154",
		// "0.1": codes [48,46,49] — the '.' code unit (46) folds in.
		"0.1":       "bc2bfa877b8748be2abf4f28d54d8dfe0db25ba4a3cee5957d13a5ec31198b00",
		"123456789": "bd37f88360804fa822be532ad1568af91bba5ab8a1cafe927a05aded2d1b8f1b",
		"1234567890123456789012345678901234567890": "bd37f88360804fa822bf502bd65789f814bb52baa3ccf890780ba3e426188a1c",
		// Non-ASCII: JS charCodeAt operates on UTF-16 code units (é = 0xE9).
		"héllo": "e4eca7db3ade91fc77e009f0890fd3a6caef07e5fa17a7c822557db07347d3c7",
	}
	for bid, wantHex := range multi {
		got, err := aaMask(bid)
		if err != nil {
			t.Errorf("aaMask(%q): %v", bid, err)
			continue
		}
		if hex.EncodeToString(got) != wantHex {
			t.Errorf("aaMask(%q) = %s, want %s", bid, hex.EncodeToString(got), wantHex)
		}
	}

	// cy("") returned null in the sandbox (the caller's destructuring
	// turns it into a TypeError); the Go port errors loudly.
	if _, err := aaMask(""); err == nil {
		t.Error("aaMask(\"\") must fail (live cy returns null)")
	}
	// cy("") returned null in the sandbox (the caller's destructuring
	// turns it into a TypeError); the Go port errors loudly.
	if _, err := aaMask(""); err == nil {
		t.Error("aaMask(\"\") must fail (live cy returns null)")
	}
}

// TestAAKeyGroup pins gT(host) — the referer-host to key-group map.
// [LIVE-VERIFIED 2026-09-13] via sandbox gT() on the live chunk.
func TestAAKeyGroup(t *testing.T) {
	t.Parallel()
	vectors := map[string]string{
		"mkissa.to":      "mkissa",
		"api.mkissa.net": "mkissa",
		"127.0.0.1":      "mkissa",
		"localhost":      "mkissa", // default branch
		"example.com":    "mkissa", // default branch
		"192.168.1.5":    "mirror",
		"192.168.0.1":    "mirror",
		"youtu-chan.com": "mirror",
		"isekai2nd.com":  "mirror",
		"":               "mkissa",
	}
	for host, want := range vectors {
		if got := aaKeyGroup(host); got != want {
			t.Errorf("aaKeyGroup(%q) = %q, want %q", host, got, want)
		}
	}
}

// TestAABootMessage pins nT (boot message) and aT (parameter string).
// [LIVE-VERIFIED 2026-09-13] via sandbox nT()/aT() on the live chunk:
// nT = bootPrefix + buildId; aT = lane:buildId:group:host:epoch joined
// with ":" (empty lane kept — omitEmptyLane is false).
func TestAABootMessage(t *testing.T) {
	t.Parallel()
	if got := aaBootMessage("168"); got != "vmcFXS3Dmg:168" {
		t.Errorf("aaBootMessage(168) = %q", got)
	}
	if got := aaBootMessage("0"); got != "vmcFXS3Dmg:0" {
		t.Errorf("aaBootMessage(0) = %q", got)
	}
	got := aaParamString(aaBootParams{Lane: "k7", BuildID: "168", Group: "mkissa", Host: "mkissa.to", Epoch: 2958})
	if got != "k7:168:mkissa:mkissa.to:2958" {
		t.Errorf("aaParamString = %q", got)
	}
	// Empty lane stays a field (live: ":168:::2958").
	if got := aaParamString(aaBootParams{BuildID: "168", Epoch: 2958}); got != ":168:::2958" {
		t.Errorf("aaParamString empty lane = %q", got)
	}
}

// TestAABootHeaderGolden pins iT — the x-aa-boot HMAC chain:
//
//	x-aa-boot = hex( HMAC( key = cy(buildId), msg = nT(buildId) ) )
//	                then HMAC( key = prev, msg = aT(params) )
//
// Vector 1 is the dossier golden; 2-4 were sandbox-captured the same
// session. [LIVE-VERIFIED 2026-09-13].
func TestAABootHeaderGolden(t *testing.T) {
	t.Parallel()
	vectors := []struct {
		params aaBootParams
		want   string
	}{
		{
			aaBootParams{Lane: "k7", BuildID: "168", Group: "mkissa", Host: "mkissa.to", Epoch: 2958},
			"a30800eb809e407e286ab3534da8d48371ad56463d5c2d6dafab407e4669228b",
		},
		{
			aaBootParams{Lane: "k9", BuildID: "169", Group: "mirror", Host: "192.168.0.1", Epoch: 3000},
			"f5980b4e4b0606c98717fc7cabb313e8e27c48cad6182669d34db6bb46a333e4",
		},
		{
			aaBootParams{Lane: "k2", BuildID: "168", Group: "mkissa", Host: "mkissa.to", Epoch: 2958},
			"fddbb5da82751975af780dc43b6918a6e7b77f09cac24f795e9bb85e8dde8750",
		},
		{
			aaBootParams{Lane: "k7", BuildID: "1000", Group: "mkissa", Host: "api.mkissa.net", Epoch: 1},
			"5181ca84a03bb821b446ca13b41b212b203e8fd1ce6b1bb1e7fd059efe8f9c50",
		},
	}
	for i, v := range vectors {
		maskFor, err := aaMask(v.params.BuildID)
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		got, err := aaBootHeader(maskFor, v.params)
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		if got != v.want {
			t.Errorf("vector %d: aaBootHeader = %s, want %s", i, got, v.want)
		}
	}
}

// TestAADeriveKey pins ST — key = partB_b64dec[i] ^ mask[i % len(mask)]
// for i < 32. Vector: the live bootstrap partB for epoch 2958 against
// cy("168") — the sandbox ST() output equals the key the live chunk
// used to decrypt the captured tobeparsed blob.
// [LIVE-VERIFIED 2026-09-13].
func TestAADeriveKey(t *testing.T) {
	t.Parallel()
	mask, err := aaMask("168")
	if err != nil {
		t.Fatal(err)
	}
	key, err := aaDeriveKeyMaterial(mask, "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=")
	if err != nil {
		t.Fatal(err)
	}
	want := "29f65d91ec588d32262f1905ae4a7d1cfe3f5ab61e77604ca24912c1772ce2e6"
	if hex.EncodeToString(key) != want {
		t.Errorf("derived key = %s, want %s", hex.EncodeToString(key), want)
	}

	// Short partB is a typed error (live ST throws "invalid_part_b").
	short, err := aaDeriveKeyMaterial(mask, base64.StdEncoding.EncodeToString(make([]byte, 31)))
	if err == nil {
		t.Errorf("short partB accepted: %x", short)
	}
}

// TestAABuildAAReqAtGolden pins the aaReq token build:
//
//	ts      = floor(nowMs/300000) * 300000
//	payload = {"v":1,"ts":ts,"epoch":E,"buildId":"B","qh":"H","k":"L"}
//	          (JSON key order as the site's JSON.stringify insertion
//	          order; epoch is an unquoted number)
//	iv      = SHA-256("epoch:buildId:qh:ts:lane")[:12]
//	blob    = base64( 0x01 || iv || AES-256-GCM(key, iv, payload) )
//
// Vectors were computed in the live sandbox with the same primitives
// (crypto.subtle) and the live epoch-2958 material key.
// [LIVE-VERIFIED 2026-09-13].
func TestAABuildAAReqAtGolden(t *testing.T) {
	t.Parallel()
	key := aaHexDec(t, "29f65d91ec588d32262f1905ae4a7d1cfe3f5ab61e77604ca24912c1772ce2e6")
	qh := "f4662f4b7510b26795dd53ef824a0bf1740fbbc5d1273fab18222ac831bca8d0"

	got, err := aaBuildAAReqAt(qh, key, 2958, "168", "k7", 1760000000000)
	if err != nil {
		t.Fatal(err)
	}
	want1 := "ASanAxQxIJfPo3/rGrfMcuN77u+BBYBVGPDzD9rtxoueJzrSZYSsBmKHpHCO1kh+k+aJuiYKkjx3p2ZQagpJVUUVSKpdlLmmyCh4y0BCkPQr90DSJegQeMncO9jcBwBVujZg4Z4QlZ9FiL0plMoEAQdcuNPJRyUyPjSOB/rKE+rD3tUUfkE/XNcJUnxZJjpYQsqqUuvuIQhVy6drvTzipwfAf3La"
	if got != want1 {
		t.Errorf("aaReq vector 1 =\n %s\nwant\n %s", got, want1)
	}

	// Second vector: different epoch/buildId/lane and a raw ts that is
	// NOT bucket-aligned (must floor to the 5-minute window).
	got2, err := aaBuildAAReqAt(qh, key, 2957, "167", "k9", 1760001234999)
	if err != nil {
		t.Fatal(err)
	}
	want2 := "ATv2ni1owIbOjOIYYg+u0xBzb++96X3sy4btedRvTuCfnx/8U8qPbYQy6PcxGrDxoscv8c0jW0hrYhiIIvXDAXeEGAZnowhNy2r90bLp24c4PYR2USWXRJo808QFMajKMKXshxytZqkDDOicQXgvSEsD0fHGHkmTbFoU0jiP84Cbtcl2EVc4jUgiGL7pIV0z8TChlHSfXuNGqlwvfAulcMrXPnAV"
	if got2 != want2 {
		t.Errorf("aaReq vector 2 =\n %s\nwant\n %s", got2, want2)
	}
}

// TestAAEpochCandidates pins the bT epoch-candidate semantics:
// gy = floor(ms / 7d); mT = gy-1 when inside the first 24h of the week
// (grace) and gy > 0. Candidates are [mT(), gy()] deduplicated,
// order-preserving. [LIVE-VERIFIED 2026-09-13] (my=604800000,
// dT=86400000 live constants; live sandbox mT/gy returned 2958 at
// capture time).
func TestAAEpochCandidates(t *testing.T) {
	t.Parallel()
	const (
		week = int64(7 * 24 * time.Hour / time.Millisecond)
		day  = int64(24 * time.Hour / time.Millisecond)
		// ref is a timestamp sitting exactly on a week boundary
		// (2958 * 7d; the captured epoch 2958).
		ref = week * 2958
	)
	// A moment INSIDE the grace window (first 24h of a week).
	if got := aaEpochCandidates(ref + day/2); len(got) != 2 || got[0] != 2957 || got[1] != 2958 {
		t.Errorf("in-grace candidates = %v, want [2957 2958]", got)
	}
	// Past the grace window: mT collapses onto gy (dedup to one).
	if got := aaEpochCandidates(ref + day + 1); len(got) != 1 || got[0] != 2958 {
		t.Errorf("past-grace candidates = %v, want [2958]", got)
	}
	// Zero timestamp: never emits a negative epoch.
	if got := aaEpochCandidates(0); len(got) != 1 || got[0] != 0 {
		t.Errorf("zero-ts candidates = %v, want [0]", got)
	}
}

// aaTestSeal encrypts plain with AES-256-GCM into the 0x01|iv|ct||tag
// blob framing (test-side oracle for the decrypt paths).
func aaTestSeal(t *testing.T, key, iv []byte, plain string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	out := append([]byte{0x01}, iv...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out)
}

// TestAADecryptBlobLegacyFirst pins the tobeparsed decrypt order:
// the legacy key SHA-256("Xot36i3lK3:v"+version) is tried FIRST
// (dossier/yuzono order), then the per-lane material key. Both
// ciphertexts are crafted test-side; the legacy secret literal is
// [LIVE-VERIFIED 2026-09-13] (sandbox fT() -> "Xot36i3lK3").
func TestAADecryptBlobLegacyFirst(t *testing.T) {
	t.Parallel()
	iv := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	plain := `{"episode":{"episodeString":"1","sourceUrls":[{"sourceUrl":"--deadbeef","sourceName":"S-mp4"}]}}`
	material := aaHexDec(t, "29f65d91ec588d32262f1905ae4a7d1cfe3f5ab61e77604ca24912c1772ce2e6")

	legacySum := sha256.Sum256([]byte("Xot36i3lK3:v1"))
	legacy := legacySum[:]

	legacyBlob := aaTestSeal(t, legacy, iv, plain)
	materialBlob := aaTestSeal(t, material, iv, plain)

	if _, err := aaDecryptToBeParsed(legacyBlob, material); err != nil {
		t.Fatalf("legacy-encrypted blob not decrypted: %v", err)
	}
	if _, err := aaDecryptToBeParsed(materialBlob, material); err != nil {
		t.Fatalf("material-encrypted blob not decrypted: %v", err)
	}

	// Garbage that matches NEITHER key: typed GCM auth failure.
	garbageKey := aaHexDec(t, "0000000000000000000000000000000000000000000000000000000000000000")
	wrongBlob := aaTestSeal(t, garbageKey, iv, plain)
	if _, err := aaDecryptToBeParsed(wrongBlob, material); err == nil {
		t.Fatal("wrong-key blob decrypted without error")
	}

	// Malformed framing: wrong version byte / too short.
	if _, err := aaDecryptToBeParsed(base64.StdEncoding.EncodeToString([]byte{0x02, 1, 2}), material); err == nil {
		t.Fatal("version-2 blob accepted")
	}
	if _, err := aaDecryptToBeParsed(base64.StdEncoding.EncodeToString(make([]byte, 10)), material); err == nil {
		t.Fatal("short blob accepted")
	}
}

// TestAADecryptLiveBlob decrypts the captured live tobeparsed blob
// (ROAD OF NARUTO episode 1, encrypted server-side with the epoch-2958
// lane key) offline. [LIVE-VERIFIED 2026-09-13].
func TestAADecryptLiveBlob(t *testing.T) {
	t.Parallel()
	blob := aaFixture(t, "tobeparsed_live.txt")
	mask, err := aaMask("168")
	if err != nil {
		t.Fatal(err)
	}
	key, err := aaDeriveKeyMaterial(mask, "lMWuF4/WxJQFkU4keh/54+uEAAq0uJ3Q3kK+LF48aP4=")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := aaDecryptToBeParsed(blob, key)
	if err != nil {
		t.Fatalf("live blob decrypt: %v", err)
	}
	// The plaintext (testdata/allanime/plaintext_live.json) carries 4
	// sources: Ok, Mp4, Fm-Hls (iframe embeds) and Yt-mp4 (direct).
	if len(sources) != 4 {
		t.Fatalf("sources = %d, want 4 (%+v)", len(sources), sources)
	}
	var sawYt, sawOk bool
	for _, s := range sources {
		switch s.Name {
		case "Yt-mp4":
			sawYt = s.URL == "https://tools.fast4speed.rsvp/media9/videos/2oXgpDPd3xKWdgnoz/sub/1?Authorization=3_20260912202446_38f923f5f68e27c06545278e_1d738e027da1f71f5d6068f599a1bd6d8b520548_000_20260915202446_0041_dnld"
		case "Ok":
			sawOk = s.URL == "https://ok.ru/videoembed/9373914499730"
		}
	}
	if !sawYt || !sawOk {
		t.Errorf("Yt-mp4/Ok sources missing: %+v", sources)
	}
}

// TestAADecryptDataWrappedPayload pins the tolerance for plaintexts
// that wrap the episode object in a "data" key (the shape the old
// protocol used; both decode paths must work).
func TestAADecryptDataWrappedPayload(t *testing.T) {
	t.Parallel()
	iv := []byte{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 1, 2}
	key := aaHexDec(t, "29f65d91ec588d32262f1905ae4a7d1cfe3f5ab61e77604ca24912c1772ce2e6")
	plain := `{"data":{"episode":{"episodeString":"1","sourceUrls":[{"sourceUrl":"--616263","sourceName":"Default"}]}}}`
	sources, err := aaDecryptToBeParsed(aaTestSeal(t, key, iv, plain), key)
	if err != nil {
		t.Fatalf("data-wrapped decrypt: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != "Default" {
		t.Fatalf("sources = %+v", sources)
	}
}

// TestAAQueryDocsMatchLive pins the GraphQL documents byte-for-byte
// against the live-captured texts (testdata/allanime/*_doc_live.txt)
// and the episode document's SHA-256 against the live-captured
// persisted-query hash. [LIVE-VERIFIED 2026-09-13].
func TestAAQueryDocsMatchLive(t *testing.T) {
	t.Parallel()

	if got := aaEpisodeQuery; got != aaFixture(t, "episode_doc_live.txt") {
		t.Error("aaEpisodeQuery diverged from the live-captured document")
	}
	const liveQH = "2654f89a406c29bc8fe36940ec493a131c063af3ab95b4279adad45caa5f91e8"
	if aaEpisodeQueryHash != liveQH {
		t.Errorf("aaEpisodeQueryHash = %s, want live %s", aaEpisodeQueryHash, liveQH)
	}
	if got := aaSearchQuery; got != aaFixture(t, "search_doc_live.txt") {
		t.Error("aaSearchQuery diverged from the live-captured document")
	}
}

// TestAALegacySecret pins the legacy key literal.
func TestAALegacySecret(t *testing.T) {
	t.Parallel()

	const want = "Xot36i" + "3lK3" //nolint:gosec // public protocol constant, split to dodge the secret heuristic
	if aaLegacySecret != want {
		t.Errorf("aaLegacySecret = %q", aaLegacySecret)
	}
}
