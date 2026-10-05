// Package luaproviders embeds the team-maintained Lua provider
// scripts (the "bundled directory" of the PR116 Lua provider system):
// scripts/<id>/main.lua compiled into the binary so the Lua-backed
// roster members ship without a user setup step.
//
// The factory assembles the user-dir sources AHEAD of Sources() in
// its LoadSources precedence list (first occurrence wins: the user
// config dir → [providers.lua].dir → the bundled embeds), so a user
// script with the same id overrides a bundled one without a rebuild,
// and a bundled script shadows the compiled Go factory with the same
// id (Lua shadows Go). See internal/lua/loader.go for the loading
// rules and internal/providers/factory.go for the assembly.
package luaproviders

import (
	"embed"
	"io/fs"
	"strings"

	"github.com/an0nx/anicli-go/internal/lua"
)

//go:embed all:scripts
var scriptsFS embed.FS

// Sources projects the embedded scripts onto the loader's Source
// list (Dir = "bundled"), in lexicographic id order for determinism.
// The layout is enforced by the walk: only scripts/<id>/main.lua
// entries count — anything else in the tree is content, not a script.
func Sources() []lua.Source {
	matches, err := fs.Glob(scriptsFS, "scripts/*/main.lua")
	if err != nil {
		return nil // an ill-formed pattern is a build-time bug; scan to nothing
	}
	out := make([]lua.Source, 0, len(matches))
	for _, match := range matches {
		parts := strings.Split(match, "/")
		if len(parts) != 3 {
			continue
		}
		src, err := scriptsFS.ReadFile(match)
		if err != nil {
			continue // unreadable embedded file cannot happen; skip over risk
		}
		out = append(out, lua.Source{ID: parts[1], Src: string(src), Dir: "bundled"})
	}
	// fs.Glob already returns sorted results; the explicit id order is
	// the load precedence order's bundled segment.
	return out
}
