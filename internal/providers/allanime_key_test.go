package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// aaTestSeal encrypts plaintext exactly like the AllAnime wire format
// (0x01 || nonce(12) || AES-256-GCM ct||tag, standard base64). It is the
// test-side oracle for both the tobeparsed blob and token layout.
func aaTestSeal(t *testing.T, key []byte, payload string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("test cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("test gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("test nonce: %v", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(payload), nil)
	out := append([]byte{0x01}, nonce...)
	return base64.StdEncoding.EncodeToString(append(out, sealed...))
}

// aaTestOpen strips and checks the framing and returns nonce + sealed
// body of a wire-format token.
func aaTestOpen(t *testing.T, token string) (nonce, sealed []byte) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token not std base64: %v", err)
	}
	if len(raw) < 29 || raw[0] != 0x01 {
		t.Fatalf("token framing wrong: version=%d len=%d", raw[0], len(raw))
	}
	return raw[1:13], raw[13:]
}

// aaTestKey builds a deterministic 32-byte test key.
func aaTestKey(seed byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func TestAABuildAAReqAtDeterministic(t *testing.T) {
	t.Parallel()

	key := aaTestKey(3)
	a, err := aaBuildAAReqAt("abcd", key, "4130", 1_800_000_123)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := aaBuildAAReqAt("abcd", key, "4130", 1_800_000_999)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a != b {
		t.Fatalf("tokens differ inside one 5-minute window:\n%s\n%s", a, b)
	}
}

func TestAABuildAAReqAtBucketEdges(t *testing.T) {
	t.Parallel()

	key := aaTestKey(1)
	// 1_799_999_999 and 1_800_000_000 straddle the bucket floor
	// (1_800_000_000 / 300_000 == 6000).
	lo, err := aaBuildAAReqAt("qh", key, "41", 1_799_999_999)
	if err != nil {
		t.Fatalf("build lo: %v", err)
	}
	hi, err := aaBuildAAReqAt("qh", key, "41", 1_800_000_000)
	if err != nil {
		t.Fatalf("build hi: %v", err)
	}
	if lo == hi {
		t.Fatal("tokens identical across the bucket boundary")
	}
}

// TestAABuildAAReqAtPayload pins the exact token construction: payload
// JSON {"v":1,"ts":%d,"epoch":%s,"qh":%q} (epoch unquoted), IV =
// SHA-256("epoch:qh:ts")[:12], AES-256-GCM over the payload, framing
// 0x01 || IV || ct||tag (Goanime allanime crypto.go, ani-cli #1772).
func TestAABuildAAReqAtPayload(t *testing.T) {
	t.Parallel()

	key := aaTestKey(7)
	const qh = "f4662f4b"
	const epoch = "4130"
	const now = int64(2_400_123_456)
	ts := now / 300_000 * 300_000

	token, err := aaBuildAAReqAt(qh, key, epoch, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	nonce, sealed := aaTestOpen(t, token)

	wantPayload := fmt.Sprintf(`{"v":1,"ts":%d,"epoch":%s,"qh":%q}`, ts, epoch, qh)
	ivHash := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", epoch, qh, ts)))
	if string(nonce) != string(ivHash[:12]) {
		t.Fatalf("IV = %x, want SHA-256(\"epoch:qh:ts\")[:12] = %x", nonce, ivHash[:12])
	}

	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		t.Fatalf("open sealed payload: %v", err)
	}
	if string(plain) != wantPayload {
		t.Fatalf("payload = %q, want %q", plain, wantPayload)
	}
}

// TestAABuildAAReqAtBadKey checks the loud failure on a non-AES key.
func TestAABuildAAReqAtBadKey(t *testing.T) {
	t.Parallel()

	if _, err := aaBuildAAReqAt("qh", []byte("short"), "41", 0); err == nil {
		t.Fatal("short key accepted, want error")
	}
}

// TestAADecodeToBeParsedRoundTrip feeds a blob sealed with the same key
// and expects the decrypted sourceUrls with the "--" prefix trimmed.
func TestAADecodeToBeParsedRoundTrip(t *testing.T) {
	t.Parallel()

	key := aaTestKey(11)
	plain := `{"data":{"episode":{"episodeString":"1","sourceUrls":[` +
		`{"sourceUrl":"--0805171b060c2f08051b3f","sourceName":"S-mp4"},` +
		`{"sourceUrl":"https://cdn.example/v.mp4","sourceName":"Yt-mp4"}]}}}`
	blob := aaTestSeal(t, key, plain)

	sources, err := decodeToBeParsed(blob, key)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	if sources[0].Name != "S-mp4" || sources[0].URL != "0805171b060c2f08051b3f" {
		t.Errorf("source[0] = %+v, want trimmed -- prefix", sources[0])
	}
	if sources[1].Name != "Yt-mp4" || sources[1].URL != "https://cdn.example/v.mp4" {
		t.Errorf("source[1] = %+v", sources[1])
	}
}

// TestAADecodeToBeParsedEncodings accepts URL-safe alphabets too
// (Goanime accepts std, URL and raw-URL base64).
func TestAADecodeToBeParsedEncodings(t *testing.T) {
	t.Parallel()

	key := aaTestKey(5)
	plain := `{"data":{"episode":{"sourceUrls":[{"sourceUrl":"--ab","sourceName":"x"}]}}}`
	std := aaTestSeal(t, key, plain)

	sources, err := decodeToBeParsed(std, key)
	if err != nil || len(sources) != 1 {
		t.Fatalf("std base64 rejected: %v (sources %d)", err, len(sources))
	}

	raw, err := base64.StdEncoding.DecodeString(std)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	urlSafe := base64.URLEncoding.EncodeToString(raw)
	if _, err := decodeToBeParsed(urlSafe, key); err != nil {
		t.Fatalf("url-safe base64 rejected: %v", err)
	}
}

func TestAADecodeToBeParsedFailures(t *testing.T) {
	t.Parallel()

	key := aaTestKey(9)
	plain := `{"data":{"episode":{"sourceUrls":[{"sourceUrl":"--ab","sourceName":"x"}]}}}`

	if _, err := decodeToBeParsed("not base64 !!!", key); err == nil {
		t.Error("garbage base64 accepted")
	}
	if _, err := decodeToBeParsed(base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}), key); err == nil {
		t.Error("short blob accepted")
	}
	if _, err := decodeToBeParsed(aaTestSeal(t, aaTestKey(10), plain), key); err == nil {
		t.Error("wrong-key blob accepted, want GCM auth failure")
	}

	// Valid encryption, no sources inside: loud, not a silent empty set.
	if _, err := decodeToBeParsed(aaTestSeal(t, key, `{"data":{"episode":{}}}`), key); err == nil {
		t.Error("source-less payload accepted")
	}
}

