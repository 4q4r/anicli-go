package download

// PR165b T7 tests: the mkvmerge assembler argv (the owner's verbatim
// shape, generalized to N tracks), the version-gated default-track
// flag (owner order: ≥65 → --default-track-flag, <65 → the legacy
// --default-track; the rename happened in v65.0.0, 2022-02 — NOT 54
// as the plan's risk note guessed), the video-solo stage, and the
// exit-code contract (0 ok / 1 warnings=success / 2 error).

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/an0nx/anicli-go/internal/contracts"
)

func TestParseMKVMergeVersion(t *testing.T) {
	cases := []struct {
		out  string
		want int
	}{
		{"mkvmerge v102.0 ('Little Houses') 64-bit", 102},
		{"mkvmerge v65.0.0 ('And It Goes Like') 64-bit", 65},
		{"mkvmerge v54.0.0 ('A Little Bitty') 64-bit", 54},
		{"mkvmerge v7.1.0 ('Owl') 32-bit", 7},
	}
	for _, tc := range cases {
		got, err := parseMKVMergeVersion(tc.out)
		if err != nil || got != tc.want {
			t.Fatalf("parseMKVMergeVersion(%q) = %d, %v; want %d", tc.out, got, err, tc.want)
		}
	}
	if _, err := parseMKVMergeVersion("garbage without a version"); err == nil {
		t.Fatal("unparsable output must error")
	}
	if _, err := parseMKVMergeVersion(""); err == nil {
		t.Fatal("empty output must error")
	}
}

