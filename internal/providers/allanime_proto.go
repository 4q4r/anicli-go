package providers

// AllAnime v3 client crypto — a pure-Go port of the challenge layer
// embedded in the mkissa.to player bundle, characterized live on
// 2026-09-13 by sandboxing chunk 6_SNhjnz.js inside a real mkissa.to
// page and RE-VERIFIED on 2026-09-17 against chunk DhCxOiZl.js (the
// rotated constants re-captured; the epoch-2958 bootstrap answered
// 200 to a token built from this port). Every constant and formula
// below is [LIVE-VERIFIED 2026-09-17]; the golden vectors live in
// allanime_proto_test.go.
//
// Layers (bottom-up):
//
//	aaMask            cy(buildId) — 32-byte mask table fold
//	aaKeyGroup        gT(host) — referer host → key group
//	aaBootMessage     nT(buildId) — HMAC message #1
//	aaParamString     aT(params) — HMAC message #2
//	aaBootHeader      iT — the x-aa-boot HMAC-SHA256 chain (hex)
//	aaDeriveKeyMaterial ST — lane key = partB XOR mask
//	aaBuildAAReqAt    ET — the aaReq GraphQL proof token
//	aaEpochCandidates bT epoch candidates [mT(), gy()]
//	aaDecryptToBeParsed tobeparsed blob decrypt (legacy → material)
//
// Drift handling: the site rotates the obfuscated chunk (and with it
// the constants) on its own schedule. When the pure-Go tables drift,
// derivation fails loudly (typed errors) and the browser-bridge
// fallback (allanime_bridge.go) re-derives the material from the LIVE
// chunk; the bridge-returned mask bytes let resolution continue even
// under formula drift (see the drift note on aaMask).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/an0nx/anicli-go/internal/crypto"
)

// aaLegacySecret is the pre-rotation tobeparsed key seed (live fT():
// char-code assembly of "Xot36" + "i3lK3"). The legacy key is
// SHA-256(aaLegacySecret + ":v" + blobVersion) and is tried before the
// lane key (dossier/yuzono decrypt order).
const aaLegacySecret = "Xot36i3lK3" //nolint:gosec // public protocol constant from the player bundle, not a credential

// aaCrypto constants live-coded from the chunk's constants table.
// [LIVE-VERIFIED 2026-09-17] (chunk DhCxOiZl.js on cdn.mkissa.net; the
// 2026-09-13 values rotated — formula unchanged, all 56 sandbox inputs
// re-verified byte-for-byte).
const (
	// aaSaltMul/aaSaltAdd fold the per-buildId seed (live saltMul=219,
	// saltAdd=2): seed[l] = charCode(l%len) ^ ((l*saltMul+saltAdd)&255).
	aaSaltMul = 219
	aaSaltAdd = 2
	// aaFragMul/aaFragAdd salt the mask blocks (live fragMul=72,
	// fragAdd=143): mask[l*8+f] ^= ((l*fragMul + f*fragAdd) & 255).
	aaFragMul = 72
	aaFragAdd = 143
	// aaBootPrefix is the fixed HMAC message prefix (live bootPrefix).
	aaBootPrefix = "4Itcfoti4u:" //nolint:gosec // public protocol constant from the player bundle, not a credential
	// aaEpochBucketMs is the epoch bucket width my (live 7 days,
	// re-confirmed by the 2026-09-17 bootstrap epochMs=604800000).
	aaEpochBucketMs = int64(7 * 24 * time.Hour / time.Millisecond)
	// aaEpochGraceMs is the early-bucket grace dT (live 24h, bootstrap
	// graceMs=86400000): inside the first dT of a fresh week the
	// previous epoch is the primary bootstrap candidate.
	aaEpochGraceMs = int64(24 * time.Hour / time.Millisecond)
	// aaReqWindowMillis is the aaReq ts bucket width rv (live 5 min).
	aaReqWindowMillis = int64(5 * time.Minute / time.Millisecond)
)

// aaDMBase64 is the cy() base table dm: four 8-byte blocks the seed is
// XOR-folded into. [LIVE-VERIFIED 2026-09-17] — chunk DhCxOiZl.js
// (sandbox my() re-captured for 56 inputs; the 2026-09-13 table is
// dead: the live bootstrap rejects every token built from it).
var aaDMBase64 = [4]string{
	"6ACQAF2rRcU=",
	"BWfbn4SQ+5U=",
	"5SrrYqEpaUE=",
	"7C7jIdx1zeM=",
}

