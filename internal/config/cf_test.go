package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCFDefaults(t *testing.T) {
	s := Default()
	if s.CF.Enabled {
		t.Error("cf.enabled must default to false (opt-in, zero behavior change)")
	}
	if !s.CF.Headed {
		t.Error("cf.headed must default to true (interactive challenges need a visible browser)")
	}
	if s.CF.SolveTimeout != 90*time.Second {
		t.Errorf("cf.solve_timeout default = %v, want 90s", s.CF.SolveTimeout)
	}
	if !s.CF.AutoUpdate {
		t.Error("cf.auto_update must default to true")
	}
	if s.CF.UpdateInterval != 30*time.Minute {
		t.Errorf("cf.update_interval default = %v, want 30m", s.CF.UpdateInterval)
	}
}

func TestCFFileOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.toml")
	content := `
[cf]
enabled = true
headed = false
solve_timeout = "2m"
auto_update = false
update_interval = "1h"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !s.CF.Enabled || s.CF.Headed {
		t.Errorf("cf flags: %+v", s.CF)
	}
	if s.CF.SolveTimeout != 2*time.Minute {
		t.Errorf("solve_timeout = %v", s.CF.SolveTimeout)
	}
	if s.CF.AutoUpdate {
		t.Error("auto_update = true, want file value false")
	}
	if s.CF.UpdateInterval != time.Hour {
		t.Errorf("update_interval = %v", s.CF.UpdateInterval)
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