// TestDefaultTrackFlagFor pins the owner-ordered threshold against the
// upstream evidence: the flag rename shipped in v65.0.0 (2022-02), so
// 65 takes the modern form and 64 the legacy one.
func TestDefaultTrackFlagFor(t *testing.T) {
	for _, tc := range []struct {
		version int
		want    string
	}{
		{102, "--default-track-flag"},
		{65, "--default-track-flag"},
		{64, "--default-track"},
		{54, "--default-track"},
		{1, "--default-track"},
	} {
		if got := defaultTrackFlagFor(tc.version); got != tc.want {
			t.Fatalf("defaultTrackFlagFor(%d) = %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestBuildMkvMergeArgsOwnerShape(t *testing.T) {
	in := AssembleInput{
		VideoPath: "v.tmp.mkv",
		AudioFiles: []AssembleAudio{
			{Path: "dub.m4a", Lang: "rus", Title: "AniLibria.TV", Default: true},
		},
		OutputPath: "out.part.mkv",
		Title:      "Тайтл — эп 1",
	}
	want := []string{"-o", "out.part.mkv", "--no-audio", "v.tmp.mkv",
		"--language", "0:rus", "--track-name", "0:AniLibria.TV", "--default-track-flag", "0:yes", "dub.m4a",
		"--title", "Тайтл — эп 1"}
	if got := buildMkvMergeArgs(in, defaultTrackFlagFor(102)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

// TestBuildMkvMergeArgsLegacyFlag pins the pre-65 shape: the same
// builder renders the owner's verbatim legacy form when the probe
// says the installed mkvmerge predates the rename.
func TestBuildMkvMergeArgsLegacyFlag(t *testing.T) {
	in := AssembleInput{
		VideoPath:  "v.tmp.mkv",
		AudioFiles: []AssembleAudio{{Path: "dub.m4a", Lang: "rus", Title: "DUB", Default: true}},
		OutputPath: "out.part.mkv",
		Title:      "Тайтл",
	}
	want := []string{"-o", "out.part.mkv", "--no-audio", "v.tmp.mkv",
		"--language", "0:rus", "--track-name", "0:DUB", "--default-track", "0:yes", "dub.m4a",
		"--title", "Тайтл"}
	if got := buildMkvMergeArgs(in, defaultTrackFlagFor(54)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestBuildMkvMergeArgsMultiTrack(t *testing.T) {
	in := AssembleInput{
		VideoPath: "v.tmp.mkv",
		AudioFiles: []AssembleAudio{
			{Path: "a1.m4a", Lang: "rus", Title: "DUB1", Default: true},
			{Path: "a2.m4a", Lang: "", Title: "DUB2", Default: false},
		},
		OutputPath: "out.part.mkv",
		Title:      "Тайтл — эп 1",
	}
	got := buildMkvMergeArgs(in, defaultTrackFlagFor(102))
	want := []string{"-o", "out.part.mkv", "--no-audio", "v.tmp.mkv",
		"--language", "0:rus", "--track-name", "0:DUB1", "--default-track-flag", "0:yes", "a1.m4a",
		"--track-name", "0:DUB2", "--default-track-flag", "0:no", "a2.m4a",
		"--title", "Тайтл — эп 1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestBuildMkvMergeArgsChapters(t *testing.T) {
	in := AssembleInput{
		VideoPath:    "v.tmp.mkv",
		AudioFiles:   []AssembleAudio{{Path: "dub.m4a", Lang: "rus", Title: "DUB", Default: true}},
		ChaptersFile: "c.simple",
		OutputPath:   "out.part.mkv",
		Title:        "Тайтл",
	}
	got := buildMkvMergeArgs(in, defaultTrackFlagFor(102))
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "--chapters|c.simple") {
		t.Fatalf("chapters flag missing: %q", got)
	}
	// Chapters stay ahead of the global --title (mkvmerge option order).
	if strings.Index(joined, "--chapters") > strings.Index(joined, "--title") {
		t.Fatalf("--chapters must precede --title: %q", got)
	}
}

// TestBuildMkvMergeArgsDefaults: "" title renders the python default
// «Эпизод» (parity with the ffmpeg builders).
func TestBuildMkvMergeArgsDefaults(t *testing.T) {
	in := AssembleInput{
		VideoPath:  "v.tmp.mkv",
		AudioFiles: []AssembleAudio{{Path: "dub.m4a", Default: true}},
		OutputPath: "out.part.mkv",
	}
	got := buildMkvMergeArgs(in, defaultTrackFlagFor(102))
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "--title|Эпизод") {
		t.Fatalf("default title missing: %q", got)
	}
	// No Lang/Title on the track → neither per-track flag.
	if strings.Contains(joined, "--language") || strings.Contains(joined, "--track-name") {
		t.Fatalf("empty track metadata must omit the flags: %q", got)
	}
}

func TestBuildVideoSoloArgs(t *testing.T) {
	video := contracts.VideoSource{
		URL:     "https://x/v.m3u8",
		Headers: map[string]string{"Referer": "https://r/"},
	}
	got := buildVideoSoloArgs(video, "video.tmp.mkv")
	want := []string{"-y", "-hide_banner", "-loglevel", "error",
		"-headers", "Referer: https://r/\r\n",
		"-i", "https://x/v.m3u8", "-map", "0:v:0", "-c:v", "copy", "video.tmp.mkv"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

// TestRunAssembleExitCodes pins the mkvmerge exit-code contract:
// 0 = ok; 1 = WARNINGS = success (stderr logged, never an error);
// ≥2 = typed failure carrying one stderr line. An unparsable version
// probe is a typed error BEFORE any assembly runs (never a silently
// wrong flag).
func TestRunAssembleExitCodes(t *testing.T) {
	in := AssembleInput{
		VideoPath:  "v.tmp.mkv",
		AudioFiles: []AssembleAudio{{Path: "dub.m4a", Lang: "rus", Title: "DUB", Default: true}},
		OutputPath: "out.mkv",
		Title:      "Тайтл",
	}
	cases := []struct {
		name      string
		code      int
		stderr    string
		version   string
		versionEr error
		wantErr   bool
	}{
		{"ok", 0, "", "mkvmerge v102.0", nil, false},
		{"warnings_are_success", 1, "Warning: something minor", "mkvmerge v102.0", nil, false},
		{"error", 2, "Error: The file is not a Matroska document", "mkvmerge v102.0", nil, true},
		{"unparsable_version", 0, "", "total garbage", nil, true},
		{"version_probe_fails", 0, "", "", errors.New("exec failed"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := New(Options{})
			d.runMKV = func(ctx context.Context, bin string, args []string) (int, string, error) {
				if !strings.Contains(strings.Join(args, " "), defaultTrackFlagFor(102)) {
					t.Errorf("argv missing the modern default flag: %q", args)
				}
				return tc.code, tc.stderr, nil
			}
			d.mkvVersion = func(ctx context.Context, bin string) (string, error) {
				return tc.version, tc.versionEr
			}
			err := d.RunAssemble(context.Background(), in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RunAssemble err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && tc.name == "error" && !strings.Contains(err.Error(), "not a Matroska document") {
				t.Fatalf("typed error must carry one stderr line, got: %v", err)
			}
		})
	}
}

// TestMKVMergeVersionErrorTyped pins the typed unparsable-version
// error text (never a silent wrong flag).
func TestMKVMergeVersionErrorTyped(t *testing.T) {
	var err error = &ErrMKVMergeVersion{Output: "nonsense"}
	if !strings.Contains(err.Error(), "mkvmerge") {
		t.Fatalf("typed error text = %q", err.Error())
	}
}
