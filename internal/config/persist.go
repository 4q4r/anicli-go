package config

// Settings persistence for secrets that mutate at runtime (PR25: the
// Shikimori OAuth token refresh; PR32: comment-preserving rewrite).
// The file is updated read-modify-write: load (defaults + file, no
// environment overrides — an env secret must never get baked into the
// file), mutate the one section, validate, then rewrite ONLY the
// [shikimori] section's key-value lines on a line-by-line basis so
// every other section, every comment and the overall file ordering
// survive verbatim. The write itself is atomic (temp file + rename)
// so a crash mid-write can never truncate the previous file.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// settingsFilePerm is the settings file mode: the file carries session
// cookies, OAuth tokens and password hashes.
const settingsFilePerm = 0o600

// shikimoriSectionName is the TOML table header of the section this
// persistence path owns.
const shikimoriSectionName = "shikimori"

// UpdateShikimori loads the settings file at path (defaults when the
// file is missing), applies mutate to the [shikimori] section and
// writes the result back atomically, replacing only the section's
// key-value lines — user comments and file ordering survive. A
// malformed or unknown-key file fails loud and is left untouched; the
// caller surfaces the error.
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
	return writeShikimoriSection(path, &s.Shikimori)
}

// writeShikimoriSection rewrites the file at path so its [shikimori]
// section carries section, preserving everything else. A missing file
// is created fresh with just the section.
func writeShikimoriSection(path string, section *Shikimori) error {
	var body string
	// G304: path is the caller-resolved settings file (ResolveConfigPath
	// or --config), already validated by applyFile — not user input.
	data, err := os.ReadFile(path) //nolint:gosec // settings path, see above
	switch {
	case err == nil:
		body = rewriteShikimoriSection(string(data), section)
	case os.IsNotExist(err):
		body = encodeShikimoriSection(section)
	default:
		return fmt.Errorf("rewrite settings %s: %w", path, err)
	}
	return writeSettingsAtomic(path, []byte(body))
}

// rewriteShikimoriSection replaces the [shikimori] section's key-value
// lines inside src, keeping:
//   - every line OUTSIDE the section verbatim (other sections, their
//     comments, their key order);
//   - standalone comment lines INSIDE the section (moved above the
//     fresh key block; inline comments on replaced key lines are lost);
//   - the section ordering.
//
// A missing section is appended at the end of the file.
func rewriteShikimoriSection(src string, section *Shikimori) string {
	return rewriteTOMLSection(src, shikimoriSectionName, shikimoriKeyLines(section))
}

// rewriteTOMLSection is the comment-preserving section rewrite shared
// by every runtime-mutated section ([shikimori] PR32, [mal] PR112):
// replace name's key-value lines with keyLines, keep standalone section
// comments above the fresh block, keep every other line and the section
// ordering verbatim, append the section at the end when absent.
func rewriteTOMLSection(src, name string, keyLines []string) string {
	lines := strings.Split(src, "\n")

	start := -1
	for i, line := range lines {
		if sectionHeaderName(line) == name {
			start = i
			break
		}
	}

	// No section yet: keep the file verbatim, append the fresh section
	// behind a blank separator.
	if start == -1 {
		out := src
		if out != "" {
			out = strings.TrimRight(out, "\n") + "\n\n"
		}
		return out + "[" + name + "]\n" + strings.Join(keyLines, "\n")
	}

	// The section body reaches until the next table header line.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if isTableHeader(lines[i]) {
			end = i
			break
		}
	}

	// Standalone comments survive, above the fresh keys.
	var kept []string
	for _, line := range lines[start+1 : end] {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}

	var out []string
	out = append(out, lines[:start]...)
	out = append(out, "["+name+"]")
	out = append(out, kept...)
	out = append(out, keyLines...)
	if end < len(lines) {
		// Another section follows: keep it separated by one blank line.
		out = append(out, "")
		out = append(out, lines[end:]...)
	} else {
		out = append(out, "") // exactly one trailing newline
	}
	return strings.Join(out, "\n")
}

// isShikimoriHeader reports whether the line is the [shikimori] table
// header (a trailing comment on the line is tolerated; sub-tables like
// [shikimori.x] do not match).
func isShikimoriHeader(line string) bool {
	return sectionHeaderName(line) == shikimoriSectionName
}

