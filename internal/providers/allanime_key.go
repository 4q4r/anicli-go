package providers

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/an0nx/anicli-go/internal/crypto"
)

// This file implements the AllAnime per-epoch client crypto required
// since the 2026-07-22 rotation (ani-cli PR #1779, issues #1677/#1772):
// there is no static key anymore. The key is derived at runtime as
// mask XOR partB — the mkissa.to referer page embeds "epoch" and
// base64(32-byte) "partB" plus the entry-bundle URL; one of the first
// code-split chunks the bundle imports embeds a 64-hex "mask". The same
// 32-byte key both signs the aaReq GraphQL token and decrypts the
// "tobeparsed" response blob (AES-256-GCM on both sides).
//
// Reference implementation (semantics, not code): alvarorichard/Goanime
// v1.8.6 internal/scraper/providers/allanime {keys.go, crypto.go},
// fetched via pkg.go.dev/GitHub for this port. Live behaviour is
// [UNVERIFIED] until the parity pass; every assumption is listed in the
// PR6 report.

// aaKeys is the per-epoch secret material: a 32-byte AES-256 key plus
// the numeric epoch string. Both are bound into the aaReq token; the
// key also decrypts the tobeparsed blob.
type aaKeys struct {
	key   []byte
	epoch string
}

// aaKeysTTL bounds how long a derived key is reused before a refetch.
// The server epoch rotates on its own schedule; three minutes keeps
// tokens valid while avoiding a bundle re-scrape per episode
// (Goanime aaKeysTTL).
const aaKeysTTL = 3 * time.Minute

// aaFetchBudget bounds a detached key refresh so an abandoned caller
// cannot leave a fetch running forever; the netclient's own timeouts
// are the inner bound.
const aaFetchBudget = 2 * time.Minute

// Typed derivation failures. Callers classify with errors.Is; the
// provider wraps them in ProviderError alongside ErrExtractFailed.
var (
	errAAEpochMissing = errors.New("allanime: epoch not found on referer page")
	errAAPartBMissing = errors.New("allanime: partB not found on referer page")
	errAAAppMissing   = errors.New("allanime: entry bundle URL not found on referer page")
	errAAMaskNotFound = errors.New("allanime: mask not found in entry bundle chunks")
	// errAAGCMAuth marks a tobeparsed blob that failed GCM
	// authentication (wrong key — the refresh-and-retry trigger).
	errAAGCMAuth = errors.New("allanime: tobeparsed gcm authentication failed")
	// errAADecryptFailed marks a tobeparsed blob that still fails GCM
	// authentication after one forced key refresh.
	errAADecryptFailed = errors.New("allanime: tobeparsed decrypt failed after key refresh")
)

// Scrapers for the key-derivation flow (Goanime keys.go regexes, the
// epoch variant made whitespace-tolerant).
var (
	aaEpochRe = regexp.MustCompile(`"epoch":\s*(\d+)`)
	aaPartBRe = regexp.MustCompile(`"partB":\s*"([^"]*)"`)
	aaAppRe   = regexp.MustCompile(`https?://[^"'\s]+/entry/app\.[A-Za-z0-9_.-]+\.js`)
	aaChunkRe = regexp.MustCompile(`"\.\./chunks/[A-Za-z0-9_.-]+\.js"`)
	aaMaskRe  = regexp.MustCompile(`[0-9a-f]{64}`)
)

// aaReqWindowMillis is the clock-bucket width the aaReq timestamp is
// rounded to; it must match the server window (5 minutes) or the token
// is rejected.
const aaReqWindowMillis int64 = 300_000

