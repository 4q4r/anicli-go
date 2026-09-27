// Package anicli hosts the repository-root embedded assets that cannot
// live under internal/: the //go:embed directive cannot reference parent
// directories, so the bundled locale tables in locales/ are exposed here
// and handed to internal/i18n by the CLI startup.
package anicli

import "embed"

// Locales carries the bundled translation tables (locales/*.toml) inside
// the binary: en.toml is the source of truth, ru.toml the shipped
// translation. Community-contributed tables live in
// ~/.config/anicli/locales/ and never require a rebuild.
//
//go:embed locales/*.toml
var Locales embed.FS
