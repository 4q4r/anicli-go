package config

// Settings persistence for secrets that mutate at runtime (PR25: the
// Shikimori OAuth token refresh). The file is updated read-modify-write:
// load (defaults + file, no environment overrides — an env secret must
// never get baked into the file), mutate the one section, validate, then
// write the whole settings back atomically (temp file + rename) so a
// crash mid-write can never truncate the previous file.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// settingsFilePerm is the settings file mode: the file carries session
// cookies, OAuth tokens and password hashes.
const settingsFilePerm = 0o600

// UpdateShikimori loads the settings file at path (defaults when the
// file is missing), applies mutate to the [shikimori] section and writes
// the result back atomically. A malformed or unknown-key file fails
// loud and is left untouched; the caller surfaces the error.
func UpdateShikimori(path string, mutate func(*Shikimori)) error {
	if mutate == nil {
		return fmt.Errorf("config: UpdateShikimori: nil mutator")
	}

	s := Default()
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := applyFile(path, &s); err != nil {
				return fmt.Errorf("rewrite settings %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat settings %s: %w", path, err)
		}
	}

	mutate(&s.Shikimori)

	if err := s.Validate(); err != nil {
		return fmt.Errorf("rewrite settings %s: %w", path, err)
	}
	return writeSettingsAtomic(path, &s)
}

// writeSettingsAtomic encodes the settings and installs them with a
// same-directory temp file + rename: readers observe either the old or
// the new complete file, never a partial write.
func writeSettingsAtomic(path string, s *Settings) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".settings-*.toml")
	if err != nil {
		return fmt.Errorf("create temp settings in %q: %w", dir, err)
	}
	tmpName := tmp.Name()

	if err := toml.NewEncoder(tmp).Encode(s); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("encode settings: %w", err)
	}
	if err := tmp.Chmod(settingsFilePerm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod temp settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp settings: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install settings %s: %w", path, err)
	}
	return nil
}
