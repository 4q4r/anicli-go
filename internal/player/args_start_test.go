package player

import (
	"reflect"
	"strings"
	"testing"
)

// TestBuildArgsStartEmittedOnlyWhenPositive (#166): the redub resume
// relaunch must carry the captured playback position as --start so mpv
// continues where the user was instead of restarting the episode from
// zero. The flag is emitted ONLY for a positive position — every other
// launch (fresh watch, offline, next episode) builds the exact argv it
// always did. Placement: right after --save-position-on-quit, the
// position family of flags kept together.
func TestBuildArgsStartEmittedOnlyWhenPositive(t *testing.T) {
	t.Parallel()

	withStart := BuildArgs(Request{
		URL:           "https://cdn.example/ep.m3u8",
		StartPosition: 187.5,
	}, Options{Bin: "mpv"})

	idxFlag := -1
	for i, arg := range withStart {
		if arg == "--start=187.5" {
			idxFlag = i
			break
		}
	}
	if idxFlag < 0 {
		t.Fatalf("argv must contain --start=187.5, got %v", withStart)
	}
	if withStart[idxFlag-1] != "--save-position-on-quit" {
		t.Errorf("--start must sit directly after --save-position-on-quit, got %v", withStart)
	}
	if last := withStart[len(withStart)-1]; last != "https://cdn.example/ep.m3u8" {
		t.Errorf("the URL must stay the final argv element, got %v", withStart)
	}

	withoutStart := BuildArgs(Request{URL: "https://cdn.example/ep.m3u8"}, Options{Bin: "mpv"})
	for _, arg := range withoutStart {
		if strings.HasPrefix(arg, "--start=") {
			t.Fatalf("a zero position must not emit --start, got %v", withoutStart)
		}
	}
	if !reflect.DeepEqual(withoutStart, BuildArgs(Request{URL: "https://cdn.example/ep.m3u8"}, Options{Bin: "mpv"})) {
		t.Fatal("the zero-position argv must be deterministic")
	}
}