// aaDMDelayed is aaDMBase64 decoded once at init; a malformed entry is
// a programming error and panics loudly.
var aaDMDelayed [4][8]byte

func init() {
	for i, s := range aaDMBase64 {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != 8 {
			panic(fmt.Sprintf("allanime: bad dm table entry %d: %v", i, err))
		}
		copy(aaDMDelayed[i][:], b)
	}
}

// errAAMaskBuildID reports an unusable buildId (live cy("") returned
// null, surfacing as a TypeError in the browser).
var errAAMaskBuildID = errors.New("allanime: empty build id")

// aaMask ports cy(buildId): the deterministic 32-byte mask folded from
// the dm table, the UTF-16 char codes of the buildId and the salt
// constants:
//
//	seed[l]  = charCodeUTF16(l % len) ^ ((l*saltMul + saltAdd) & 0xff)
//	mask[i]  = (dm[i/8][i%8] ^ seed[i]) ^ (((i/8)*fragMul + (i%8)*fragAdd) & 0xff)
//
// The chunk applies an extra environment-XOR (live envXor=27, was 142)
// when its bot-detection (jk()) fires; in clean browsers jk() is false
// and no XOR applies — the 2026-09-17 server-validated boot token was
// derived from the envXor-clean mask, and this port reproduces it
// byte-for-byte without the XOR. Drift note: if a future chunk rotates
// these tables, aaMask output diverges from the live cy(); the bridge
// fallback returns the live mask bytes and key derivation switches to
// those (see allanime_bridge.go) while the tables here await a re-port.
func aaMask(buildID string) ([]byte, error) {
	if buildID == "" {
		return nil, errAAMaskBuildID
	}
	codes := utf16.Encode([]rune(buildID))
	mask := make([]byte, 32)
	for l := range 32 {
		var code int
		if len(codes) > 0 {
			code = int(codes[l%len(codes)])
		}
		seed := byte(code) ^ byte((l*aaSaltMul+aaSaltAdd)&0xff)
		block, off := l/8, l%8
		mask[l] = aaDMDelayed[block][off] ^ seed ^ byte((block*aaFragMul+off*aaFragAdd)&0xff)
	}
	return mask, nil
}

// aaKeyGroup ports gT(host): the referer-host → key-group mapping used
// inside the x-aa-boot parameters. Hosts are trimmed and www.-stripped;
// the mkissa family and loopback map to "mkissa", RFC1918 192.168.* and
// the mirror domains map to "mirror", everything else defaults to
// "mkissa". [LIVE-VERIFIED 2026-09-13].
func aaKeyGroup(host string) string {
	r := strings.TrimPrefix(strings.TrimSpace(host), "www.")
	switch {
	case r == "":
		return "mkissa"
	case r == "mkissa.to" || r == "api.mkissa.net" || r == "127.0.0.1":
		return "mkissa"
	case strings.HasPrefix(r, "192.168."):
		return "mirror"
	case r == "youtu-chan.com" || r == "isekai2nd.com":
		return "mirror"
	default:
		return "mkissa"
	}
}

// aaBootMessage ports nT(buildId): the first HMAC message.
func aaBootMessage(buildID string) string {
	return aaBootPrefix + buildID
}

// aaBootParams are the x-aa-boot parameter set (iT's input object; the
// lane/buildId/group/host/epoch parts of il().parts).
type aaBootParams struct {
	Lane    string
	BuildID string
	Group   string
	Host    string
	Epoch   int64
}

// aaParamString ports aT(params): the second HMAC message — the five
// parts joined with "+" in the live order
// lane:epoch:host:group:buildId — i.e. "+".join([lane, epoch, host,
// group, buildId]) (live cl.join/cl.parts; empty lane kept:
// omitEmptyLane=false). The 2026-09-13 shape (":" join,
// lane:buildId:group:host:epoch) is dead — the live bootstrap rejects
// every token built with it. [LIVE-VERIFIED 2026-09-17].
func aaParamString(p aaBootParams) string {
	return strings.Join([]string{p.Lane, strconv.FormatInt(p.Epoch, 10), p.Host, p.Group, p.BuildID}, "+")
}

