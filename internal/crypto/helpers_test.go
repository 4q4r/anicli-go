package crypto

import (
	"encoding/base64"
	"testing"
)

// encodeB64/decodeB64 are test-only conveniences.
func encodeB64(t *testing.T, raw []byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(raw)
}

func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return raw
}