// TestAADecodeToBeParsedRegexFallback pins the fallback extraction when
// the plaintext is not clean JSON (Goanime keeps a sed-style regex
// fallback for exactly this case).
func TestAADecodeToBeParsedRegexFallback(t *testing.T) {
	t.Parallel()

	key := aaTestKey(13)
	plain := `prefix junk {"sourceUrl":"--deadbeef","sourceName":"Luf-Mp4"} trailing`
	sources, err := decodeToBeParsed(aaTestSeal(t, key, plain), key)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != "Luf-Mp4" || sources[0].URL != "deadbeef" {
		t.Fatalf("sources = %+v, want regex-extracted Luf-Mp4", sources)
	}
}

// TestAADecodeHexPairsOracle pins the Python _decrypt port
// (allanime.py:273-283: chr(int(hex,16) ^ 56) per pair) against vectors
// generated by running the frozen Python code.
func TestAADecodeHexPairsOracle(t *testing.T) {
	t.Parallel()

	var oracle struct {
		URLDecodes []struct {
			Encoded string `json:"encoded"`
			Want    string `json:"want"`
		} `json:"url_decodes"`
	}
	if err := json.Unmarshal(fixture(t, "allanime_oracle.json"), &oracle); err != nil {
		t.Fatalf("decode oracle: %v", err)
	}
	if len(oracle.URLDecodes) == 0 {
		t.Fatal("oracle has no url vectors")
	}
	for _, tc := range oracle.URLDecodes {
		got, ok := aaDecodeHexPairs(tc.Encoded)
		if !ok {
			t.Errorf("aaDecodeHexPairs(%q) rejected", tc.Encoded)
			continue
		}
		if got != tc.Want {
			t.Errorf("aaDecodeHexPairs(%q) = %q, want %q", tc.Encoded, got, tc.Want)
		}
	}
}

