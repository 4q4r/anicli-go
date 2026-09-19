package extractors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aksorFixture loads the captured aksor API body.
func aksorFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // trusted testdata path
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// TestAksorMatches pins the URL gate: every aksor host variant the
// upstream regex accepts (player.aksor.tv, aksor.tv, aksor.yani.tv —
// aksor.py:13-20) matches on the "aksor" substring, foreign players do
// not.
func TestAksorMatches(t *testing.T) {
	t.Parallel()

	e := &aksorExtractor{}
	yes := []string{
		"https://player.aksor.tv/video/fccc776e8f4908a63a613f39ced88f27",
		"https://aksor.tv/video/abc",
		"https://aksor.yani.tv/embed/9",
		"http://www.player.aksor.tv/video/abc",
	}
	no := []string{
		"https://kodikplayer.com/season/1/2/720p",
		"https://video.sibnet.ru/shell.php?videoid=1",
		"https://alloha.yani.tv/?token_movie=x",
		"https://ru.yummyani.me/iframeCVH.html?dubbing_code=X",
	}
	for _, u := range yes {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true", u)
		}
	}
	for _, u := range no {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false", u)
		}
	}
}

// TestAksorExtractRoundTrip pins the full resolve against the real
// captured API body (testdata/aksor_video.json, captured live from
// https://player.aksor.tv/api/video/fccc776e8f4908a63a613f39ced88f27 on
// 2026-09-19): the video id is taken from the embed path (query string
// stripped, aksor.py:30), the API is queried with the upstream headers
// (Accept: application/json, Referer: old.yummyani.me —
// aksor_parser.py fetch), null qualities are skipped and the type is
// the URL suffix.
func TestAksorExtractRoundTrip(t *testing.T) {
	t.Parallel()

	var (
		gotPath    string
		gotQuery   string
		gotAccept  string
		gotReferer string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAccept = r.Header.Get("Accept")
		gotReferer = r.Header.Get("Referer")
		_, _ = rw.Write(aksorFixture(t, "aksor_video.json"))
	}))
	t.Cleanup(srv.Close)

	e := &aksorExtractor{http: testHTTPClient(t)}
	sources, err := e.Extract(context.Background(), srv.URL+"/video/fccc776e8f4908a63a613f39ced88f27?season=1&episode=1")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if gotPath != "/api/video/fccc776e8f4908a63a613f39ced88f27" {
		t.Errorf("api path = %q, want /api/video/<id> (query stripped from the id)", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("api query = %q, want empty", gotQuery)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
	if gotReferer != "https://old.yummyani.me/" {
		t.Errorf("Referer = %q, want https://old.yummyani.me/", gotReferer)
	}

	// The capture carries exactly one non-null quality: q1080 → an
	// .mpd URL; the null q360/q480/q720/q2k/q4k never surface.
	if len(sources) != 1 {
		t.Fatalf("sources = %d entries (%v), want 1", len(sources), sources)
	}
	src, ok := sources["1080"]
	if !ok {
		t.Fatalf("no 1080 entry: %v", sources)
	}
	want := "https://cdn13.takehost-cdn.aksor.tv/static13/video/a1080/MC Entertainment/01/1080.mpd"
	if src.URL != want {
		t.Errorf("1080 URL = %q, want %q", src.URL, want)
	}
	if src.Quality != "1080" {
		t.Errorf("quality = %q, want 1080", src.Quality)
	}
	if src.Type != "mpd" {
		t.Errorf("type = %q, want mpd (the URL suffix)", src.Type)
	}
}

// TestAksorExtractAllQualitiesNull pins the upstream empty outcome
// (aksor.py:33-35: falsy urls are skipped, the parser returns an empty
// list without an error).
func TestAksorExtractAllQualitiesNull(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte(`{"id":"x","qualities":{"q1080":null,"q360":null,"q480":null,"q720":null,"q2k":null,"q4k":null}}`))
	}))
	t.Cleanup(srv.Close)

	e := &aksorExtractor{http: testHTTPClient(t)}
	sources, err := e.Extract(context.Background(), srv.URL+"/video/x")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(sources) != 0 {
		t.Errorf("sources = %v, want empty", sources)
	}
}

// TestAksorExtractUnknownQualityKey pins the defensive mapping: a
// quality key outside the upstream _QUALITY_KEYS table is skipped, not
// a crash (the Python dict index would raise KeyError).
func TestAksorExtractUnknownQualityKey(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte(`{"id":"x","qualities":{"q1080":"https://cdn.example/a.mp4","q8k":"https://cdn.example/b.mp4"}}`))
	}))
	t.Cleanup(srv.Close)

	e := &aksorExtractor{http: testHTTPClient(t)}
	sources, err := e.Extract(context.Background(), srv.URL+"/video/x")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %v, want only the known q1080 entry", sources)
	}
	if src := sources["1080"]; src.URL != "https://cdn.example/a.mp4" || src.Type != "mp4" {
		t.Errorf("1080 = %+v, want the mp4 URL", src)
	}
}

// TestAksorExtractTransportError pins the typed transport failure.
func TestAksorExtractTransportError(t *testing.T) {
	t.Parallel()

	e := &aksorExtractor{http: testHTTPClient(t)}
	_, err := e.Extract(context.Background(), "http://127.0.0.1:1/video/x")
	if err == nil {
		t.Fatal("error = nil, want the transport failure")
	}
	if !strings.Contains(err.Error(), "extractor:aksor") {
		t.Errorf("error = %v, want extractor:aksor context", err)
	}
}