// isTableHeader reports whether the line opens any table
// ([section], [a.b] or [[array]]).
func isTableHeader(line string) bool {
	return sectionHeaderName(line) != ""
}

// sectionHeaderName extracts the table name from a [name] header line
// ("" when the line is not a table header). Everything after the
// closing bracket — a trailing comment — is ignored.
func sectionHeaderName(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	rest := trimmed[1:]
	close := strings.Index(rest, "]")
	if close < 1 {
		return ""
	}
	return strings.TrimSpace(rest[:close])
}

// encodeShikimoriSection renders the section as a standalone TOML
// document ("[shikimori]\nkey = …\n…") via the real encoder, so value
// quoting matches a full encode exactly.
func encodeShikimoriSection(section *Shikimori) string {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(struct {
		Shikimori Shikimori `toml:"shikimori"`
	}{*section}); err != nil {
		// A struct of scalars cannot fail to encode; panic-guard only.
		return "[" + shikimoriSectionName + "]\n"
	}
	return buf.String()
}

// shikimoriKeyLines renders just the section's key-value lines (the
// encoder output minus its header line).
func shikimoriKeyLines(section *Shikimori) []string {
	encoded := strings.Split(strings.TrimSuffix(encodeShikimoriSection(section), "\n"), "\n")
	if len(encoded) > 0 && isShikimoriHeader(encoded[0]) {
		return encoded[1:]
	}
	return encoded
}

// writeSettingsAtomic installs data as the settings file with a
// same-directory temp file + rename: readers observe either the old or
// the new complete file, never a partial write.
func writeSettingsAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".settings-*.toml")
	if err != nil {
		return fmt.Errorf("create temp settings in %q: %w", dir, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp settings: %w", err)
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

// malSectionName is the TOML table header of the [mal] section.
const malSectionName = "mal"

// UpdateMAL loads the settings file at path (defaults when the file is
// missing), applies mutate to the [mal] section and writes the result
// back atomically, replacing only the section's key-value lines (PR112)
// — user comments and file ordering survive; the [shikimori] section is
// untouched, so both trackers stay authenticated independently. A
// malformed or unknown-key file fails loud and is left untouched.
func UpdateMAL(path string, mutate func(*MAL)) error {
	if mutate == nil {
		return fmt.Errorf("config: UpdateMAL: nil mutator")
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

	mutate(&s.MAL)

	if err := s.Validate(); err != nil {
		return fmt.Errorf("rewrite settings %s: %w", path, err)
	}
	return writeMALSection(path, &s.MAL)
}

// writeMALSection rewrites the file at path so its [mal] section carries
// section, preserving everything else. A missing file is created fresh
// with just the section.
func writeMALSection(path string, section *MAL) error {
	var body string
	// G304: path is the caller-resolved settings file (ResolveConfigPath
	// or --config), already validated by applyFile — not user input.
	data, err := os.ReadFile(path) //nolint:gosec // settings path, see above
	switch {
	case err == nil:
		body = rewriteTOMLSection(string(data), malSectionName, malKeyLines(section))
	case os.IsNotExist(err):
		body = encodeMALSection(section)
	default:
		return fmt.Errorf("rewrite settings %s: %w", path, err)
	}
	return writeSettingsAtomic(path, []byte(body))
}

// encodeMALSection renders the section as a standalone TOML document
// via the real encoder, so value quoting matches a full encode exactly.
func encodeMALSection(section *MAL) string {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(struct {
		MAL MAL `toml:"mal"`
	}{*section}); err != nil {
		// A struct of scalars cannot fail to encode; panic-guard only.
		return "[" + malSectionName + "]\n"
	}
	return buf.String()
}

// malKeyLines renders just the section's key-value lines (the encoder
// output minus its header line).
func malKeyLines(section *MAL) []string {
	encoded := strings.Split(strings.TrimSuffix(encodeMALSection(section), "\n"), "\n")
	if len(encoded) > 0 && sectionHeaderName(encoded[0]) == malSectionName {
		return encoded[1:]
	}
	return encoded
}