// aaBootHeader ports iT: the x-aa-boot value handed to the bootstrap
// endpoint —
//
//	hex( HMAC-SHA256( key = HMAC-SHA256(key=mask, msg=nT(bid)),
//	                  msg = aT(params) ) )
//
// Both ev() stages are plain HMAC-SHA256 over the raw key bytes and the
// UTF-8 message (verified in-sandbox against crypto.subtle).
// [LIVE-VERIFIED 2026-09-13] — golden vectors in allanime_proto_test.go.
func aaBootHeader(mask []byte, p aaBootParams) (string, error) {
	if len(mask) == 0 {
		return "", errAAMaskBuildID
	}
	first := hmac.New(sha256.New, mask)
	first.Write([]byte(aaBootMessage(p.BuildID)))
	second := hmac.New(sha256.New, first.Sum(nil))
	second.Write([]byte(aaParamString(p)))
	return hex.EncodeToString(second.Sum(nil)), nil
}

// errAAPartBShort reports a partB under 32 bytes (live ST throws
// "invalid_part_b").
var errAAPartBShort = errors.New("allanime: partB shorter than 32 bytes")

// aaDeriveKeyMaterial ports ST(mask, partB): the 32-byte lane key
//
//	key[i] = partB_b64dec[i] ^ mask[i % len(mask)]   for i < 32
//
// partB may exceed 32 bytes (only the first 32 participate).
func aaDeriveKeyMaterial(mask []byte, partB string) ([]byte, error) {
	if len(mask) == 0 {
		return nil, errAAMaskBuildID
	}
	part, err := base64.StdEncoding.DecodeString(partB)
	if err != nil {
		return nil, fmt.Errorf("allanime partB base64: %w", err)
	}
	if len(part) < 32 {
		return nil, errAAPartBShort
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = part[i] ^ mask[i%len(mask)]
	}
	return key, nil
}

// aaReqPayload is the aaReq plaintext; field order doubles as the
// JSON.stringify insertion order of the live builder (v, ts, epoch,
// buildId, qh, k) and must not change — the server binds the token to
// the exact byte string. Epoch and ts marshal as unquoted numbers.
type aaReqPayload struct {
	V       int    `json:"v"`
	TS      int64  `json:"ts"`
	Epoch   int64  `json:"epoch"`
	BuildID string `json:"buildId"`
	QH      string `json:"qh"`
	Lane    string `json:"k"`
}

// aaBuildAAReqAt ports ET: the aaReq proof token, deterministic in
// (qh, key, epoch, buildId, lane, nowMillis):
//
//	ts      = floor(nowMillis / 5min) * 5min
//	payload = JSON of aaReqPayload
//	iv      = SHA-256("epoch:buildId:qh:ts:lane")[:12]
//	blob    = base64( 0x01 || iv || AES-256-GCM(key, iv, payload) )
//
// The IV derivation (IT in the chunk) spans all five fields — the
// pre-rotation Go port used only epoch:qh:ts and is superseded here.
// [LIVE-VERIFIED 2026-09-13].
func aaBuildAAReqAt(qh string, key []byte, epoch int64, buildID, lane string, nowMillis int64) (string, error) {
	ts := nowMillis / aaReqWindowMillis * aaReqWindowMillis
	payload, err := json.Marshal(aaReqPayload{V: 1, TS: ts, Epoch: epoch, BuildID: buildID, QH: qh, Lane: lane})
	if err != nil {
		return "", fmt.Errorf("allanime aaReq payload: %w", err)
	}

	ivSeed := fmt.Sprintf("%d:%s:%s:%d:%s", epoch, buildID, qh, ts, lane)
	ivSum := sha256.Sum256([]byte(ivSeed))
	iv := ivSum[:12]

	blob, err := crypto.GCMSealFrame(key, iv, payload)
	if err != nil {
		return "", fmt.Errorf("allanime aaReq cipher: %w", err)
	}
	return blob, nil
}

// aaEpochCandidates ports the bT bootstrap epoch strategy: candidates
// are [mT(now), gy(now)], deduplicated order-preserving:
//
//	gy = floor(nowMs / 7d)                (the week bucket)
//	mT = gy-1 when nowMs is inside the first 24h of gy and gy > 0
//
// [LIVE-VERIFIED 2026-09-13] (live my=604800000, dT=86400000; the
// captured bootstrap served epoch 2958 with both mT() and gy() equal).
func aaEpochCandidates(nowMillis int64) []int64 {
	gy := nowMillis / aaEpochBucketMs
	if gy > 0 && nowMillis-gy*aaEpochBucketMs < aaEpochGraceMs {
		return []int64{gy - 1, gy}
	}
	return []int64{gy}
}

