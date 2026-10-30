package browsercontrol

import (
	"errors"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

type fakeState struct {
	allowed  bool
	settings model.BrowserSettings
	domains  []string
	err      error
}

func (f *fakeState) PlayAllowed() bool                      { return f.allowed }
func (f *fakeState) BrowserSettings() model.BrowserSettings { return f.settings }
func (f *fakeState) BrowserBlockedDomains() ([]string, error) {
	return append([]string(nil), f.domains...), f.err
}

type fakeApplier struct {
	activeCalls []bool
	domainCalls [][]string
	status      model.BrowserStatus
	err         error
	restored    bool
}

func (f *fakeApplier) Sync(active bool, domains []string) (model.BrowserStatus, error) {
	f.activeCalls = append(f.activeCalls, active)
	f.domainCalls = append(f.domainCalls, append([]string(nil), domains...))
	if f.err != nil {
		return f.status, f.err
	}
	status := f.status
	status.Active = active
	if active {
		status.URLBlocklistApplied = true
		status.HostsApplied = true
		status.DoHDisabled = true
	}
	return status, nil
}
func (f *fakeApplier) Restore() error              { f.restored = true; return nil }
func (f *fakeApplier) Status() model.BrowserStatus { return f.status }

type fakeAuditor struct{ events []model.AuditEvent }

func (f *fakeAuditor) AddEvent(event model.AuditEvent) error {
	f.events = append(f.events, event)
	return nil
}

func TestManagerAppliesOnlyWhileBlockedAndEnabled(t *testing.T) {
	state := &fakeState{
		settings: model.BrowserSettings{Enabled: true, AutoBlock: true},
		domains:  []string{"poki.com", "crazygames.com"},
	}
	applier := &fakeApplier{status: model.BrowserStatus{Browsers: []string{"chrome"}}}
	manager := NewManager(state, applier, &fakeAuditor{}, Options{Now: time.Now})

	if err := manager.Sync(); err != nil {
		t.Fatal(err)
	}
	if len(applier.activeCalls) != 1 || !applier.activeCalls[0] {
		t.Fatalf("active calls = %v", applier.activeCalls)
	}
	if len(applier.domainCalls[0]) != 2 {
		t.Fatalf("domains = %v", applier.domainCalls[0])
	}
	status := manager.Status()
	if !status.Active || !status.URLBlocklistApplied || !status.HostsApplied || !status.DoHDisabled {
		t.Fatalf("status = %+v", status)
	}

	state.allowed = true
	if err := manager.Sync(); err != nil {
		t.Fatal(err)
	}
	if applier.activeCalls[1] {
		t.Fatalf("controls remained active while play was allowed: %v", applier.activeCalls)
	}

	state.allowed = false
	state.settings.Enabled = false
	if err := manager.Sync(); err != nil {
		t.Fatal(err)
	}
	if applier.activeCalls[2] {
		t.Fatalf("controls remained active while monitoring was disabled: %v", applier.activeCalls)
	}
}

func TestManagerAuditsDriftAndErrors(t *testing.T) {
	auditor := &fakeAuditor{}
	state := &fakeState{settings: model.BrowserSettings{Enabled: true}, domains: []string{"poki.com"}}
	applier := &fakeApplier{status: model.BrowserStatus{
		Browsers: []string{"chrome"}, DriftDetected: true,
	}}
	manager := NewManager(state, applier, auditor, Options{Now: time.Now})
	if err := manager.Sync(); err != nil {
		t.Fatal(err)
	}
	if len(auditor.events) == 0 || auditor.events[0].Result != "browser-controls-drift" {
		t.Fatalf("drift audit missing: %+v", auditor.events)
	}

	applier.err = errors.New("hosts locked")
	err := manager.Sync()
	if err == nil {
		t.Fatal("expected sync error")
	}
	if manager.Status().LastError == "" {
		t.Fatal("status did not retain the sync error")
	}
}
