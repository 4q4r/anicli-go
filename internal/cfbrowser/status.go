package cfbrowser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

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

// StatusLicenseReport resolves the license tier for `cf status`:
// tier "pro" requires an effectively-valid license (cache-backed,
// stale fallback on network failure); anything else is "free" with a
// diagnostic note. It never fails: status display is advisory.
func StatusLicenseReport(ctx context.Context, opts LicenseOptions) (tier, plan, expires, note string) {
	rep, err := CheckLicense(ctx, opts)
	if err != nil {
		return "free", "", "", "лицензия не проверена: " + err.Error()
	}
	if rep == nil {
		return "free", "", "", ""
	}
	if rep.Status.Valid {
		note = ""
		if rep.Stale {
			note = "офлайн: данные из кэша"
		}
		return "pro", rep.Status.Plan, rep.Status.Expires, note
	}
	return "free", rep.Status.Plan, rep.Status.Expires, "ключ недействителен или истёк"
}
