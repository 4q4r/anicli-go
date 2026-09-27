package tui

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/skip"
)

// stubSkipHTTP answers Get/PostJSON from memory — no sockets. The
// real-loopback variant (httptest + netclient) flaked ~1/10 package
// runs: httptest connection teardown races in shared fd-number space
// under -race (the same PR43 finding that moved the buffered tests
// off real GETs).
type stubSkipHTTP struct {
	status int
	body   string
}

func (s *stubSkipHTTP) Get(_ context.Context, _ string, _ map[string]string) (*netclient.Response, error) {
	if s.status >= 400 {
		// netclient's contract: non-2xx surfaces as an error (the
		// manager's transport-failure verdict), never as a body.
		return nil, fmt.Errorf("request: HTTP %d: unexpected http status %d", s.status, s.status)
	}
	return &netclient.Response{StatusCode: s.status, Status: "200 OK", Body: []byte(s.body)}, nil
}

func (s *stubSkipHTTP) PostJSON(_ context.Context, _ string, _ any, _ map[string]string) (*netclient.Response, error) {
	return &netclient.Response{StatusCode: s.status, Body: []byte(s.body)}, nil
}

// newSkipTestPlayback builds a realPlayback whose skip manager reads
// payload (or status), logging into the returned buffer.
func newSkipTestPlayback(t *testing.T, status int, payload string) (*realPlayback, *bytes.Buffer) {
	t.Helper()
	m := skip.NewManager(config.Skip{}, &stubSkipHTTP{status: status, body: payload},
		skip.WithAniSkip(skip.AniSkipOptions{BaseURL: "https://aniskip.stub"}))
	buf := &bytes.Buffer{}
	return &realPlayback{skips: m, log: slog.New(slog.NewTextHandler(buf, nil))}, buf
}

// TestRealPlaybackSkipNoteSuccess (PR61): a found answer produces the
// chapter file plus the range note, and both outcomes hit the file
// logger. The payload is the live v2 camelCase shape.
func TestRealPlaybackSkipNoteSuccess(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, 200,
		`{"found":true,"results":[{"interval":{"startTime":0,"endTime":90},"skipType":"op","episodeLength":1440}]}`)

	path, cleanup, note, err := pb.ResolveSkips(context.Background(), 21, 2)
	if err != nil {
		t.Fatalf("ResolveSkips: %v", err)
	}
	if path == "" {
		t.Fatal("chapters path expected")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("chapters file must exist: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the chapters file")
	}
	if note != "skips: op 0:00–1:30" {
		t.Fatalf("note = %q, want the op range", note)
	}
	if !strings.Contains(logs.String(), "skips: resolved") {
		t.Fatalf("success must reach the file logger, got:\n%s", logs.String())
	}
}

// TestRealPlaybackSkipNoteNotFound (PR61): a clean found=false answer
// is the «не найдены» note with an info log — not silence.
func TestRealPlaybackSkipNoteNotFound(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, 200, `{"found":false,"results":[]}`)

	path, cleanup, note, err := pb.ResolveSkips(context.Background(), 21, 2)
	if err != nil || path != "" {
		t.Fatalf("clean empty must give no file and no error, got %q/%v", path, err)
	}
	cleanup()
	if note != "skips: not found" {
		t.Fatalf("note = %q, want not found", note)
	}
	if !strings.Contains(logs.String(), "skips: no entry") {
		t.Fatalf("the miss must reach the file logger, got:\n%s", logs.String())
	}
}

// TestRealPlaybackSkipNoteUnavailable (PR61): a transport failure is
// the «недоступны» note with a warn log.
func TestRealPlaybackSkipNoteUnavailable(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, 500, `{}`)

	_, _, note, err := pb.ResolveSkips(context.Background(), 21, 2)
	if err == nil {
		t.Fatal("the transport failure must be reported")
	}
	if note != "skips: unavailable" {
		t.Fatalf("note = %q, want unavailable", note)
	}
	if !strings.Contains(logs.String(), "skips: lookup failed") {
		t.Fatalf("the failure must reach the file logger, got:\n%s", logs.String())
	}
}

// TestRealPlaybackSkipNoteNoBinding (PR61): without a shikimori
// binding no fetch runs, the note stays empty and the skip is logged.
func TestRealPlaybackSkipNoteNoBinding(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, 200, `{"found":false,"results":[]}`)

	path, cleanup, note, err := pb.ResolveSkips(context.Background(), 0, 2)
	if err != nil || path != "" || note != "" {
		t.Fatalf("no binding must be a silent-empty surface, got %q/%q/%v", path, note, err)
	}
	cleanup()
	if !strings.Contains(logs.String(), "skips: no binding") {
		t.Fatalf("the typed skip must reach the file logger, got:\n%s", logs.String())
	}
}
