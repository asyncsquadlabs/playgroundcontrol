package windowsapp

import (
	"errors"
	"strings"
	"testing"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/buildinfo"
)

func withVersion(t *testing.T, version string) {
	t.Helper()
	previous := buildinfo.Version
	buildinfo.Version = version
	t.Cleanup(func() { buildinfo.Version = previous })
}

type fakeBackend struct {
	info       installationInfo
	inspectErr error
	installErr error
	resetErr   error
	startErr   error

	installModes []installMode
	resetCalls   int
	startCalls   int
}

func (f *fakeBackend) Inspect() (installationInfo, error) {
	return f.info, f.inspectErr
}

func (f *fakeBackend) Install(_ installReporter, mode installMode) error {
	f.installModes = append(f.installModes, mode)
	err := f.installErr
	f.installErr = nil
	return err
}

func (f *fakeBackend) FullReset(_ installReporter) error {
	f.resetCalls++
	err := f.resetErr
	f.resetErr = nil
	return err
}

func (f *fakeBackend) TryStart(_ installReporter) error {
	f.startCalls++
	return f.startErr
}

type fakeUI struct {
	actions      []recoveryAction
	actionIndex  int
	confirmReset bool
	success      bool
	failure      bool
	terminalSet  bool
	lines        []string
}

func (u *fakeUI) StartStep(text string)    { u.lines = append(u.lines, "STEP "+text) }
func (u *fakeUI) CompleteStep(text string) { u.lines = append(u.lines, "OK "+text) }
func (u *fakeUI) Info(text string)         { u.lines = append(u.lines, "INFO "+text) }
func (u *fakeUI) Failure(text string)      { u.lines = append(u.lines, "FAIL "+text) }
func (u *fakeUI) ServiceLine(text string)  { u.lines = append(u.lines, "SVC "+text) }
func (u *fakeUI) SetStatus(string)         {}
func (u *fakeUI) SetProgress(int, int)     {}
func (u *fakeUI) MarkSuccess()             { u.success = true; u.terminalSet = true }
func (u *fakeUI) MarkFailure()             { u.failure = true; u.terminalSet = true }
func (u *fakeUI) ClearTerminal()           { u.terminalSet = false }
func (u *fakeUI) WaitForFinish()           {}

func (u *fakeUI) AskRecovery(string) recoveryAction {
	if u.actionIndex >= len(u.actions) {
		return recoveryExit
	}
	action := u.actions[u.actionIndex]
	u.actionIndex++
	return action
}

func (u *fakeUI) ConfirmFullReset() bool { return u.confirmReset }

func TestRunInstallFlowFreshSuccess(t *testing.T) {
	backend := &fakeBackend{}
	ui := &fakeUI{}
	outcome := runInstallFlow(backend, ui, modeAuto)
	if outcome != outcomeInstalled {
		t.Fatalf("outcome = %v, want installed", outcome)
	}
	if len(backend.installModes) != 1 || backend.installModes[0] != modeFresh {
		t.Fatalf("install modes = %v, want [fresh]", backend.installModes)
	}
	if !ui.success {
		t.Fatal("success was not marked")
	}
}

func TestRunInstallFlowAlreadyHealthy(t *testing.T) {
	withVersion(t, "1.2.0")
	backend := &fakeBackend{info: installationInfo{
		ServiceRegistered: true,
		InstalledExe:      true,
		InstalledVersion:  "1.2.0",
		Healthy:           true,
	}}
	outcome := runInstallFlow(backend, &fakeUI{}, modeAuto)
	if outcome != outcomeAlreadyRunning {
		t.Fatalf("outcome = %v, want already running", outcome)
	}
	if len(backend.installModes) != 0 || backend.startCalls != 0 {
		t.Fatalf("unexpected install/start calls: %v/%d", backend.installModes, backend.startCalls)
	}
}

