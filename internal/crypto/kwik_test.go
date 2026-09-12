package crypto

import "testing"

// Golden vectors generated from the Python original
// anicli-py/anicli/core/crypto.py (_kwik_get_string L14-41, kwik_decrypt
// L44-78) via `uv run --with pycryptodome python` running that logic
// verbatim. Inputs were built with the inverse routine:
//
//	def kwik_encrypt_char(c, key, v1, v2):
//	    n = ord(c) + v1
//	    ds = ""
//	    while n > 0:
//	        ds = str(n % v2) + ds
//	        n //= v2
//	    return "".join(key[int(d)] for d in ds) + key[v2]
//
// Round-trip only holds for v2 <= 10 (see concat case below).
var kwikGoldens = []struct {
	name   string
	in     string
	key    string
	v1, v2 int
	want   string
}{
	{
		name: "url round trip",
		in:   "cgcjchfjchfjchbjchejcbbjbiijbiijcgfjchijcgdjcgfjbihjchejcgdjbiijcfijbiijcfejcffjccbjcfhjcabjcacj",
		key:  "abcdefghijklmnopqrstuvwxyz",
		v1:   114, v2: 9,
		want: "https://kwik.si/e/abCd12",
	},
	{
		name: "ascii round trip",
		in:   "irpwrrpweipwrepwerpwqqpwwopweupwerprip",
		key:  "qwertyuiop",
		v1:   1, v2: 9,
		want: "AnimePahe!",
	},
	{
		name: "cyrillic round trip",
		in:   "bacbbaebaccabebacbdaebaccaaebacbcbecbabebaccbcebacbcbebaccbbebaccbce",
		key:  "abcde",
		v1:   100, v2: 4,
		want: "аниме-тест",
	},
	{
		name: "numeric alphabet round trip",
		in:   "2238237822182238",
		key:  "0123456789",
		v1:   40, v2: 8,
		want: "kwik",
	},
	{
		name: "high base uses positional digit concatenation",
		// v2=25: key indices >= 10 replace to multi-char digit strings that
		// concatenate positionally (python computes 'ጷttp' here — not a
		// round trip; locks the bug-compatible semantics).
		in:  "iszjfzjfzjbz",
		key: "abcdefghijklmnopqrstuvwxyz",
		v1:  114, v2: 25,
		want: "ጷttp",
	},
	{
		name: "non-digit chars count as zero",
		// segment "aXb" -> "0X1" -> 'X' contributes 0 -> value 1.
		in:  "aXb.",
		key: "abcd.",
		v1:  0, v2: 4,
		want: "\x01",
	},
	{
		name: "empty input",
		in:   "",
		key:  "abc",
		v1:   5, v2: 2,
		want: "",
	},
	{
		name: "zero input base still evaluates",
		// Python allows v2=0 (division is by the output base only):
		// kwik_decrypt("ba", "ab", 0, 0) == "\x01".
		in:  "ba",
		key: "ab",
		v1:  0, v2: 0,
		want: "\x01",
	},
}

func TestKwikDecryptMatchesPython(t *testing.T) {
	t.Parallel()

	for _, tt := range kwikGoldens {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := KwikDecrypt(tt.in, tt.key, tt.v1, tt.v2)
			if err != nil {
				t.Fatalf("KwikDecrypt: %v", err)
			}
			if got != tt.want {
				t.Errorf("KwikDecrypt(%q, key=%q, v1=%d, v2=%d) = %q, want %q",
					tt.in, tt.key, tt.v1, tt.v2, got, tt.want)
			}
		})
	}
}

func TestKwikDecryptErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		key  string
		v1   int
		v2   int
	}{
		// Python raises IndexError: string index out of range.
		{name: "no trailing delimiter", in: "a", key: "ab", v1: 0, v2: 1},
		// Python raises IndexError (key[v2] out of range).
		{name: "v2 out of key range", in: "a", key: "ab", v1: 0, v2: 5},
		// Python raises ValueError: chr() arg not in range(0x110000).
		{name: "empty segment with positive offset", in: "ba", key: "ab", v1: 5, v2: 1},
		// Empty key can never carry a delimiter (python IndexError).
		{name: "empty key", in: "a", key: "", v1: 0, v2: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := KwikDecrypt(tt.in, tt.key, tt.v1, tt.v2); err == nil {
				t.Errorf("KwikDecrypt(%q, key=%q, v1=%d, v2=%d) = nil error, want failure",
					tt.in, tt.key, tt.v1, tt.v2)
			}
		})
	}
}
