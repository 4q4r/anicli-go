package tui

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/netclient"
	"github.com/an0nx/anicli-go/internal/skip"
)

// newSkipTestPlayback builds a realPlayback whose skip manager points
// at an httptest aniskip v2 server answering with payload (or status),
// logging into the returned buffer.
func newSkipTestPlayback(t *testing.T, status int, payload string) (*realPlayback, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	cfgNet := config.Default().Network
	cfgNet.ProxyURL = ""
	cfgNet.RequestTimeout = 5 * time.Second
	net, err := netclient.New(cfgNet, netclient.WithProvider("tui-skip-note-test"))
	if err != nil {
		t.Fatalf("netclient: %v", err)
	}
	m := skip.NewManager(config.Skip{}, net, skip.WithAniSkip(skip.AniSkipOptions{BaseURL: srv.URL}))
	buf := &bytes.Buffer{}
	return &realPlayback{skips: m, log: slog.New(slog.NewTextHandler(buf, nil))}, buf
}

// TestRealPlaybackSkipNoteSuccess (PR61): a found answer produces the
// chapter file plus the range note, and both outcomes hit the file
// logger. The payload is the live v2 camelCase shape.
func TestRealPlaybackSkipNoteSuccess(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, http.StatusOK,
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
	if note != "скипы: op 0:00–1:30" {
		t.Fatalf("note = %q, want the op range", note)
	}
	if !strings.Contains(logs.String(), "skips: resolved") {
		t.Fatalf("success must reach the file logger, got:\n%s", logs.String())
	}
}

// TestRealPlaybackSkipNoteNotFound (PR61): a clean found=false answer
// is the «не найдены» note with an info log — not silence.
func TestRealPlaybackSkipNoteNotFound(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, http.StatusOK, `{"found":false,"results":[]}`)

	path, cleanup, note, err := pb.ResolveSkips(context.Background(), 21, 2)
	if err != nil || path != "" {
		t.Fatalf("clean empty must give no file and no error, got %q/%v", path, err)
	}
	cleanup()
	if note != "скипы: не найдены" {
		t.Fatalf("note = %q, want не найдены", note)
	}
	if !strings.Contains(logs.String(), "skips: no entry") {
		t.Fatalf("the miss must reach the file logger, got:\n%s", logs.String())
	}
}

// TestRealPlaybackSkipNoteUnavailable (PR61): a transport failure is
// the «недоступны» note with a warn log.
func TestRealPlaybackSkipNoteUnavailable(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, http.StatusInternalServerError, `{}`)

	_, _, note, err := pb.ResolveSkips(context.Background(), 21, 2)
	if err == nil {
		t.Fatal("the transport failure must be reported")
	}
	if note != "скипы: недоступны" {
		t.Fatalf("note = %q, want недоступны", note)
	}
	if !strings.Contains(logs.String(), "skips: lookup failed") {
		t.Fatalf("the failure must reach the file logger, got:\n%s", logs.String())
	}
}

// TestRealPlaybackSkipNoteNoBinding (PR61): without a shikimori
// binding no fetch runs, the note stays empty and the skip is logged.
func TestRealPlaybackSkipNoteNoBinding(t *testing.T) {
	pb, logs := newSkipTestPlayback(t, http.StatusOK, `{"found":false,"results":[]}`)

	path, cleanup, note, err := pb.ResolveSkips(context.Background(), 0, 2)
	if err != nil || path != "" || note != "" {
		t.Fatalf("no binding must be a silent-empty surface, got %q/%q/%v", path, note, err)
	}
	cleanup()
	if !strings.Contains(logs.String(), "skips: no binding") {
		t.Fatalf("the typed skip must reach the file logger, got:\n%s", logs.String())
	}
}
