package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// XDG environment names.
const (
	EnvXDGConfigHome = "XDG_CONFIG_HOME"
	EnvXDGDataHome   = "XDG_DATA_HOME"
)

// ResolveConfigPath returns the settings file path to use:
// explicit argument > $ANICLI_CONFIG > $XDG_CONFIG_HOME/anicli/settings.toml
// > ~/.config/anicli/settings.toml. The returned path may not exist; Load
// treats a missing file as defaults.
func ResolveConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v, ok := lookupEnv(EnvConfigPath); ok {
		return v
	}
	if xdg, ok := lookupEnv(EnvXDGConfigHome); ok {
		return filepath.Join(xdg, AppName, SettingsFileName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home resolvable: no candidate path; Load will use defaults.
		return ""
	}
	return filepath.Join(home, ".config", AppName, SettingsFileName)
}

// DataDir returns the writable data directory, creating it if missing:
// $ANICLI_DATA > $XDG_DATA_HOME/anicli > ~/.local/share/anicli.
func DataDir() (string, error) {
	if v, ok := lookupEnv(EnvDataDir); ok {
		if err := mkdir(v); err != nil {
			return "", err
		}
		return v, nil
	}
	if xdg, ok := lookupEnv(EnvXDGDataHome); ok {
		dir := filepath.Join(xdg, AppName)
		if err := mkdir(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".local", "share", AppName)
	if err := mkdir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func mkdir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data dir %q: %w", dir, err)
	}
	return nil
}