func TestRunInstallFlowRepairAfterFailedStart(t *testing.T) {
	withVersion(t, "1.2.0")
	backend := &fakeBackend{
		info: installationInfo{
			ServiceRegistered: true,
			InstalledExe:      true,
			InstalledVersion:  "1.2.0",
		},
		startErr: errors.New("service exited"),
	}
	ui := &fakeUI{actions: []recoveryAction{recoveryRepair}}
	outcome := runInstallFlow(backend, ui, modeAuto)
	if outcome != outcomeInstalled {
		t.Fatalf("outcome = %v, want installed", outcome)
	}
	if len(backend.installModes) != 1 || backend.installModes[0] != modeRepair {
		t.Fatalf("install modes = %v, want [repair]", backend.installModes)
	}
}

func TestRunInstallFlowFullResetAfterFailure(t *testing.T) {
	backend := &fakeBackend{installErr: errors.New("copy failed")}
	ui := &fakeUI{actions: []recoveryAction{recoveryReset}, confirmReset: true}
	outcome := runInstallFlow(backend, ui, modeAuto)
	if outcome != outcomeInstalled {
		t.Fatalf("outcome = %v, want installed", outcome)
	}
	if backend.resetCalls != 1 {
		t.Fatalf("reset calls = %d, want 1", backend.resetCalls)
	}
	if len(backend.installModes) != 2 || backend.installModes[0] != modeFresh || backend.installModes[1] != modeFresh {
		t.Fatalf("install modes = %v, want [fresh fresh]", backend.installModes)
	}
}

func TestRunInstallFlowFullResetCanceled(t *testing.T) {
	backend := &fakeBackend{installErr: errors.New("copy failed")}
	ui := &fakeUI{actions: []recoveryAction{recoveryReset}, confirmReset: false}
	outcome := runInstallFlow(backend, ui, modeAuto)
	if outcome != outcomeCanceled {
		t.Fatalf("outcome = %v, want canceled", outcome)
	}
	if backend.resetCalls != 0 {
		t.Fatalf("reset was not canceled: %d", backend.resetCalls)
	}
}

func TestRunInstallFlowRefusesDowngrade(t *testing.T) {
	withVersion(t, "1.2.0")
	backend := &fakeBackend{info: installationInfo{
		ServiceRegistered: true,
		InstalledExe:      true,
		InstalledVersion:  "1.3.0",
		Healthy:           true,
	}}
	ui := &fakeUI{}
	outcome := runInstallFlow(backend, ui, modeAuto)
	if outcome != outcomeError {
		t.Fatalf("outcome = %v, want error", outcome)
	}
	if !ui.failure {
		t.Fatal("downgrade failure was not shown")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"1.2.0", "1.2.0", 0},
		{"1.2.1", "1.2.0", 1},
		{"1.1.0", "1.2.0", -1},
		{"v1.2.0", "1.2", 0},
		{"1.10.0", "1.9.9", 1},
		{"dev", "1.2.0", 0},
		{"", "1.2.0", 0},
	}
	for _, test := range cases {
		if got := compareVersions(test.left, test.right); got != test.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestProgressModelRetainsRecentLinesAndTerminalState(t *testing.T) {
	model := newProgressModel("1.2.0")
	model.StartStep("one")
	model.CompleteStep("one")
	model.Failure("bad")
	model.MarkAwaitingChoice()
	snapshot := model.Snapshot()
	if len(snapshot.Lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(snapshot.Lines))
	}
	if !snapshot.Terminal || !snapshot.AwaitingChoice {
		t.Fatalf("state = terminal %t awaiting %t", snapshot.Terminal, snapshot.AwaitingChoice)
	}
	if !strings.Contains(snapshot.Lines[2], "bad") {
		t.Fatalf("failure line missing: %v", snapshot.Lines)
	}
	model.ClearTerminal()
	if model.Snapshot().Terminal || model.Snapshot().AwaitingChoice {
		t.Fatal("terminal state was not cleared")
	}
}
