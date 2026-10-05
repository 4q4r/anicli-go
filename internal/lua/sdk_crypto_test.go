package lua

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// The PR127 crypto SDK: anicli.crypto (AES-256-CBC decrypt with PKCS7,
// HMAC-SHA256) and the base64 URL-safe variants. The megaplay player
// chain (anikoto) needs exactly these — implementing them in pure Lua
// would drag AES and SHA-256 tables into a sandbox with no bit
// library, so the primitives surface from the Go stdlib instead. The
// SDK stays primitive-honest: key/IV material arrives as RAW bytes
// (zero-padding policy belongs to the caller), the ciphertext leaves
// PKCS7-stripped and validated.

// aesCBCEncrypt is the test-side encryptor (the SDK exposes decrypt
// only — providers never encrypt).
func aesCBCEncrypt(t *testing.T, key, iv, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	pad := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+pad)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(pad)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	return string(ct)
}

// luaBytesLiteral renders s as a Lua string literal (Lua 5.1 has no
// \xNN escapes — binary bytes go out as \ddd decimals).
func luaBytesLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\%d`, c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestSDKCryptoAESCBDecrypt(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	key := "0123456789abcdef0123456789abcdef" // raw 32 bytes
	iv := "fedcba9876543210"                  // raw 16 bytes
	ct := aesCBCEncrypt(t, []byte(key), []byte(iv), []byte(`{"file":"https://cdn.example/master.m3u8"}`))

	got, err := evalSDK(t, e, fmt.Sprintf(
		`return anicli.crypto.aes_cbc_decrypt(%q, %q, %s)`, key, iv, luaBytesLiteral(ct)))
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"file":"https://cdn.example/master.m3u8"}` {
		t.Fatalf("aes_cbc_decrypt = %q", got)
	}
}

func TestSDKCryptoAESCBDecryptRejectsBadPadding(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	key := "0123456789abcdef0123456789abcdef"
	iv := "fedcba9876543210"

	// A ciphertext whose final block carries no valid PKCS7 tail must
	// raise, never return padded garbage.
	ct := aesCBCEncrypt(t, []byte(key), []byte(iv), []byte("sixteen-byte msg"))
	truncated := ct[:len(ct)-1] + "\x00"
	if _, err := evalSDK(t, e, fmt.Sprintf(
		`local ok = pcall(anicli.crypto.aes_cbc_decrypt, %q, %q, %s)
		 if ok then return "UNDETECTED" end
		 return "raised"`, key, iv, luaBytesLiteral(truncated))); err != nil {
		t.Fatal(err)
	}
	// Corrupt one padding byte instead of truncating: same verdict.
	bad := []byte(ct)
	bad[len(bad)-1] ^= 0xff
	got, err := evalSDK(t, e, fmt.Sprintf(
		`local ok, res = pcall(anicli.crypto.aes_cbc_decrypt, %q, %q, %s)
		 if ok then return "UNDETECTED: " .. res end
		 return "raised: " .. tostring(res)`, key, iv, luaBytesLiteral(string(bad))))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "raised") || strings.Contains(got, "UNDETECTED") {
		t.Fatalf("bad padding verdict = %q, want a raise", got)
	}
	if !strings.Contains(got, "padding") {
		t.Fatalf("raise = %q, want the PKCS7 wall named", got)
	}
}

func TestSDKCryptoHMACSHA256(t *testing.T) {
	t.Parallel()

	// The well-known "key"/fox vector, pinned against the Go primitive
	// the SDK wraps.
	const secret, message = "key", "The quick brown fox jumps over the lazy dog"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	want := string(mac.Sum(nil))

	e, _, _ := newSDKEngine(t, nil)
	got, err := evalSDK(t, e, fmt.Sprintf(
		`return anicli.crypto.hmac_sha256(%q, %q)`, secret, message))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hmac_sha256 = %x, want %x", got, want)
	}

	// The anikoto signing shape: the digest over "{expires}|{h1}/{h2}".
	msg := "1758800090|577bcc914f9e55d5e4e4f82f9f00e7d4/0c06a5a3738e876160b5db4319d5d12a"
	mac2 := hmac.New(sha256.New, []byte("s3cret"))
	mac2.Write([]byte(msg))
	got2, err := evalSDK(t, e, fmt.Sprintf(
		`return anicli.crypto.hmac_sha256("s3cret", %q)`, msg))
	if err != nil {
		t.Fatal(err)
	}
	if got2 != string(mac2.Sum(nil)) {
		t.Fatalf("hmac_sha256 message digest mismatch: %x", got2)
	}
}

func TestSDKBase64URLRoundTrip(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)

	// RFC 4648 test vectors, URL-safe alphabet, no padding. The padded
	// input case ("Zm9vYmE=" = "fooba") proves padding tolerance.
	got, err := evalSDK(t, e, `
		local a = anicli.base64.url_encode("foobar")
		local b = anicli.base64.url_encode("")
		local c = anicli.base64.url_decode("Zm9vYmFy")
		local d = anicli.base64.url_decode("Zm9vYmE=") -- padded input tolerated
		return a .. "|" .. b .. "|" .. c .. "|" .. d
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Zm9vYmFy||foobar|fooba" {
		t.Fatalf("base64url round trip = %q", got)
	}

	// Bytes that differ between the std and URL alphabets ride
	// verbatim through the URL-safe codec.
	bin := string([]byte{0xfb, 0xef, 0xbe})
	enc, err := evalSDK(t, e, fmt.Sprintf(`return anicli.base64.url_encode(%s)`, luaBytesLiteral(bin)))
	if err != nil {
		t.Fatal(err)
	}
	want := base64.RawURLEncoding.EncodeToString([]byte(bin))
	if enc != want {
		t.Fatalf("url_encode(%x) = %q, want %q", bin, enc, want)
	}
	if strings.ContainsAny(enc, "+/") {
		t.Fatalf("url_encode emitted std-alphabet bytes: %q", enc)
	}
}

func TestSDKBase64URLDecodeRejectsGarbage(t *testing.T) {
	t.Parallel()

	e, _, _ := newSDKEngine(t, nil)
	_, err := evalSDK(t, e, `return anicli.base64.url_decode("not base64!!")`)
	if err == nil {
		t.Fatal("garbage decode passed, want a raise")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Fatalf("raise = %v, want the codec named", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte("url")) {
		t.Fatalf("raise = %v, want the url variant named", err)
	}
}
