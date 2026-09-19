//go:build live

// PR67 buildId probe: bootstraps and resolves the episode query with
// candidate buildIds (env AA_BUILD_IDS, comma-separated; default
// "174,173") — discriminates a buildId-grace-window rotation (bootstrap
// OK on the old build while the episode resolver demands the new one)
// from an egress gate.
package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLivePR67BuildIdCandidates(t *testing.T) {
	cands := []string{"174", "173"}
	if v := os.Getenv("AA_BUILD_IDS"); v != "" {
		cands = strings.Split(v, ",")
	}
	p := aaPr67Provider(t)
	ctx := context.Background()

	for _, bid := range cands {
		bid = strings.TrimSpace(bid)
		if bid == "" {
			continue
		}
		mask, err := aaMask(bid)
		if err != nil {
			t.Fatalf("mask %q: %v", bid, err)
		}
		p.material.setBuildID(bid)
		p.material.maskOverride = mask
		p.material.mu.Lock()
		p.material.material = nil
		p.material.mu.Unlock()
		mat, err := p.material.get(ctx)
		if err != nil {
			t.Logf("[buildId %s] bootstrap: %v", bid, err)
			continue
		}
		t.Logf("[buildId %s] bootstrap OK: epoch=%d switchAt=%s", bid, mat.Epoch, mat.switchAt.Format(time.RFC3339))
		aaPr67EpisodePost(t, p, mat, "buildId-"+bid)
		time.Sleep(3 * time.Second)
	}
}