// aaBlobFrame is the parsed tobeparsed/aaReq blob framing
// 0x01 | iv(12) | ciphertext || tag(16).
type aaBlobFrame struct {
	Version byte
	IV      []byte
	Body    []byte // ciphertext||tag (GCM Open layout)
}

// errAABlobFrame reports a blob that does not match the framing.
var errAABlobFrame = errors.New("allanime: malformed encrypted blob framing")

// aaParseBlobFrame base64-decodes and frames a version-1 blob. The
// base64 alphabets are tried standard-first (the wire format), then the
// URL-safe variants (parity with the previous decoder).
func aaParseBlobFrame(blob string) (aaBlobFrame, error) {
	data, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(blob)
		if err != nil {
			data, err = base64.RawURLEncoding.DecodeString(blob)
			if err != nil {
				return aaBlobFrame{}, fmt.Errorf("allanime blob base64: %w", err)
			}
		}
	}
	if len(data) < 29 || data[0] != 1 {
		return aaBlobFrame{}, errAABlobFrame
	}
	return aaBlobFrame{
		Version: data[0],
		IV:      data[1:13],
		Body:    data[13:],
	}, nil
}

// aaDecryptToBeParsed decrypts a tobeparsed blob with the documented
// key order — legacy first, then the lane material key — and decodes
// the episode sourceUrls. Both the wrapped {"data":{"episode":...}}
// and the current bare {"episode":...} plaintext shapes are accepted.
// A blob failing both keys returns errAAGCMAuth (the caller's
// refresh-and-retry trigger).
func aaDecryptToBeParsed(blob string, materialKey []byte) ([]aaSource, error) {
	frame, err := aaParseBlobFrame(blob)
	if err != nil {
		return nil, err
	}
	legacySum := sha256.Sum256([]byte(aaLegacySecret + ":v" + strconv.Itoa(int(frame.Version))))
	keys := [][]byte{legacySum[:], materialKey}
	var plaintext []byte
	for _, key := range keys {
		p, derr := crypto.GCMDecrypt(frame.Body, frame.IV, key)
		if derr == nil {
			plaintext = p
			break
		}
	}
	if plaintext == nil {
		return nil, fmt.Errorf("%w: neither legacy nor material key authenticated", errAAGCMAuth)
	}
	return aaParseSourcePayload(plaintext)
}

// aaSourceEntry mirrors one sourceUrls entry of the decrypted payload;
// the extra live fields (type/fallBack/stype) drive the direct-media
// decision in ResolveStream.
type aaSourceEntry struct {
	SourceURL  string `json:"sourceUrl"`
	SourceName string `json:"sourceName"`
	Type       string `json:"type"`
	FallBack   string `json:"fallBack"`
	Stype      string `json:"stype"`
}

// aaParseSourcePayload decodes the decrypted episode JSON into
// prioritized sources. The payload may be bare ({episode:{...}}, the
// live shape) or data-wrapped ({data:{episode:{...}}}, the pre-rotation
// shape); a non-JSON body falls back to the sourceUrl/sourceName regex
// scrapers (upstream tolerance for polluted payloads).
func aaParseSourcePayload(plaintext []byte) ([]aaSource, error) {
	var parsed struct {
		Episode *struct {
			SourceUrls []aaSourceEntry `json:"sourceUrls"`
		} `json:"episode"`
		Data *struct {
			Episode *struct {
				SourceUrls []aaSourceEntry `json:"sourceUrls"`
			} `json:"episode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(plaintext, &parsed); err == nil {
		entries := parsed.Episode
		if entries == nil && parsed.Data != nil {
			entries = parsed.Data.Episode
		}
		if entries != nil && len(entries.SourceUrls) > 0 {
			sources := make([]aaSource, 0, len(entries.SourceUrls))
			for _, su := range entries.SourceUrls {
				sources = append(sources, aaSource{
					Name:     su.SourceName,
					URL:      strings.TrimPrefix(su.SourceURL, "--"),
					Type:     su.Type,
					FallBack: su.FallBack,
				})
			}
			return sources, nil
		}
	}

	// Regex fallback for payloads that are not clean JSON.
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
