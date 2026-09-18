package extractors

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

// Fixture provenance (testdata/blogger_video_rpc.txt): verbatim live
// capture [LIVE-VERIFIED 2026-09-18] of the Blogger player data RPC
// (POST /_/BloggerVideoPlayerUi/data/batchexecute?rpcids=WcwnYd) for a
// real anitaku.io mirror (One Piece 1178, blogger.com/video.g embed).
// The embed page itself carries NO inline playback config (the modern
// WIZ/boq player fetches it through this RPC — capture kept with the
// PR53 evidence), so the extractor replays the RPC stdlib-only.
func bloggerRPCFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/blogger_video_rpc.txt")
	if err != nil {
		t.Fatalf("read rpc fixture: %v", err)
	}
	return data
}

// TestBloggerExtractorExtractsProgressiveFormats pins the extraction
// contract: the WcwnYd payload's streamingData JSON yields the
// progressive mp4 formats keyed by their own qualityLabel (720/360 —
// itags 22/18 on the live capture), NOT a hardcoded itag table.
func TestBloggerExtractorExtractsProgressiveFormats(t *testing.T) {
	t.Parallel()

	var gotPath, gotRawQuery string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bloggerRPCFixture(t))
	}))
	t.Cleanup(srv.Close)

	ex := &bloggerExtractor{http: testHTTPClient(t), batchExecuteURL: srv.URL + "/_/BloggerVideoPlayerUi/data/batchexecute"}
	embed := "https://www.blogger.com/video.g?token=AD6v5dyTestToken&origin=op.blogspot.com"
	sources, err := ex.Extract(context.Background(), embed)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// The request: the token rides the f.req form field inside the
	// WcwnYd row; the rpcid rides the query (minimal param set — the
	// live player needs no f.sid/cookies, verified 2026-09-18).
	if gotPath != "/_/BloggerVideoPlayerUi/data/batchexecute" {
		t.Errorf("rpc path = %q", gotPath)
	}
	if !strings.Contains(gotRawQuery, "rpcids=WcwnYd") {
		t.Errorf("rpc query = %q, want rpcids=WcwnYd", gotRawQuery)
	}
	if !strings.Contains(gotBody, `WcwnYd`) {
		t.Errorf("rpc body missing the WcwnYd row: %q", gotBody)
	}
	if !strings.Contains(gotBody, "AD6v5dyTestToken") {
		t.Errorf("rpc body missing the embed token: %q", gotBody)
	}

	// The result: progressive mp4s keyed by qualityLabel.
	if len(sources) != 2 {
		t.Fatalf("sources = %d entries, want 2 (720p + 360p)", len(sources))
	}
	hi, ok := sources["720"]
	if !ok {
		t.Fatalf("no 720 source: %v", sources)
	}
	if !strings.Contains(hi.URL, "itag=22") {
		t.Errorf("720 url = %q, want the itag 22 format", hi.URL)
	}
	if hi.Type != "mp4" {
		t.Errorf("720 type = %q, want mp4 (progressive)", hi.Type)
	}
	lo, ok := sources["360"]
	if !ok {
		t.Fatalf("no 360 source: %v", sources)
	}
	if !strings.Contains(lo.URL, "itag=18") {
		t.Errorf("360 url = %q, want the itag 18 format", lo.URL)
	}
}

// TestBloggerExtractorMatchesGate: the URL gate accepts the
// blogger.com/video.g embed shape the anitaku mirrors emit.
func TestBloggerExtractorMatchesGate(t *testing.T) {
	t.Parallel()

	ex := &bloggerExtractor{}
	if !ex.Matches("https://www.blogger.com/video.g?token=x&origin=op.blogspot.com") {
		t.Error("blogger.com/video.g embed must match")
	}
	if ex.Matches("https://megacloud.bloggy.click/embed/#x") {
		t.Error("other hosts must not match")
	}
}

// TestBloggerExtractorMissingTokenTypedError: an embed URL without a
// token cannot drive the RPC — typed shape error, never a silent {}.
func TestBloggerExtractorMissingTokenTypedError(t *testing.T) {
	t.Parallel()

	ex := &bloggerExtractor{http: testHTTPClient(t), batchExecuteURL: "https://blogger.invalid"}
	_, err := ex.Extract(context.Background(), "https://www.blogger.com/video.g?origin=op.blogspot.com")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
}

// TestBloggerExtractorNoFormatsTypedError: a well-formed RPC answer
// without usable mp4 formats (shape drift, DRM-gated payload) fails
// loud instead of yielding nothing.
func TestBloggerExtractorNoFormatsTypedError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(")]}'\n\n12345\n[[\"wrb.fr\",\"WcwnYd\",\"[1,null,[],\\\"x\\\",\\\"{\\\\\\\"other\\\\\\\":1}\\\",false,null,null,null,8]\",null,null,null,\"generic\"]]\n"))
	}))
	t.Cleanup(srv.Close)

	ex := &bloggerExtractor{http: testHTTPClient(t), batchExecuteURL: srv.URL}
	sources, err := ex.Extract(context.Background(), "https://www.blogger.com/video.g?token=t")
	if !errors.Is(err, contracts.ErrExtractFailed) {
		t.Fatalf("err = %v, want ErrExtractFailed", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want none", sources)
	}
}

// TestBloggerExtractorRegisteredInFactory: the extractor joins the
// factory walk (after kwik, the second task-mandated addition) so
// gogoanime's blogger mirrors resolve through the standard path.
func TestBloggerExtractorRegisteredInFactory(t *testing.T) {
	t.Parallel()

	f := NewFactory(testHTTPClient(t))
	found := false
	for _, ex := range f.extractors {
		if ex.Name() == "blogger" {
			found = true
			if !ex.Matches("https://www.blogger.com/video.g?token=x") {
				t.Error("registered blogger extractor must match the video.g gate")
			}
		}
	}
	if !found {
		t.Fatal("blogger extractor not registered in the factory")
	}
}
