package lua

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// The PR127 crypto SDK: the primitives the megaplay player chain
// (anikoto) needs, surfaced from the Go stdlib. A pure-Lua AES/SHA-256
// would drag cipher tables into a sandbox with no bit library, so the
// primitives ride the host instead. The surface stays primitive-honest:
//
//   - crypto.aes_cbc_decrypt(key, iv, data) takes RAW key/IV bytes
//     (zero-padding policy belongs to the caller) and returns the
//     plaintext with the PKCS7 padding stripped and validated — a bad
//     padding raises, it never returns padded garbage;
//   - crypto.hmac_sha256(secret, message) returns the raw 32-byte
//     digest (encoding stays the caller's business);
//   - base64.url_encode / base64.url_decode are the unpadded
//     URL-safe codec (url_decode tolerates padded input).

// openSDKCrypto registers the anicli.crypto module and the base64 URL
// variants onto the SDK module table.
func openSDKCrypto(ls *lua.LState, mod *lua.LTable) {
	cryptoTbl := ls.NewTable()
	ls.SetFuncs(cryptoTbl, map[string]lua.LGFunction{
		"aes_cbc_decrypt": sdkAESCBCDecrypt,
		"hmac_sha256":     sdkHMACSHA256,
	})
	mod.RawSetString("crypto", cryptoTbl)

	b64 := mod.RawGetH(lua.LString("base64")).(*lua.LTable)
	b64.RawSetString("url_encode", ls.NewFunction(sdkBase64URLEncode))
	b64.RawSetString("url_decode", ls.NewFunction(sdkBase64URLDecode))
}

// sdkAESCBCDecrypt implements anicli.crypto.aes_cbc_decrypt(key, iv,
// data): one AES-CBC block-mode decryption over data (which must be a
// block multiple) plus the strict PKCS7 strip. Key material must be
// the exact AES-key-size bytes (16/24/32); the IV exactly one block.
func sdkAESCBCDecrypt(ls *lua.LState) int {
	key := ls.CheckString(1)
	iv := ls.CheckString(2)
	data := ls.CheckString(3)

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		ls.RaiseError("crypto.aes_cbc_decrypt: key: %v", err)
		return 0
	}
	if len(iv) != aes.BlockSize {
		ls.RaiseError("crypto.aes_cbc_decrypt: iv is %d bytes, want %d", len(iv), aes.BlockSize)
		return 0
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		ls.RaiseError("crypto.aes_cbc_decrypt: ciphertext length %d is not a block multiple", len(data))
		return 0
	}

	pt := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, []byte(iv)).CryptBlocks(pt, []byte(data))

	n := int(pt[len(pt)-1])
	if n == 0 || n > aes.BlockSize || n > len(pt) {
		ls.RaiseError("crypto.aes_cbc_decrypt: invalid PKCS7 padding")
		return 0
	}
	for _, b := range pt[len(pt)-n:] {
		if int(b) != n {
			ls.RaiseError("crypto.aes_cbc_decrypt: invalid PKCS7 padding bytes")
			return 0
		}
	}
	ls.Push(lua.LString(pt[:len(pt)-n]))
	return 1
}

// sdkHMACSHA256 implements anicli.crypto.hmac_sha256(secret, message):
// the raw HMAC-SHA256 digest bytes as a Lua string.
func sdkHMACSHA256(ls *lua.LState) int {
	secret := ls.CheckString(1)
	message := ls.CheckString(2)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	ls.Push(lua.LString(mac.Sum(nil)))
	return 1
}

// sdkBase64URLEncode implements anicli.base64.url_encode: the unpadded
// URL-safe alphabet (the CDN token shape).
func sdkBase64URLEncode(ls *lua.LState) int {
	ls.Push(lua.LString(base64.RawURLEncoding.EncodeToString([]byte(ls.CheckString(1)))))
	return 1
}

// sdkBase64URLDecode implements anicli.base64.url_decode: the URL-safe
// alphabet, padding tolerated (the megaplay enc blob arrives padded,
// the tokens unpadded).
func sdkBase64URLDecode(ls *lua.LState) int {
	in := strings.TrimRight(ls.CheckString(1), "=")
	decoded, err := base64.RawURLEncoding.DecodeString(in)
	if err != nil {
		ls.RaiseError("base64.url_decode: %v", err)
		return 0
	}
	ls.Push(lua.LString(decoded))
	return 1
}
