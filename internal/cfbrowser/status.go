package cfbrowser

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// licenseKeyFile is the upstream pro-license marker inside the cache
// directory; its presence upgrades the reported tier.
const licenseKeyFile = "license.key"

// LicenseTier reports the cache's license tier: "pro" when
// license.key exists, "free" otherwise.
func LicenseTier(cacheDir string) string {
	if fi, err := os.Stat(filepath.Join(cacheDir, licenseKeyFile)); err == nil && !fi.IsDir() {
		return "pro"
	}
	return "free"
}

// ReadUpdateStatus loads the persisted update bookkeeping (absent or
// unreadable → ok=false, never an error: status display is advisory).
func ReadUpdateStatus(cacheDir string) (UpdateStatus, bool) {
	raw, err := os.ReadFile(filepath.Join(cacheDir, updateStatusFile)) //nolint:gosec // app-owned cache path
	if err != nil {
		return UpdateStatus{}, false
	}
	var st UpdateStatus
	if json.Unmarshal(raw, &st) != nil {
		return UpdateStatus{}, false
	}
	return st, true
}
