// Package browsercontrol applies and removes browser enforcement controls for
// the persisted blocked-domain list. It owns reconciliation and the dashboard
// status view.
package browsercontrol

import (
	"context"
	"sync"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	// DefaultReconcileInterval is how often policies and hosts are verified.
	DefaultReconcileInterval = 60 * time.Second
)

// State supplies the authoritative permission state and blocked-domain list.
type State interface {
	PlayAllowed() bool
	BrowserSettings() model.BrowserSettings
	BrowserBlockedDomains() ([]string, error)
}

// Applier applies desired controls. Implementations must be idempotent and safe
// for serial calls.
type Applier interface {
	Sync(active bool, domains []string) (model.BrowserStatus, error)
	Restore() error
	Status() model.BrowserStatus
}

// Auditor stores enforcement drift and failure events.
type Auditor interface {
	AddEvent(model.AuditEvent) error
}

// Options configures a Manager.
type Options struct {
	ReconcileInterval time.Duration
	Now               func() time.Time
}

// Manager reconciles browser controls and reports their status.
type Manager struct {
	state    State
	applier  Applier
	auditor  Auditor
	interval time.Duration
	now      func() time.Time

	wake chan struct{}

	syncMu sync.Mutex
	mu     sync.RWMutex
	status model.BrowserStatus
}

// NewManager builds a browser-control manager.
func NewManager(state State, applier Applier, auditor Auditor, options Options) *Manager {
	if options.ReconcileInterval <= 0 {
		options.ReconcileInterval = DefaultReconcileInterval
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	m := &Manager{
		state:    state,
		applier:  applier,
		auditor:  auditor,
		interval: options.ReconcileInterval,
		now:      options.Now,
		wake:     make(chan struct{}, 1),
		status:   model.BrowserStatus{Browsers: []string{}},
	}
	if applier != nil {
		m.status = applier.Status()
		if m.status.Browsers == nil {
			m.status.Browsers = []string{}
		}
	}
	return m
}

// Wake requests an immediate reconciliation.
func (m *Manager) Wake() {
	if m == nil {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run reconciles until the context is canceled.
func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	_ = m.Sync()
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.Sync()
		case <-m.wake:
			_ = m.Sync()
		}
	}
}

// Sync computes the desired controls from the authoritative state and applies
// them. It is safe for concurrent callers.
func (m *Manager) Sync() error {
	if m == nil || m.state == nil || m.applier == nil {
		return nil
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	settings := m.state.BrowserSettings()
	playAllowed := m.state.PlayAllowed()
	domains, err := m.state.BrowserBlockedDomains()
	if err != nil {
		m.setStatus(m.applier.Status(), settings.Enabled, "Unable to read blocked browser domains: "+err.Error())
		return err
	}
	active := settings.Enabled && !playAllowed && len(domains) > 0
	desired := []string(nil)
	if active {
		desired = domains
	}
	status, syncErr := m.applier.Sync(active, desired)
	status.MonitoringEnabled = settings.Enabled
	status.Active = active
	if status.Browsers == nil {
		status.Browsers = []string{}
	}
	if syncErr != nil {
		status.LastError = syncErr.Error()
		m.setStatus(status, settings.Enabled, syncErr.Error())
		m.audit(model.AuditEvent{
			Time:    m.now().UTC(),
			Type:    model.EventBrowser,
			Message: "Browser controls could not be applied: " + syncErr.Error(),
			Result:  "browser-controls-failed",
		})
		return syncErr
	}
	if status.DriftDetected {
		m.audit(model.AuditEvent{
			Time:    m.now().UTC(),
			Type:    model.EventBrowser,
			Message: "Browser policies or hosts entries were modified outside Playground Control and were repaired.",
			Result:  "browser-controls-drift",
		})
	}
	m.setStatus(status, settings.Enabled, "")
	return nil
}

// Status returns a copy of the latest enforcement status.
func (m *Manager) Status() model.BrowserStatus {
	if m == nil {
		return model.BrowserStatus{Browsers: []string{}}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := m.status
	status.Browsers = append([]string(nil), m.status.Browsers...)
	if status.Browsers == nil {
		status.Browsers = []string{}
	}
	return status
}

// Restore removes Playground Control browser controls and restores originally
// saved policy values and hosts content.
func (m *Manager) Restore() error {
	if m == nil || m.applier == nil {
		return nil
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	return m.applier.Restore()
}

func (m *Manager) setStatus(status model.BrowserStatus, monitoringEnabled bool, lastError string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status.MonitoringEnabled = monitoringEnabled
	status.LastError = lastError
	if status.Browsers == nil {
		status.Browsers = []string{}
	}
	m.status = status
}

func (m *Manager) audit(event model.AuditEvent) {
	if m.auditor == nil {
		return
	}
	_ = m.auditor.AddEvent(event)
}
