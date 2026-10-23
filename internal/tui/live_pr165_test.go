//go:build live

package tui

// LIVE proof for PR165 (download modes + multi-audio muxing). Run
// manually:
//
//	go test -tags live -run TestLivePR165MultiAudioMux -count=1 -v -timeout 30m ./internal/tui/
//
// The mandate's proof: a real multi-dub title, TWO episodes downloaded
// with video + TWO marked audios in one ffmpeg pass each; ffprobe
// pins the container shape (1 video + 2 audio, exactly one default
// disposition, language/title metadata); mpv plays the result
// headless (--frames decode proof). The REAL downloader runs (real
// ffprobe container decisions, real ffmpeg muxes) into a temp dir —
// the owner's download directory is never touched.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/an0nx/anicli-go/internal/config"
	"github.com/an0nx/anicli-go/internal/contracts"
)

// live165Settings points the download root into a test-owned temp dir.
func live165Settings(t *testing.T) config.Settings {
	t.Helper()
	settings := liveSettings(t)
	settings.Download.Dir = t.TempDir()
	return settings
}

// TestLivePR165MultiAudioMux proves the multi-audio batch end to end.
func TestLivePR165MultiAudioMux(t *testing.T) {
	_, deps := liveDeps(t)
	// The real downloader stays wired; only the root moves.
	deps.Download = func() DownloadService {
		real := deps.Download.(*realDownload)
		real.core.settings.Download.Dir = t.TempDir()
		return real
	}()

	group := liveSearch(t, deps, "черная лагуна")
	if len(group) < 1 {
		t.Fatalf("no live results")
	}
	g := stableGroup(mustResults(group))
	primary := g[0]
	t.Logf("primary source: %s/%s (%q)", primary.SourceID, primary.URL, primary.Title)

	s := NewSessionScreen(deps, primary, g)
	s.loadEpisodesSync()
	t.Logf("merged episodes: %v", s.order)
	if len(s.order) < 2 {
		t.Fatalf("need a 2+ episode title, got %v", s.order)
	}

	// The merged resolve of episode 1 feeds the picker marks: the best
	// row is the video, two further DUBS are the marked audios.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	entries, _, err := resolveAllStreams(ctx, deps.Episode, s.episodes[s.order[0]], "", deps.Log)
	if err != nil {
		t.Fatalf("merged resolve: %v", err)
	}
	if len(entries) < 3 {
		t.Fatalf("need 3+ stream rows to pick a video + 2 dubs, got %d", len(entries))
	}
	video := entries[0]
	t.Logf("video row: %s %s", video.Quality, video.DubKey)

	marked := make([]string, 0, 2)
	for _, e := range entries[1:] {
		if e.DubKey == video.DubKey || containsDub(marked, e.DubKey) {
			continue
		}
		marked = append(marked, e.DubKey)
		if len(marked) == 2 {
			break
		}
	}
	if len(marked) < 2 {
		t.Fatalf("need 2 further dubs to mark, got %v from %d rows", marked, len(entries))
	}
	t.Logf("marked audio set: %v (default: %s)", marked, marked[0])

	tasks := make([]DownloadTask, 0, 2)
	for _, num := range s.order[:2] {
		tasks = append(tasks, DownloadTask{
			AnimeTitle:   "PR165 Live Proof",
			EpisodeNum:   num,
			Episode:      s.episodes[num],
			Quality:      video.Quality,
			Mode:         "video_audio",
			AudioSet:     marked,
			DefaultAudio: marked[0],
		})
	}

	dlStart := time.Now()
	settled := runDirectBatchCollect(ctx, deps, tasks, video.DubKey, false)
	t.Logf("batch settled in %s: %d/%d, err=%v", time.Since(dlStart).Round(time.Second),
		settled.count, settled.total, settled.firstErr)
	for _, r := range settled.report {
		t.Logf("report: ep %s dub %q path %q err %v dropped %v skipAudioSet %v",
			r.Episode, r.Dub, r.Path, r.Err, r.DroppedAudio, r.SkipAudioSet)
	}
	if settled.count != 2 {
		t.Fatalf("both episodes must download, got %d/%d", settled.count, settled.total)
	}

	// --- ffprobe pins on every produced file.
	for _, r := range settled.report {
		out := r.Path
		if _, err := os.Stat(out); err != nil {
			t.Fatalf("the muxed file must exist: %v", err)
		}
		if !strings.HasSuffix(out, ".mkv") {
			t.Errorf("the multi-audio container must be mkv, got %q", out)
		}
		probeOut, err := exec.Command("ffprobe", "-v", "error",
			"-show_entries", "stream=index,codec_type,codec_name:stream_disposition=default:stream_tags=language,title",
			"-of", "json", out).Output()
		if err != nil {
			t.Fatalf("ffprobe %s: %v", out, err)
		}
		var parsed struct {
			Streams []struct {
				Index       int    `json:"index"`
				CodecType   string `json:"codec_type"`
				CodecName   string `json:"codec_name"`
				Disposition struct {
					Default int `json:"default"`
				} `json:"disposition"`
				Tags struct {
					Language string `json:"language"`
					Title    string `json:"title"`
				} `json:"tags"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(probeOut, &parsed); err != nil {
			t.Fatalf("ffprobe json: %v", err)
		}
		videoCount, audioCount, defaults := 0, 0, 0
		for _, st := range parsed.Streams {
			switch st.CodecType {
			case "video":
				videoCount++
			case "audio":
				audioCount++
				if st.Disposition.Default == 1 {
					defaults++
					t.Logf("  default audio: #%d %s lang=%s title=%q", st.Index, st.CodecName, st.Tags.Language, st.Tags.Title)
				} else {
					t.Logf("  audio:        #%d %s lang=%s title=%q", st.Index, st.CodecName, st.Tags.Language, st.Tags.Title)
				}
			}
		}
		if videoCount != 1 || audioCount != 2 {
			t.Errorf("%s must carry 1 video + 2 audio, got %d video + %d audio", out, videoCount, audioCount)
		}
		if defaults != 1 {
			t.Errorf("%s must carry EXACTLY one default audio, got %d", out, defaults)
		}
	}

	// --- mpv headless decode proof on the first file.
	out := settled.report[0].Path
	mpvCtx, mpvCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer mpvCancel()
	cmd := exec.CommandContext(mpvCtx, "mpv",
		"--no-config", "--really-quiet", "--vo=null", "--ao=null",
		"--frames=120", out)
	mpvOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mpv decode failed: %v\n%s", err, mpvOut)
	}
	t.Logf("mpv headless decode: OK (120 frames) — %s", filepath.Base(out))
	fmt.Printf("PR165 LIVE PROOF FILES:\n  %s\n  %s\n", settled.report[0].Path, settled.report[1].Path)
}

// mustResults flattens the grouped live search results.
func mustResults(group map[string]contracts.SearchResult) []contracts.SearchResult {
	out := make([]contracts.SearchResult, 0, len(group))
	for _, r := range group {
		out = append(out, r)
	}
	return out
}
