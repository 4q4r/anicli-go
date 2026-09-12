package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Token machinery of the API auth face, ported from anicli-py
// api_server.py:969-1125 — the exact wire formats are contract:
//
//   - password hashes: pbkdf2_sha256$<iterations>$<salt_b64url_nopad>$<digest_b64url_nopad>
//     (interoperable with anicli-py settings.toml hashes);
//   - access tokens: JWT-shaped HS256 HMAC-SHA256 with unpadded base64url
//     segments and claims {sub, sid, iat, exp, typ:"access"};
//   - refresh tokens: 48 random bytes, unpadded base64url (64 chars),
//     stored only as their sha256 hex digest.

// errToken is the internal decode failure; callers map it to the
// unauthorized API error contract.
var errToken = errors.New("invalid access token")

// refreshTokenBytes matches python secrets.token_urlsafe(48).
const refreshTokenBytes = 48

// base64URLEncode encodes without padding (python _b64url_encode).
func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// base64URLDecode restores implicit padding then decodes
// (python _b64url_decode).
func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// VerifyPasswordHash verifies password against a
// pbkdf2_sha256$<iterations>$<salt_b64url>$<digest_b64url> hash with
// constant-time comparison (port of api_server.py _verify_user_password).
// Malformed hashes simply fail.
func VerifyPasswordHash(password, passwordHash string) bool {
	parts := strings.Split(passwordHash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	salt, err := base64URLDecode(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64URLDecode(parts[3])
	if err != nil || len(expected) == 0 {
		return false
	}
	computed := pbkdf2SHA256([]byte(password), salt, iterations, len(expected))
	return hmac.Equal(computed, expected)
}

// pbkdf2SHA256 derives a key (RFC 2898, HMAC-SHA256) — std crypto only
// per the dependency constraint (golang.org/x/crypto/pbkdf2 exists but
// stays out of the direct surface; the iteration count comes from the
// stored hash, never from the caller).
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	u := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		_, _ = prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		_, _ = prf.Write(buf)
		u = u[:hashLen]
		sum := prf.Sum(nil)
		copy(u, sum)
		t := make([]byte, hashLen)
		copy(t, u)
		for i := 1; i < iterations; i++ {
			prf.Reset()
			_, _ = prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// accessClaims is the decoded access-token payload.
type accessClaims struct {
	Subject   string `json:"sub"`
	SessionID string `json:"sid"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	TokenType string `json:"typ"`
}

// accessHeader is the fixed JWT-like header segment.
type accessHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

// IssueAccessToken builds the signed access token for one session
// (python _issue_access_token): HS256, typ "access", TTL from config.
func IssueAccessToken(secret []byte, login, sessionID string, ttl time.Duration, now time.Time) string {
	header, err := json.Marshal(accessHeader{Algorithm: "HS256", Type: "JWT"})
	if err != nil {
		// Static struct marshal cannot fail; refuse to sign on the
		// impossible path instead of panicking mid-request.
		return ""
	}
	claims, err := json.Marshal(accessClaims{
		Subject:   login,
		SessionID: sessionID,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		TokenType: "access",
	})
	if err != nil {
		return ""
	}

	encodedHeader := base64URLEncode(header)
	encodedClaims := base64URLEncode(claims)
	signingInput := encodedHeader + "." + encodedClaims
	signature := signToken(secret, signingInput)
	return signingInput + "." + base64URLEncode(signature)
}

// DecodeAccessToken verifies the signature and expiry and returns the
// claims (python _decode_access_token). Every failure is errToken —
// callers surface it as the unauthorized error contract.
func DecodeAccessToken(secret []byte, token string, now time.Time) (*accessClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errToken
	}
	expected := signToken(secret, parts[0]+"."+parts[1])
	provided, err := base64URLDecode(parts[2])
	if err != nil || !hmac.Equal(expected, provided) {
		return nil, errToken
	}
	raw, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, errToken
	}
	var claims accessClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, errToken
	}
	// Python: exp must be an int strictly in the future (exp <= now fails).
	if claims.ExpiresAt <= now.Unix() {
		return nil, errToken
	}
	return &claims, nil
}

// signToken computes the HS256 HMAC over the signing input.
func signToken(secret []byte, signingInput string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

// NewRefreshToken returns a fresh high-entropy refresh token
// (python secrets.token_urlsafe(48) — 64 unpadded url-safe chars).
func NewRefreshToken() (string, error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	return base64URLEncode(buf), nil
}

// HashRefreshToken returns the deterministic sha256 hex digest under
// which refresh tokens are persisted (never the token itself).
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
