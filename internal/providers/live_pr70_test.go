//go:build live

// PR70 live proof: the build-id material is derived AUTOMATICALLY from
// the live crypto chunk and accepted by the live bootstrap endpoint.
// Prints the derived buildId as the auto-derivation evidence. Excluded
// from the hermetic default suite by the `live` build tag. Run manually:
//
//	ANICLI_LIVE_PROXY=http://127.0.0.1:10809 \
//	  go test -tags live -run TestLivePR70 -count=1 -v ./internal/providers/
//
// Steps: discover the crypto chunk from the live bundle graph (the
// production aaLiveChunkSource), parse it (aaParseChunkMaterial), log
// the derived constants, then bootstrap the real endpoint through the
// manager's own ladder and require HTTP 200 + non-empty partB.
package providers

import (
	"context"
	"os"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
)

func TestLivePR70AutoDerivedMaterial(t *testing.T) {
	cfg := config.Default()
	if proxy := os.Getenv("ANICLI_LIVE_PROXY"); proxy != "" {
		cfg.Network.ProxyURL = proxy
	}
	http, err := netclient.New(cfg.Network)
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}

	source := &aaLiveChunkSource{HTTP: http, Root: AllAnimeReferer}
	chunk, err := source.FetchChunk(context.Background())
	if err != nil {
		t.Fatalf("chunk discovery: %v", err)
	}
	prof, err := aaParseChunkMaterial(chunk)
	if err != nil {
		t.Fatalf("chunk parse: %v", err)
	}
	// THE PROOF: the material was derived from the live chunk, not the
	// pinned tables — log every derived constant.
	t.Logf("derived buildId:    %q", prof.BuildID)
	t.Logf("derived bootPrefix: %q", prof.BootPrefix)
	t.Logf("derived salt:       %d/%d  frag: %d/%d", prof.SaltMul, prof.SaltAdd, prof.FragMul, prof.FragAdd)
	t.Logf("derived parts:      %v join %q", prof.Parts, prof.Join)
	if prof.BuildID == aaDefaultBuildID {
		t.Logf("NOTE: live chunk still matches the pinned buildId %q (pre-rotation window)", aaDefaultBuildID)
	}

	mgr := newAAMaterialManager(aaMaterialDeps{
		BootstrapBase: "https://api.mkissa.net/client-crypto/v1/bootstrap",
		Referer:       AllAnimeReferer,
		RefererHost:   "mkissa.to",
		Lane:          aaContentLane,
		HTTP:          http,
		Chunks:        source,
	})
	mat, err := mgr.get(context.Background())
	if err != nil {
		t.Fatalf("manager bootstrap: %v", err)
	}
	if mat.BuildID != prof.BuildID {
		t.Errorf("manager used buildId %q, want the chunk-derived %q", mat.BuildID, prof.BuildID)
	}
	t.Logf("bootstrapped OK: buildId=%s epoch=%d key=%x…", mat.BuildID, mat.Epoch, mat.Key[:4])
}