func TestAADecodeHexPairsRejects(t *testing.T) {
	t.Parallel()

	if _, ok := aaDecodeHexPairs("abc"); ok {
		t.Error("odd-length input accepted")
	}
	if _, ok := aaDecodeHexPairs("zz"); ok {
		t.Error("non-hex pair accepted")
	}
	if _, ok := aaDecodeHexPairs(""); ok {
		t.Error("empty input accepted")
	}
}

// TestAADecodeSourceURL pins the full internal-URL pipeline: trim "--",
// hex-pair decode, "clock" -> "clock.json", absolutize against the
// internal base (allanime.py:196-208 transport updated for the mkissa
// rotation: the internal host is injectable for tests, allanime.day in
// production).
func TestAADecodeSourceURL(t *testing.T) {
	t.Parallel()

	const base = "https://internal.example"

	enc := func(s string) string {
		out := make([]byte, 0, len(s)*2)
		for _, r := range s {
			out = append(out, []byte(fmt.Sprintf("%02x", int(r)^56))...)
		}
		return string(out)
	}

	got, ok := decodeAllAnimeSourceURL("--"+enc("/all/manga/clock?w=1"), base)
	if !ok {
		t.Fatal("valid internal URL rejected")
	}
	if want := base + "/all/manga/clock.json?w=1"; got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	// Absolute external URLs pass through, still clock-rewritten.
	got, ok = decodeAllAnimeSourceURL("--"+enc("https://ext.example/clock?x"), base)
	if !ok || got != "https://ext.example/clock.json?x" {
		t.Fatalf("external url = %q ok=%v", got, ok)
	}

	// Direct .mp4 path: no rewrite of anything but clock.
	got, ok = decodeAllAnimeSourceURL("--"+enc("/getwide/abc.mp4"), base)
	if !ok || got != base+"/getwide/abc.mp4" {
		t.Fatalf("mp4 url = %q ok=%v", got, ok)
	}

	// A "--"-free value is already a plain URL: pass through untouched
	// (documented divergence: Python pushed every value through the hex
	// decoder and crashed the whole resolve on non-hex input).
	got, ok = decodeAllAnimeSourceURL("https://plain.example/x.m3u8", base)
	if !ok || got != "https://plain.example/x.m3u8" {
		t.Fatalf("plain url = %q ok=%v", got, ok)
	}

	// Odd-length hex body after "--": rejected loudly for that source.
	if _, ok := decodeAllAnimeSourceURL("--abc", base); ok {
		t.Fatal("malformed hex body accepted")
	}
}

// aaTestPage builds a referer page embedding epoch, partB and the entry
// bundle URL, mirroring the mkissa.to landing HTML shape.
func aaTestPage(epoch int, partB64, appURL string) string {
	return fmt.Sprintf(`<html><script>window.__SSR__={%q:%d,%q:%q};</script>`+
		`<script src=%q></script></html>`, "epoch", epoch, "partB", partB64, appURL)
}

func TestAAParseRefererPage(t *testing.T) {
	t.Parallel()

	mask := make([]byte, 32)
	for i := range mask {
		mask[i] = byte(i * 3)
	}
	partB := make([]byte, 32)
	for i := range partB {
		partB[i] = byte(200 - i)
	}
	appURL := "https://cdn.example/all/mk/_app/immutable/entry/app.D_sfA4pp.js"
	page := aaTestPage(4130, base64.StdEncoding.EncodeToString(partB), appURL)

	epoch, gotPartB, gotApp, err := aaParseRefererPage(page)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if epoch != "4130" || gotPartB != base64.StdEncoding.EncodeToString(partB) || gotApp != appURL {
		t.Fatalf("parse = (%q, %q, %q)", epoch, gotPartB, gotApp)
	}
}

func TestAAParseRefererPageFailures(t *testing.T) {
	t.Parallel()

	if _, _, _, err := aaParseRefererPage(`<html>{"epoch":41}</html>`); !errors.Is(err, errAAPartBMissing) {
		t.Errorf("missing partB: err = %v, want errAAPartBMissing", err)
	}
	if _, _, _, err := aaParseRefererPage(`<html>{"partB":"AAAA"}</html>`); !errors.Is(err, errAAEpochMissing) {
		t.Errorf("missing epoch: err = %v, want errAAEpochMissing", err)
	}
	noApp := aaTestPage(41, "QUFB", "")
	if _, _, _, err := aaParseRefererPage(noApp); !errors.Is(err, errAAAppMissing) {
		t.Errorf("missing app bundle: err = %v, want errAAAppMissing", err)
	}
}

