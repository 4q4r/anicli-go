//go:build !windows

package cfbrowser

import (
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// TestSetNewProcessGroupNoParentDeathSignal pins the PR18 fix:
// setNewProcessGroup must NOT arm PR_SET_PDEATHSIG.
//
// prctl(2) ties the death signal to the forking THREAD, not to the
// parent process — and the Go runtime migrates goroutines between OS
// threads and retires them freely (nothing holds the spawning thread
// alive for the child's lifetime). The kernel therefore SIGKILLs a
// Pdeathsig'd browser at an arbitrary moment seconds after launch:
// live-verified as every solve since PR14 navigating to a dead
// process ("context canceled" on the first Navigate).
//
// The race itself is probabilistic and cannot be forced from a unit
// test; this call-shape assertion is the regression pin that the
// footgun cannot be silently reintroduced. On platforms whose
// SysProcAttr has no Pdeathsig field the field check is vacuous and
// skipped (the field cannot be set there anyway).
func TestSetNewProcessGroupNoParentDeathSignal(t *testing.T) {
	cmd := &exec.Cmd{}
	setNewProcessGroup(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("setNewProcessGroup left SysProcAttr nil")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("SysProcAttr.Setpgid = false, want true (group-kill-on-Close depends on it)")
	}

	v := reflect.ValueOf(*cmd.SysProcAttr).FieldByName("Pdeathsig")
	if !v.IsValid() {
		t.Skip("SysProcAttr has no Pdeathsig field on this platform")
	}
	if got := v.Int(); got != 0 {
		t.Errorf("SysProcAttr.Pdeathsig = %d, want 0 (unset): "+
			"PDEATHSIG is thread-scoped (prctl(2)) and kills healthy browsers "+
			"when the Go runtime retires the forking thread — see prockill_unix.go", got)
	}
}

// TestSetNewProcessGroupChildSurvives is a real-spawn canary for the
// PR18 Pdeathsig race: a child spawned through setNewProcessGroup must
// still be alive two seconds later. A quiet test binary cannot force
// the runtime to retire the forking thread, so this cannot
// deterministically reproduce the historical failure — it pins the
// helper's happy-path contract and fails loudly if a reintroduced
// kernel death signal happens to fire under it. Skipped in -short.
func TestSetNewProcessGroupChildSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("real-spawn integration test, skipped in -short")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary on PATH: %v", err)
	}

	cmd := exec.Command(sleep, "30") //nolint:gosec // test-resolved sleep(1), not request input
	setNewProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", sleep, err)
	}
	defer func() {
		_ = killProcessGroup(cmd)
		_ = cmd.Wait()
	}()

	time.Sleep(2 * time.Second)

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child died within 2s of spawn (PR18 Pdeathsig failure signature): %v", err)
	}
}
