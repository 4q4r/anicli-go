package crypto

import (
	"fmt"
	"math/big"
	"strings"
)

// characterMap mirrors python CHARACTER_MAP (crypto.py L8-11): the alphabet
// used by the Kwik base-conversion routines.
const characterMap = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ+/"

// kwikGetString converts a digit string between custom bases. It interprets
// content as a base-s1 number (non-digit characters count as zero, exactly
// like python's `int(char) if char.isdigit() else 0`) and re-encodes it in
// base s2 using the first s2 characters of characterMap.
//
// Port of python _kwik_get_string (crypto.py L14-41); math/big replaces
// python's unbounded ints.
func kwikGetString(content string, s1, s2 int) string {
	target := characterMap[:s2]

	acc := new(big.Int)
	power := new(big.Int).SetInt64(1) // s1^0
	base := new(big.Int).SetInt64(int64(s1))
	contrib := new(big.Int)
	for i := len(content) - 1; i >= 0; i-- {
		digit := int64(0)
		if c := content[i]; c >= '0' && c <= '9' {
			digit = int64(c - '0')
		}
		contrib.Mul(power, big.NewInt(digit))
		acc.Add(acc, contrib)
		power.Mul(power, base)
	}

	if acc.Sign() == 0 {
		return "0"
	}
	var encoded strings.Builder
	rem := new(big.Int)
	div := big.NewInt(int64(s2))
	digit := new(big.Int)
	for acc.Sign() > 0 {
		acc.QuoRem(acc, div, rem)
		digit.SetInt64(rem.Int64())
		encoded.WriteString(target[digit.Int64() : digit.Int64()+1])
	}
	// Digits were produced least-significant first; reverse once here.
	return reverseString(encoded.String())
}

func reverseString(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// maxCodePoint is python chr()'s domain upper bound.
const maxCodePoint = 0x10FFFF

// KwikDecrypt decodes Kwik/AnimePahe obfuscated strings. key is the page's
// obfuscation alphabet, v1 the character offset and v2 the index into key
// that both delimits segments and sets the segment base.
//
// Port of python kwik_decrypt (crypto.py L44-78), bug-compatible by
// design:
//   - key characters are replaced by their index digit strings strictly in
//     key order (python's sequential str.replace), so indices >= 10
//     concatenate positionally — see the golden test "high base";
//   - non-digit characters surviving replacement count as zero;
//   - python's IndexError (delimiter never found, v2 out of range, empty
//     key) and ValueError (code point outside chr's domain) become typed
//     errors here.
//
// Note: python chr() admits surrogate code points which Go renders as
// U+FFFD; real kwik payloads are printable and never hit this.
func KwikDecrypt(fullString, key string, v1, v2 int) (string, error) {
	if v2 < 0 || v2 >= len(key) {
		return "", fmt.Errorf("kwik: v2 %d out of range for key of length %d", v2, len(key))
	}
	delimiter := key[v2]

	var result strings.Builder
	index := 0
	for index < len(fullString) {
		var segment strings.Builder
		for index < len(fullString) && fullString[index] != delimiter {
			segment.WriteByte(fullString[index])
			index++
		}
		if index >= len(fullString) {
			return "", fmt.Errorf("kwik: input ends without delimiter %q", string(delimiter))
		}

		replaced := segment.String()
		for keyIndex := range len(key) {
			replaced = strings.ReplaceAll(replaced, string(key[keyIndex]), fmt.Sprintf("%d", keyIndex))
		}

		value, ok := new(big.Int).SetString(kwikGetString(replaced, v2, 10), 10)
		if !ok {
			return "", fmt.Errorf("kwik: segment %q produced non-numeric value", segment.String())
		}
		codePoint := new(big.Int).Sub(value, big.NewInt(int64(v1)))
		if codePoint.Sign() < 0 || codePoint.Cmp(big.NewInt(maxCodePoint)) > 0 {
			return "", fmt.Errorf("kwik: code point %s out of range", codePoint.String())
		}
		// Value is provably within [0, 0x10FFFF] (maxCodePoint check
		// above), so the int64 -> rune conversion cannot lose data.
		result.WriteRune(rune(codePoint.Int64())) //nolint:gosec // bounded by maxCodePoint
		index++                                   // step over the delimiter
	}
	return result.String(), nil
}
