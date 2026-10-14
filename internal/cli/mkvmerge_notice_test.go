package cli

// PR165b T12: the startup mkvmerge version probe — the owner-ordered
// once-per-launch check. An installed mkvmerge with an UNPARSABLE
// version means the sync cannot pick its default-track flag: the
// download degrades to unsynchronized, and this notice says so up
// front (never a silent wrong flag at mux time). An ABSENT mkvmerge
// is already named by the sysdeps startup warning — no second notice.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	anicli "github.com/an0nx/anicli-go"
	"github.com/an0nx/anicli-go/internal/i18n"
)

// fakeMkvmergeOnPATH puts a fake mkvmerge printing the given output
// at the FRONT of PATH for the duration of the test.
func fakeMkvmergeOnPATH(t *testing.T, output string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' " + strconvQuote(output) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "mkvmerge"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: the test writes its own fake binary
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
}

func strconvQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\"'"'"'`) + "'"
}

func bundleEn(t *testing.T) {
	t.Helper()
	b, err := i18n.Load(anicli.Locales, "", "en")
	if err != nil {
		t.Fatalf("load bundled en: %v", err)
	}
	prev := i18n.Active()
	i18n.SetBundle(b)
	t.Cleanup(func() { i18n.SetBundle(prev) })
}

func TestMkvmmergeSyncNotice(t *testing.T) {
	bundleEn(t)
	ctx := context.Background()

	// A healthy mkvmerge: no notice.
	fakeMkvmergeOnPATH(t, "mkvmerge v102.0 64-bit")
	if got := mkvmergeSyncNotice(ctx); got != "" {
		t.Fatalf("healthy probe notice = %q, want empty", got)
	}

	// An unparsable version: the degrade notice.
	fakeMkvmergeOnPATH(t, "total garbage")
	if got := mkvmergeSyncNotice(ctx); got == "" {
		t.Fatal("an unparsable version must surface the degrade notice")
	}

	// No mkvmerge at all: the deps warning already covers it — no
	// second notice.
	t.Setenv("PATH", "/nonexistent-for-pr165b")
	if got := mkvmergeSyncNotice(ctx); got != "" {
		t.Fatalf("absent mkvmerge notice = %q, want empty (the deps warning owns it)", got)
	}
}
