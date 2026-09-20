package cfbrowser

import (
	"context"
	"encoding/json"
	"fmt"
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

// VerdictSummaryLine renders the one-line advisory verdict-store
// status for `cf status`: the last-known-good binary and the verdict
// counts. Empty when the store carries nothing (the display is
// advisory and never fails).
func VerdictSummaryLine(cacheDir string) string {
	s := loadVerdictStore(cacheDir)
	b := s.bucket()
	var goods, bads int
	for _, e := range b.Verdicts {
		if e.Verdict == "bad" {
			bads++
			continue
		}
		goods++
	}
	if lkg := b.LastKnownGood; lkg != nil {
		return fmt.Sprintf("последняя работавшая: %s (канал %s, проверено %s); good=%d, bad=%d",
			lkg.Version, lkg.Channel, lkg.CheckedAt.Format("2006-01-02 15:04"), goods, bads)
	}
	if goods+bads == 0 {
		return ""
	}
	return fmt.Sprintf("последняя работавшая неизвестна; good=%d, bad=%d", goods, bads)
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
