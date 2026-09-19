package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCFDefaults(t *testing.T) {
	s := Default()
	if s.CF.Enabled {
		t.Error("cf.enabled must default to false (opt-in, zero behavior change)")
	}
	if s.CF.SolveTimeout != 90*time.Second {
		t.Errorf("cf.solve_timeout default = %v, want 90s", s.CF.SolveTimeout)
	}
	if s.CF.BrowserIdleTimeout != 15*time.Second {
		t.Errorf("cf.browser_idle_timeout default = %v, want 15s", s.CF.BrowserIdleTimeout)
	}
	if !s.CF.AutoUpdate {
		t.Error("cf.auto_update must default to true")
	}
	if s.CF.UpdateInterval != 30*time.Minute {
		t.Errorf("cf.update_interval default = %v, want 30m", s.CF.UpdateInterval)
	}
	if s.CF.Channel != "auto" {
		t.Errorf("cf.channel default = %q, want auto (free base + license upgrade, PR73)", s.CF.Channel)
	}
}

func TestCFFileOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	content := `
[cf]
enabled = true
solve_timeout = "2m"
browser_idle_timeout = "5s"
auto_update = false
update_interval = "1h"
channel = "free"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !s.CF.Enabled {
		t.Errorf("cf.enabled: %+v", s.CF)
	}
	if s.CF.SolveTimeout != 2*time.Minute {
		t.Errorf("solve_timeout = %v", s.CF.SolveTimeout)
	}
	if s.CF.BrowserIdleTimeout != 5*time.Second {
		t.Errorf("browser_idle_timeout = %v", s.CF.BrowserIdleTimeout)
	}
	if s.CF.AutoUpdate {
		t.Error("auto_update = true, want file value false")
	}
	if s.CF.UpdateInterval != time.Hour {
		t.Errorf("update_interval = %v", s.CF.UpdateInterval)
	}
	if s.CF.Channel != "free" {
		t.Errorf("channel = %q, want file value free", s.CF.Channel)
	}
}

// TestCFZeroIdleTimeoutIsImmediate pins the "0s = close right after
// the last solve" contract: the value must round-trip as zero, not be
// reinterpreted as "unset".
func TestCFZeroIdleTimeoutIsImmediate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[cf]\nbrowser_idle_timeout = \"0s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if s.CF.BrowserIdleTimeout != 0 {
		t.Errorf("browser_idle_timeout = %v, want 0 (immediate close)", s.CF.BrowserIdleTimeout)
	}
}

// TestCFChannelValidation pins the channel contract: the three
// documented values load; anything else fails loud at startup
// instead of surfacing as a mid-run resolution surprise.
func TestCFChannelValidation(t *testing.T) {
	for _, channel := range []string{"auto", "free", "pro"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.toml")
		content := "[cf]\nchannel = \"" + channel + "\"\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Load(path)
		if err != nil {
			t.Fatalf("channel %q: load: %v", channel, err)
		}
		if s.CF.Channel != channel {
			t.Errorf("channel = %q, want %q", s.CF.Channel, channel)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[cf]\nchannel = \"banana\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "cf.channel") {
		t.Fatalf("err = %v, want a loud cf.channel validation error", err)
	}
}

// TestCFHeadedKeyRejected guards the headless-only ruling: the headed
// key shipped in-repo for hours and is gone; leftovers in user files
// must fail loud under the strict unknown-key contract, not silently
// keep a dead toggle alive.
func TestCFHeadedKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[cf]\nheaded = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("cf.headed must fail loud as an unknown key (headless-only)")
	}
}

func TestCFUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	if err := os.WriteFile(path, []byte("[cf]\nnope = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("unknown cf key must fail loud")
	}
}
