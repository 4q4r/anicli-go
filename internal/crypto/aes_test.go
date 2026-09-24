package crypto

import (
	"bytes"
	"errors"
	"testing"
)

// Golden vectors generated from the Python original
// anicli-py/anicli/core/crypto.py (aes_encrypt L84-99, aes_decrypt L102-119)
// via `uv run --with pycryptodome python` with this snippet (logic verbatim):
//
//	from Crypto.Cipher import AES
//	from Crypto.Util.Padding import pad
//	def aes_encrypt(data, key, iv):
//	    cipher = AES.new(key, AES.MODE_CBC, iv=iv)
//	    padded = pad(data.encode("utf-8"), AES.block_size)
//	    return base64.b64encode(cipher.encrypt(padded)).decode("utf-8")
//
// All cases share iv = b"ABCDEFGHIJKLMNOP".
var aesGoldens = []struct {
	name   string
	key    []byte
	plain  string
	cipher string
}{
	{
		name:   "empty plaintext pads one block",
		key:    []byte("0123456789abcdef"),
		plain:  "",
		cipher: "UEgEC/RgOaLmLr8n5K1/AA==",
	},
	{
		name:   "one byte",
		key:    []byte("0123456789abcdef"),
		plain:  "h",
		cipher: "70VjHSGny6yRBbnoAP6TRA==",
	},
	{
		name:   "exactly one block adds full pad block",
		key:    []byte("0123456789abcdef"),
		plain:  "0123456789abcdef",
		cipher: "jPukLeambZQ6NGPEOehkHRdVwP0pyiLKFGtkgagltoc=",
	},
	{
		name:   "unicode utf-8 payload",
		key:    []byte("0123456789abcdef"),
		plain:  "аниме test ▲笑话",
		cipher: "laNWhPwicWO5mYD/5ekHwP0+yzoJJom7bIF3hEamNPI=",
	},
	{
		name:   "aes-192",
		key:    []byte("0123456789abcdef01234567"),
		plain:  "gogoplay payload sample",
		cipher: "Q67f/3qky/iWPAtSGBxCcyLwC6NXL4i42kUbVkQVxEI=",
	},
	{
		name:   "aes-256",
		key:    []byte("0123456789abcdef0123456789abcdef"),
		plain:  "https://example.com/embed/e/abc123",
		cipher: "Jji6/H05zEmgV4fRNJBUQ89K4pKLLqpgqjpvB9kUHShjHFOL4qQzYqlLjA5DQbfN",
	},
	{
		name:   "61 bytes spans four blocks",
		key:    []byte("0123456789abcdef0123456789abcdef"),
		plain:  "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		cipher: "GAd1bXcKvzN6p2glVQNw1OvuPezRilVpE4SqiJ39FRr6HqKkzlixxiRWsAkgAhmqHl/HEFK7tGzx6MDSVK5p/g==",
	},
}

var aesIV = []byte("ABCDEFGHIJKLMNOP")

func TestAESEncryptMatchesPython(t *testing.T) {
	t.Parallel()

	for _, tt := range aesGoldens {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := AESEncrypt(tt.plain, tt.key, aesIV)
			if err != nil {
				t.Fatalf("AESEncrypt: %v", err)
			}
			if got != tt.cipher {
				t.Errorf("AESEncrypt(%q) = %q, want python golden %q", tt.plain, got, tt.cipher)
			}
		})
	}
}

func TestAESDecryptMatchesPython(t *testing.T) {
	t.Parallel()

	for _, tt := range aesGoldens {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := AESDecrypt(tt.cipher, tt.key, aesIV)
			if err != nil {
				t.Fatalf("AESDecrypt: %v", err)
			}
			if got != tt.plain {
				t.Errorf("AESDecrypt(%q) = %q, want python plaintext %q", tt.cipher, got, tt.plain)
			}
		})
	}
}

func TestAESRoundTrip(t *testing.T) {
	t.Parallel()

	// Not from the python oracle: property check over all key sizes and
	// plaintext lengths straddling block boundaries.
	keys := [][]byte{
		bytes.Repeat([]byte{0xA1}, 16),
		bytes.Repeat([]byte{0xB2}, 24),
		bytes.Repeat([]byte{0xC3}, 32),
	}
	iv := bytes.Repeat([]byte{0x0F}, 16)
	for _, key := range keys {
		for n := range 70 {
			plain := string(bytes.Repeat([]byte{'z'}, n))
			ct, err := AESEncrypt(plain, key, iv)
			if err != nil {
				t.Fatalf("AESEncrypt(len=%d, key=%d): %v", n, len(key), err)
			}
			back, err := AESDecrypt(ct, key, iv)
			if err != nil {
				t.Fatalf("AESDecrypt(len=%d, key=%d): %v", n, len(key), err)
			}
			if back != plain {
				t.Fatalf("round trip key=%d len=%d: got %q want %q", len(key), n, back, plain)
			}
		}
	}
}

func TestAESDecryptBadPadding(t *testing.T) {
	t.Parallel()

	key := []byte("0123456789abcdef")
	// Encrypt a valid payload, corrupt the final pad byte of the last block.
	ct, err := AESEncrypt("payload", key, aesIV)
	if err != nil {
		t.Fatalf("AESEncrypt: %v", err)
	}
	raw := decodeB64(t, ct)
	raw[len(raw)-1] ^= 0xFF
	_, err = AESDecrypt(encodeB64(t, raw), key, aesIV)
	if !errors.Is(err, ErrBadPadding) {
		t.Fatalf("corrupted padding must fail with ErrBadPadding, got %v", err)
	}
}

func TestAESKeyIVValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     []byte
		iv      []byte
		wantErr error
	}{
		{name: "key 15 bytes", key: make([]byte, 15), iv: make([]byte, 16), wantErr: ErrBadKeyLength},
		{name: "key 17 bytes", key: make([]byte, 17), iv: make([]byte, 16), wantErr: ErrBadKeyLength},
		{name: "iv 15 bytes", key: make([]byte, 16), iv: make([]byte, 15), wantErr: ErrBadIVLength},
		{name: "iv 17 bytes", key: make([]byte, 32), iv: make([]byte, 17), wantErr: ErrBadIVLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := AESEncrypt("x", tt.key, tt.iv); !errors.Is(err, tt.wantErr) {
				t.Errorf("AESEncrypt err = %v, want %v", err, tt.wantErr)
			}
			if _, err := AESDecrypt("AAAAAAAAAAAAAAAAAAAAAA==", tt.key, tt.iv); !errors.Is(err, tt.wantErr) {
				t.Errorf("AESDecrypt err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAESDecryptNotBase64(t *testing.T) {
	t.Parallel()

	if _, err := AESDecrypt("!!not base64!!", []byte("0123456789abcdef"), aesIV); err == nil {
		t.Fatal("invalid base64 input must fail loud")
	}
}

func TestAESDecryptNotBlockAligned(t *testing.T) {
	t.Parallel()

	key := []byte("0123456789abcdef")
	ct := encodeB64(t, []byte("13-byte ciphertext")) // not % 16
	if _, err := AESDecrypt(ct, key, aesIV); err == nil {
		t.Fatal("ciphertext not block aligned must fail loud")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "equal", a: "secret-token", b: "secret-token", want: true},
		{name: "differ last byte", a: "secret-tokem", b: "secret-token", want: false},
		{name: "length differs", a: "secret", b: "secrets", want: false},
		{name: "both empty", a: "", b: "", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ConstantTimeEqual([]byte(tt.a), []byte(tt.b)); got != tt.want {
				t.Errorf("ConstantTimeEqual(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