func TestAADeriveKey(t *testing.T) {
	t.Parallel()

	mask := make([]byte, 32)
	partB := make([]byte, 32)
	for i := range mask {
		mask[i] = byte(i)
		partB[i] = byte(255 - i)
	}
	key, err := aaDeriveKey(hex.EncodeToString(mask), base64.StdEncoding.EncodeToString(partB))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for i := range key {
		if key[i] != mask[i]^partB[i] {
			t.Fatalf("key[%d] = %#x, want mask^partB", i, key[i])
		}
	}

	if _, err := aaDeriveKey("zz", base64.StdEncoding.EncodeToString(partB)); err == nil {
		t.Error("bad mask hex accepted")
	}
	if _, err := aaDeriveKey(hex.EncodeToString(make([]byte, 16)), base64.StdEncoding.EncodeToString(partB)); err == nil {
		t.Error("short mask accepted")
	}
	if _, err := aaDeriveKey(hex.EncodeToString(mask), "QQ=="); err == nil {
		t.Error("short partB accepted")
	}
}

// TestAAFindMask pins the chunk scan: entry imports "../chunks/<name>.js"
// relative to the "/entry/" directory; one of the first five chunks
// embeds the 64-hex mask; fetch failures skip the chunk.
func TestAAFindMask(t *testing.T) {
	t.Parallel()

	mask := hex.EncodeToString(aaTestKey(21))
	entry := `import{a}from"../chunks/one.js";import{b}from"../chunks/two.js";` +
		`import{c}from"../chunks/three.js";import{d}from"../chunks/four.js";` +
		`import{e}from"../chunks/five.js";import{f}from"../chunks/six.js";`
	const cdnRoot = "https://cdn.example/all/mk/_app/immutable"

	chunks := map[string]string{
		cdnRoot + "/chunks/one.js":  `console.log("no hex here")`,
		cdnRoot + "/chunks/two.js":  fmt.Sprintf(`const k=%q;export{k}`, mask),
		cdnRoot + "/chunks/five.js": `export const x = 1;`,
		cdnRoot + "/chunks/six.js":  fmt.Sprintf(`const late=%q;`, hex.EncodeToString(aaTestKey(99))),
	}

	got, err := aaFindMask(entry, cdnRoot, func(url string) (string, bool) {
		body, ok := chunks[url]
		return body, ok
	})
	if err != nil {
		t.Fatalf("find mask: %v", err)
	}
	if got != mask {
		t.Fatalf("mask = %s, want the two.js mask", got)
	}

	// No chunk carries a mask: typed error.
	if _, err := aaFindMask(entry, cdnRoot, func(string) (string, bool) { return "", false }); !errors.Is(err, errAAMaskNotFound) {
		t.Fatalf("no-mask err = %v, want errAAMaskNotFound", err)
	}

	// The scan stops after the first five chunks: a mask living only in
	// chunk six is not found (Goanime semantics).
	sixOnly := fmt.Sprintf(`const m=%q;`, hex.EncodeToString(aaTestKey(88)))
	if _, err := aaFindMask(entry, cdnRoot, func(url string) (string, bool) {
		if strings.HasSuffix(url, "/chunks/six.js") {
			return sixOnly, true
		}
		return "", false
	}); !errors.Is(err, errAAMaskNotFound) {
		t.Fatalf("sixth-chunk mask err = %v, want errAAMaskNotFound", err)
	}
}

// TestAAFetchKeys is the full string-level derivation round trip:
// referer page -> entry bundle -> chunk -> mask XOR partB.
func TestAAFetchKeys(t *testing.T) {
	t.Parallel()

	mask := make([]byte, 32)
	partB := make([]byte, 32)
	for i := range mask {
		mask[i] = byte(90 + i)
		partB[i] = byte(7 * i)
	}
	const cdnRoot = "https://cdn.example/all/mk/_app/immutable"
	bodies := map[string]string{
		"https://ref.example/":       aaTestPage(4130, base64.StdEncoding.EncodeToString(partB), cdnRoot+"/entry/app.x1.js"),
		cdnRoot + "/entry/app.x1.js": `import"../chunks/a.js";`,
		cdnRoot + "/chunks/a.js":     `const m="` + hex.EncodeToString(mask) + `";`,
	}
	keys, err := aaFetchKeys(context.Background(), "https://ref.example/", func(_ context.Context, url string) (string, error) {
		body, ok := bodies[url]
		if !ok {
			return "", fmt.Errorf("no fixture for %s", url)
		}
		return body, nil
	})
	if err != nil {
		t.Fatalf("fetch keys: %v", err)
	}
	if keys.epoch != "4130" {
		t.Errorf("epoch = %q, want 4130", keys.epoch)
	}
	for i := range keys.key {
		if keys.key[i] != mask[i]^partB[i] {
			t.Fatalf("key[%d] = %#x, want mask^partB", i, keys.key[i])
		}
	}
}

