//go:build live

package download

// LIVE proof for 169-A: the joined single -headers value must deliver
// EVERY header to the origin (the one-pair-per-header shape dropped all
// but the last — ffmpeg honors only the final -headers argument).
// Excluded from the hermetic suite by the `live` build tag. Run:
//
//	go test -tags live -run TestLiveJoinedHeadersArrive -count=1 -v ./internal/download/

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestLiveJoinedHeadersArrive(t *testing.T) {
	got := make(chan http.Header, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header
		_, _ = w.Write([]byte("not a stream")) // ffmpeg fails; the request is the point
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "out.mp4")
	cmd := BuildCommand(CommandInput{
		Video: contracts.VideoSource{
			URL: srv.URL + "/v.m3u8",
			Headers: map[string]string{
				"Referer":    "https://ref.example/",
				"User-Agent": "anicli-adv-proof",
			},
		},
		OutputPath: out,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d := New(Options{})
	_, _ = d.run(ctx, d.ffmpeg, cmd.Args) // the download fails by design; headers are the verdict

	select {
	case h := <-got:
		if h.Get("Referer") != "https://ref.example/" || h.Get("User-Agent") != "anicli-adv-proof" {
			t.Fatalf("origin saw Referer=%q User-Agent=%q, want both headers delivered", h.Get("Referer"), h.Get("User-Agent"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no request reached the origin")
	}
	_ = os.Remove(out)
}
