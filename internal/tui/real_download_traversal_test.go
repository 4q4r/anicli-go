package tui

// 169-D: the quality fragment of the download filename is a RAW
// provider-controlled map key (anilib main.lua builds
// links[tostring(q.quality)]), not a validated number — a hostile key
// like "../../x" glued path separators into EP_<n>_<dub>_<q>p.<ext>
// and the composed OutputPath escaped the download root (a real
// ffmpeg wrote outside it in the live repro). The key is sanitized
// like every other fragment, and a containment belt re-checks the
// composed path against the download root.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
	"github.com/an0nx/anicli-go/internal/providers"
)

// hostileQualityProvider answers ResolveStream with provider-controlled
// hostile quality keys (the threat model of 169-D).
type hostileQualityProvider struct {
	id    string
	links map[string]contracts.VideoSource
}

func (p *hostileQualityProvider) ID() string                       { return p.id }
func (p *hostileQualityProvider) Name() string                     { return p.id }
func (p *hostileQualityProvider) BaseURL() string                  { return "https://" + p.id + ".example" }
func (p *hostileQualityProvider) SourceType() contracts.SourceType { return contracts.SourceTypeBoth }
func (p *hostileQualityProvider) Search(context.Context, string) ([]contracts.SearchResult, error) {
	return nil, nil
}
func (p *hostileQualityProvider) GetEpisodes(context.Context, string) ([]contracts.Episode, error) {
	return nil, nil
}
func (p *hostileQualityProvider) ResolveStream(context.Context, contracts.Episode, string) (contracts.MediaStream, error) {
	return contracts.MediaStream{DubName: "Dub", Links: p.links}, nil
}

func hostileQualityCore(t *testing.T, quality string) (*realCore, *capturingDownloader, string) {
	t.Helper()
	prov := &hostileQualityProvider{id: "hostile", links: map[string]contracts.VideoSource{
		quality: {URL: "https://cdn.example/v.m3u8", Quality: "hostile"},
	}}
	reg := providers.NewEmptyRegistry()
	if err := reg.Register(prov); err != nil {
		t.Fatalf("register: %v", err)
	}
	dl := &capturingDownloader{}
	root := t.TempDir()
	c := &realCore{registry: reg, dl: dl, prober: &stubProber{}, log: testLogger()}
	c.settings.Download.Dir = root
	return c, dl, root
}

func hostileQualityTask() DownloadTask {
	return DownloadTask{
		ProviderID: "hostile",
		DubID:      "[hostile] Dub",
		Mode:       "video_audio",
		Episode: contracts.Episode{
			Num:   "1",
			RawID: `hostile:{"n":"1","u":"https://hostile.example/1.html"}`,
			RawEmbeds: map[string][]string{
				"Dub": {"u1"},
			},
		},
	}
}

// relEscapesRoot reports whether path sits outside root (the
// containment belt's own verdict, used to assert both directions).
func relEscapesRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestDownloadOneHostileQualityKeyStaysInRoot (169-D): every hostile
// quality key must produce an OutputPath inside the download root —
// sanitized, never escaped, never a ffmpeg option-shaped fragment.
func TestDownloadOneHostileQualityKeyStaysInRoot(t *testing.T) {
	for _, quality := range []string{"../../x", "-o /tmp/evil", "p.mp4/../../x", `..\..\x`} {
		t.Run(quality, func(t *testing.T) {
			c, dl, root := hostileQualityCore(t, quality)

			_, _, _, err := c.downloadOne(context.Background(), hostileQualityTask(), nil)
			if err != nil {
				t.Fatalf("downloadOne: %v", err)
			}
			if len(dl.inputs) != 1 {
				t.Fatalf("one ffmpeg run expected, got %d", len(dl.inputs))
			}
			out := dl.inputs[0].OutputPath
			if relEscapesRoot(root, out) {
				t.Fatalf("OutputPath %q escaped the download root %q", out, root)
			}
			base := filepath.Base(out)
			if strings.ContainsAny(base, `/\`) {
				t.Fatalf("filename fragment %q carries separators", base)
			}
			if strings.HasPrefix(base, "-") {
				t.Fatalf("filename fragment %q is option-shaped", base)
			}
		})
	}
}