// TestAAKeyManagerCachingAndTTL pins: first get fetches, TTL keeps the
// cache, expiry refetches, forced refresh always fetches.
func TestAAKeyManagerCachingAndTTL(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_000_000, 0)
	var calls atomic.Int32
	m := newAllAnimeKeyManager(
		func(context.Context) (*aaKeys, error) {
			n := calls.Add(1)
			return &aaKeys{key: aaTestKey(byte(n & 0xff)), epoch: fmt.Sprint(4000 + n)}, nil
		},
		func() time.Time { return now },
	)

	k1, err := m.get(context.Background())
	if err != nil || calls.Load() != 1 {
		t.Fatalf("first get: err=%v calls=%d", err, calls.Load())
	}
	k2, err := m.get(context.Background())
	if err != nil || calls.Load() != 1 {
		t.Fatalf("cached get refetched: err=%v calls=%d", err, calls.Load())
	}
	if k1 != k2 {
		t.Fatal("cached get returned a different key instance")
	}

	now = now.Add(aaKeysTTL) // exact expiry: not Before(now) anymore
	if _, err := m.get(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("expired get: err=%v calls=%d", err, calls.Load())
	}

	if _, err := m.refresh(context.Background()); err != nil || calls.Load() != 3 {
		t.Fatalf("forced refresh: err=%v calls=%d", err, calls.Load())
	}
}

// TestAAKeyManagerSingleflight pins the dispatch requirement: one
// refresher, waiters reuse the result.
func TestAAKeyManagerSingleflight(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var calls atomic.Int32
	m := newAllAnimeKeyManager(
		func(context.Context) (*aaKeys, error) {
			calls.Add(1)
			<-release
			return &aaKeys{key: aaTestKey(42), epoch: "42"}, nil
		},
		time.Now,
	)

	const waiters = 8
	results := make([]*aaKeys, waiters)
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = m.get(context.Background())
		}()
	}
	close(start)
	time.Sleep(50 * time.Millisecond) // let every waiter pile onto the refresh
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want exactly 1", calls.Load())
	}
	for i := range waiters {
		if errs[i] != nil {
			t.Fatalf("waiter %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("waiter %d got a different key instance", i)
		}
	}
}

// TestAAKeyManagerRefreshOnFailure pins: a failed fetch propagates the
// error and is not cached; the next get retries.
func TestAAKeyManagerRefreshOnFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	fail := true
	m := newAllAnimeKeyManager(
		func(context.Context) (*aaKeys, error) {
			calls.Add(1)
			if fail {
				return nil, errAAMaskNotFound
			}
			return &aaKeys{key: aaTestKey(1), epoch: "1"}, nil
		},
		time.Now,
	)

	if _, err := m.get(context.Background()); !errors.Is(err, errAAMaskNotFound) {
		t.Fatalf("failed fetch err = %v, want errAAMaskNotFound", err)
	}
	fail = false
	if _, err := m.get(context.Background()); err != nil || calls.Load() != 2 {
		t.Fatalf("retry after failure: err=%v calls=%d", err, calls.Load())
	}
}

// TestAAKeyManagerWaiterCancellation pins: a waiter may abandon the
// in-flight refresh via its context; the refresh itself still completes
// and serves the next caller.
func TestAAKeyManagerWaiterCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	m := newAllAnimeKeyManager(
		func(context.Context) (*aaKeys, error) {
			<-release
			return &aaKeys{key: aaTestKey(77), epoch: "77"}, nil
		},
		time.Now,
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if _, err := m.get(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter err = %v, want context.Canceled", err)
	}
	close(release)

	got := make(chan error, 1)
	go func() {
		_, err := m.get(context.Background())
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("post-refresh get: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never completed after cancellation")
	}
}
