package windowsapp

import (
	"errors"
	"testing"
)

type fakeCleanupOps struct {
	calls []string
	errs  map[string]error
}

func (f *fakeCleanupOps) record(name string) error {
	f.calls = append(f.calls, name)
	return f.errs[name]
}

func (f *fakeCleanupOps) StopService() error            { return f.record("stop") }
func (f *fakeCleanupOps) DeleteService() error          { return f.record("delete") }
func (f *fakeCleanupOps) RemoveRobloxGuard() error      { return f.record("guard") }
func (f *fakeCleanupOps) RestoreBrowserControls() error { return f.record("browser") }
func (f *fakeCleanupOps) RemoveFirewallRule() error     { return f.record("firewall") }
func (f *fakeCleanupOps) RemoveUninstallEntry() error   { return f.record("uninstall") }
func (f *fakeCleanupOps) RemoveEventLogSource() error   { return f.record("eventlog") }
func (f *fakeCleanupOps) RemoveInstallFiles() error     { return f.record("files") }
func (f *fakeCleanupOps) RemoveDataDirectory() error    { return f.record("data") }

func TestRunCleanupRepairPreservesDataAndFiles(t *testing.T) {
	ops := &fakeCleanupOps{}
	if err := runCleanup(ops, cleanupOptions{}, noopReporter{}); err != nil {
		t.Fatal(err)
	}
	for _, call := range ops.calls {
		if call == "data" || call == "files" {
			t.Fatalf("repair cleanup removed %s", call)
		}
	}
	if len(ops.calls) != 7 {
		t.Fatalf("calls = %v, want seven component removals", ops.calls)
	}
}

func TestRunCleanupFullResetRemovesEverythingAndContinuesAfterErrors(t *testing.T) {
	ops := &fakeCleanupOps{errs: map[string]error{
		"stop":     errors.New("stop failed"),
		"firewall": errors.New("firewall failed"),
	}}
	err := runCleanup(ops, cleanupOptions{RemoveData: true, RemoveInstallFile: true}, noopReporter{})
	if err == nil {
		t.Fatal("expected joined cleanup error")
	}
	if len(ops.calls) != 9 {
		t.Fatalf("calls = %v, want all nine cleanup steps", ops.calls)
	}
	last := ops.calls[len(ops.calls)-1]
	if last != "data" {
		t.Fatalf("last call = %s, want data", last)
	}
}

func TestRunCleanupRetainsDataWhenBrowserRestoreFails(t *testing.T) {
	ops := &fakeCleanupOps{errs: map[string]error{
		"browser": errors.New("hosts locked"),
	}}
	err := runCleanup(ops, cleanupOptions{RemoveData: true}, noopReporter{})
	if err == nil {
		t.Fatal("expected cleanup error")
	}
	for _, call := range ops.calls {
		if call == "data" {
			t.Fatal("application data was removed after a browser restore failure")
		}
	}
}