// aaBuildAAReqAt builds the "aaReq" proof token for the episode-sources
// query. Deterministic in (qh, key, epoch, nowMillis):
//
//	base64( 0x01 || iv(12) || AES-256-GCM(key, iv, payload) )
//	payload = {"v":1,"ts":<bucketed ms>,"epoch":<unquoted>,"qh":"<qh>"}
//	iv      = SHA-256("epoch:qh:ts")[:12]
//
// Its absence makes the API answer AA_CRYPTO_MISSING (ani-cli #1772).
// The timestamp floors to the 5-minute window, so every call inside one
// window yields the same token.
func aaBuildAAReqAt(qh string, key []byte, epoch string, nowMillis int64) (string, error) {
	ts := nowMillis / aaReqWindowMillis * aaReqWindowMillis
	payload := fmt.Sprintf(`{"v":1,"ts":%d,"epoch":%s,"qh":%q}`, ts, epoch, qh)

	ivHash := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", epoch, qh, ts)))
	iv := ivHash[:12]

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("allanime aaReq cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("allanime aaReq gcm: %w", err)
	}
	// Seal returns ciphertext with the 16-byte tag appended, matching
	// the wire layout ct||tag.
	sealed := gcm.Seal(nil, iv, []byte(payload), nil)

	out := make([]byte, 0, 1+len(iv)+len(sealed))
	out = append(out, 0x01)
	out = append(out, iv...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// aaSource is one decoded episode source: provider name plus the still
// "--"-prefixed encoded URL.
type aaSource struct {
	Name string
	URL  string
}

// Scrapers for the decrypted tobeparsed payload when it is not clean
// JSON (Goanime keeps a sed-style fallback; order matters).
var (
	aaSourceURLNameRe = regexp.MustCompile(`"sourceUrl"\s*:\s*"--([^"]*)"[^}]*"sourceName"\s*:\s*"([^"]*)"`)
	aaSourceNameURLRe = regexp.MustCompile(`"sourceName"\s*:\s*"([^"]*)"[^}]*"sourceUrl"\s*:\s*"--([^"]*)"`)
	aaToBeParsedRe    = regexp.MustCompile(`"tobeparsed"\s*:\s*"([^"]*)"`)
)

// aaExtractToBeParsedBlob pulls the base64 tobeparsed value out of a
// raw API response body, "" when absent.
func aaExtractToBeParsedBlob(response []byte) string {
	if m := aaToBeParsedRe.FindSubmatch(response); m != nil {
		return string(m[1])
	}
	return ""
}

// decodeToBeParsed decrypts the tobeparsed blob:
//
//	base64( 0x01 || nonce(12) || ciphertext || tag(16) )
//
// (restored AES-256-GCM layout, 2026-07-08; minimum 29 bytes). Standard
// base64 is tried first, then the URL-safe alphabets. The decrypted
// payload is the episode GraphQL response carrying sourceUrls; a
// regex fallback covers payloads that are not clean JSON. Uses the
// shared internal/crypto GCM helper.
func decodeToBeParsed(blob string, key []byte) ([]aaSource, error) {
	data, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(blob)
		if err != nil {
			data, err = base64.RawURLEncoding.DecodeString(blob)
			if err != nil {
				return nil, fmt.Errorf("allanime tobeparsed base64: %w", err)
			}
		}
	}
	if len(data) < 29 {
		return nil, fmt.Errorf("allanime tobeparsed blob too short: %d bytes", len(data))
	}

	// Go's GCM Open expects the tag appended to the ciphertext, which
	// is exactly the wire layout after the version byte.
	plaintext, err := crypto.GCMDecrypt(data[13:], data[1:13], key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAAGCMAuth, err)
	}

	var parsed struct {
		Data struct {
			Episode struct {
				SourceUrls []struct {
					SourceURL  string `json:"sourceUrl"`
					SourceName string `json:"sourceName"`
				} `json:"sourceUrls"`
			} `json:"episode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(plaintext, &parsed); err == nil && len(parsed.Data.Episode.SourceUrls) > 0 {
		sources := make([]aaSource, 0, len(parsed.Data.Episode.SourceUrls))
		for _, su := range parsed.Data.Episode.SourceUrls {
			sources = append(sources, aaSource{
				Name: su.SourceName,
				URL:  strings.TrimPrefix(su.SourceURL, "--"),
			})
		}
		return sources, nil
	}

	for _, re := range []*regexp.Regexp{aaSourceURLNameRe, aaSourceNameURLRe} {
		matches := re.FindAllSubmatch(plaintext, -1)
		if len(matches) == 0 {
			continue
		}
		urlIdx, nameIdx := 1, 2
		if re == aaSourceNameURLRe {
			urlIdx, nameIdx = 2, 1
		}
		sources := make([]aaSource, 0, len(matches))
		for _, m := range matches {
			sources = append(sources, aaSource{Name: string(m[nameIdx]), URL: string(m[urlIdx])})
		}
		return sources, nil
	}
	return nil, errors.New("allanime tobeparsed: no source urls in decrypted payload")
}

// aaDecodeHexPairs ports the Python "--" URL decoder (allanime.py
// _decrypt): every hex pair maps to chr(int(pair, 16) ^ 56). Python's
// oct()/int(_, 8) round-trip is a no-op. Divergence: Python built a
// Unicode string (chr of 0x80..0xFF becomes two UTF-8 bytes); real URL
// alphabets are ASCII, so the raw byte is kept. ok=false on odd length
// or non-hex input (Python raised and killed the whole resolve).
func aaDecodeHexPairs(raw string) (string, bool) {
	if len(raw) == 0 || len(raw)%2 != 0 {
		return "", false
	}
	out := make([]byte, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		pair, err := hex.DecodeString(raw[i : i+2])
		if err != nil || len(pair) != 1 {
			return "", false
		}
		out = append(out, pair[0]^56)
	}
	return string(out), true
}

// decodeAllAnimeSourceURL turns one sourceUrls entry into a fetchable
// URL (port of allanime.py:196-208, transport updated for the mkissa
// rotation): hex-pair decode, rewrite "clock" to "clock.json", then
// absolutize — http(s) URLs pass through, "/..." is prefixed with the
// internal base (allanime.day in production, injectable for tests).
//
// The "--" marker may already be stripped by decodeToBeParsed, so the
// encoded body is detected structurally: a "--"-marked value must
// decode or the source is rejected; an unmarked value that is entirely
// hex pairs decodes too; anything else is a plain URL passed through
// verbatim (documented divergence: Python pushed every value through
// the hex decoder and crashed the whole resolve on non-hex input).
// A decoded relative path without a leading slash gains the missing
// "/" (Python concatenated a malformed URL that could only 404).
func decodeAllAnimeSourceURL(src, internalBase string) (string, bool) {
	body := strings.TrimPrefix(src, "--")
	plain := body
	if strings.HasPrefix(src, "--") || aaLooksHexPairs(body) {
		decoded, ok := aaDecodeHexPairs(body)
		if !ok {
			return "", false
		}
		plain = decoded
	}
	// Python str.replace("clock", "clock.json") on the whole decoded
	// URL — bug-compatible including a "clock" inside a query value.
	plain = strings.ReplaceAll(plain, "clock", "clock.json")

	switch {
	case strings.HasPrefix(plain, "http"):
		return plain, true
	case strings.HasPrefix(plain, "/"):
		return internalBase + plain, true
	case plain == "":
		return "", false
	default:
		return internalBase + "/" + plain, true
	}
}

// aaLooksHexPairs reports whether s is a non-empty even-length run of
// hex digits (a candidate encoded body).
func aaLooksHexPairs(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, r := range s {
		hexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !hexDigit {
			return false
		}
	}
	return true
}

// aaParseRefererPage scrapes epoch, partB (still base64) and the entry
// bundle URL off the mkissa.to landing HTML.
func aaParseRefererPage(page string) (epoch, partB, appURL string, err error) {
	if m := aaPartBRe.FindStringSubmatch(page); m != nil {
		partB = m[1]
	}
	if partB == "" {
		return "", "", "", errAAPartBMissing
	}
	if m := aaEpochRe.FindStringSubmatch(page); m != nil {
		epoch = m[1]
	}
	if epoch == "" {
		return "", "", "", errAAEpochMissing
	}
	appURL = aaAppRe.FindString(page)
	if appURL == "" {
		return "", "", "", errAAAppMissing
	}
	return epoch, partB, appURL, nil
}

// aaDeriveKey computes key = mask XOR partB, byte for byte, both 32
// bytes (mask 64 hex chars, partB standard base64).
func aaDeriveKey(maskHex, partB string) ([]byte, error) {
	mask, err := hex.DecodeString(maskHex)
	if err != nil {
		return nil, fmt.Errorf("allanime mask hex: %w", err)
	}
	if len(mask) != 32 {
		return nil, fmt.Errorf("allanime mask: %d bytes, want 32", len(mask))
	}
	partBBytes, err := base64.StdEncoding.DecodeString(partB)
	if err != nil {
		return nil, fmt.Errorf("allanime partB base64: %w", err)
	}
	if len(partBBytes) != 32 {
		return nil, fmt.Errorf("allanime partB: %d bytes, want 32", len(partBBytes))
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = mask[i] ^ partBBytes[i]
	}
	return key, nil
}

// aaFindMask scans the code-split chunks the entry bundle imports
// ("../chunks/<name>.js" relative to the "/entry/" directory) for a
// 64-hex mask. Only the first five chunks are inspected (Goanime
// semantics); chunks that fail to fetch are skipped.
func aaFindMask(entryJS, cdnRoot string, fetchChunk func(url string) (string, bool)) (string, error) {
	for i, m := range aaChunkRe.FindAllString(entryJS, -1) {
		if i >= 5 {
			break
		}
		chunk := strings.TrimPrefix(strings.Trim(m, `"`), "../")
		body, ok := fetchChunk(cdnRoot + "/" + chunk)
		if !ok {
			continue
		}
		if hit := aaMaskRe.FindString(body); hit != "" {
			return hit, nil
		}
	}
	return "", errAAMaskNotFound
}

// aaFetchKeys runs the whole derivation: referer page -> entry bundle
// -> first chunks -> mask XOR partB. fetchText abstracts HTTP so tests
// inject fixtures.
func aaFetchKeys(ctx context.Context, refererURL string, fetchText func(ctx context.Context, url string) (string, error)) (*aaKeys, error) {
	page, err := fetchText(ctx, refererURL)
	if err != nil {
		return nil, fmt.Errorf("allanime keys: fetch referer page: %w", err)
	}
	epoch, partB, appURL, err := aaParseRefererPage(page)
	if err != nil {
		return nil, err
	}

	appJS, err := fetchText(ctx, appURL)
	if err != nil {
		return nil, fmt.Errorf("allanime keys: fetch entry bundle: %w", err)
	}

	// Chunks are "../chunks/<name>.js" relative to the "/entry/"
	// directory, so the CDN root is everything before "/entry/". It is
	// derived from the page instead of a pinned host so a CDN rotation
	// cannot strand the derivation.
	cdnRoot, _, ok := strings.Cut(appURL, "/entry/")
	if !ok {
		return nil, fmt.Errorf("allanime keys: bundle URL missing /entry/ segment: %s", appURL)
	}

	maskHex, err := aaFindMask(appJS, cdnRoot, func(url string) (string, bool) {
		body, err := fetchText(ctx, url)
		if err != nil {
			return "", false
		}
		return body, true
	})
	if err != nil {
		return nil, err
	}

	key, err := aaDeriveKey(maskHex, partB)
	if err != nil {
		return nil, err
	}
	return &aaKeys{key: key, epoch: epoch}, nil
}

// allAnimeKeyManager caches the per-epoch key with singleflight-style
// refresh: one refresher under a mutex, waiters reuse its result via the
// closed done channel. A waiter may abandon the refresh through its own
// context; the refresh itself runs detached on a bounded context so it
// still completes for the remaining waiters. A failed refresh is not
// cached — the next get retries.
type allAnimeKeyManager struct {
	mu       sync.Mutex
	keys     *aaKeys
	expiry   time.Time
	fetching bool
	done     chan struct{}
	err      error

	fetch func(ctx context.Context) (*aaKeys, error)
	now   func() time.Time
}

// newAllAnimeKeyManager wires the derivation (fetch) and clock (now).
func newAllAnimeKeyManager(fetch func(ctx context.Context) (*aaKeys, error), now func() time.Time) *allAnimeKeyManager {
	return &allAnimeKeyManager{fetch: fetch, now: now}
}

// get returns the cached key while the TTL holds, otherwise refreshes.
func (m *allAnimeKeyManager) get(ctx context.Context) (*aaKeys, error) {
	m.mu.Lock()
	if m.keys != nil && m.now().Before(m.expiry) {
		keys := m.keys
		m.mu.Unlock()
		return keys, nil
	}
	ch := m.beginRefreshLocked()
	m.mu.Unlock()
	return m.await(ctx, ch)
}

// refresh forces a fresh derivation even when the cache is still valid
// (the decrypt-failure path: the server epoch rotated under us). A
// refresh already in flight is joined rather than duplicated.
func (m *allAnimeKeyManager) refresh(ctx context.Context) (*aaKeys, error) {
	m.mu.Lock()
	ch := m.beginRefreshLocked()
	m.mu.Unlock()
	return m.await(ctx, ch)
}

// beginRefreshLocked marks a refresh in flight (returning its done
// channel) or returns the existing one's channel. Caller must hold mu.
func (m *allAnimeKeyManager) beginRefreshLocked() chan struct{} {
	if m.fetching {
		return m.done
	}
	m.fetching = true
	m.done = make(chan struct{})
	go m.runRefresh(m.done)
	return m.done
}

// runRefresh executes the detached derivation and publishes the result
// to every waiter before closing done.
func (m *allAnimeKeyManager) runRefresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), aaFetchBudget)
	defer cancel()

	keys, err := m.fetch(ctx)

	m.mu.Lock()
	if err != nil {
		m.err = err
	} else {
		m.keys = keys
		m.expiry = m.now().Add(aaKeysTTL)
		m.err = nil
	}
	m.fetching = false
	close(done)
	m.mu.Unlock()
}

// await blocks until the refresh backing done settles (or ctx ends) and
// returns its published result.
func (m *allAnimeKeyManager) await(ctx context.Context, done chan struct{}) (*aaKeys, error) {
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
	if m.keys == nil {
		return nil, errors.New("allanime keys: refresh settled without a result")
	}
	return m.keys, nil
}
