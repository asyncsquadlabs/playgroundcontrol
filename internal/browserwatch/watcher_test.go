package browserwatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/deepseek"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

type fakeHistory struct {
	visits []Visit
	calls  int
	err    error
}

func (f *fakeHistory) RecentVisits(context.Context, time.Time) ([]Visit, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]Visit(nil), f.visits...), nil
}

type fakeState struct {
	allowed  bool
	settings model.BrowserSettings
}

func (f *fakeState) PlayAllowed() bool                      { return f.allowed }
func (f *fakeState) BrowserSettings() model.BrowserSettings { return f.settings }

type fakeSettings struct {
	settings model.AISettings
	key      string
	ok       bool
}

func (f *fakeSettings) AICredentials() (model.AISettings, string, bool) {
	return f.settings, f.key, f.ok
}

type fakeClassifier struct {
	verdicts []deepseek.DomainVerdict
	calls    int
	err      error
}

func (f *fakeClassifier) ClassifyDomains(_ context.Context, _, _ string, observations []deepseek.DomainObservation) ([]deepseek.DomainVerdict, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]deepseek.DomainVerdict(nil), f.verdicts...), nil
}

type fakeRecorder struct {
	domains    map[string]model.BrowserDomain
	seeds      []string
	ai         []string
	detections []model.BrowserDetection
	lastError  string
	statusAt   time.Time
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{domains: make(map[string]model.BrowserDomain)}
}

func (f *fakeRecorder) BrowserDomainByDomain(domain string) (model.BrowserDomain, bool, error) {
	item, ok := f.domains[domain]
	return item, ok, nil
}

func (f *fakeRecorder) RecordBrowserSeed(domain, _ string, now time.Time) (model.BrowserDomain, bool, error) {
	f.seeds = append(f.seeds, domain)
	if existing, ok := f.domains[domain]; ok {
		if existing.Status == model.BrowserStatusAllowed {
			return existing, false, nil
		}
		if existing.Status == model.BrowserStatusBlocked {
			existing.SeenCount++
			f.domains[domain] = existing
			return existing, false, nil
		}
	}
	item := model.BrowserDomain{
		ID: domain, Domain: domain, Source: model.BrowserSourceSeed,
		Status: model.BrowserStatusBlocked, Confidence: 1,
		FirstSeen: now, LastSeen: now, SeenCount: 1, UpdatedAt: now,
	}
	f.domains[domain] = item
	return item, true, nil
}

func (f *fakeRecorder) RecordBrowserAIVerdict(domain string, confidence float64, reason, modelName string, autoBlock bool, now time.Time) (model.BrowserDomain, string, bool, error) {
	f.ai = append(f.ai, domain)
	status := model.BrowserStatusReview
	action := "review"
	changed := false
	if confidence >= model.BrowserAutoBlockConfidence && autoBlock {
		status = model.BrowserStatusBlocked
		action = "ai-auto-blocked"
		changed = true
	}
	item := model.BrowserDomain{
		ID: domain, Domain: domain, Source: model.BrowserSourceAI, Status: status,
		Confidence: confidence, Reason: reason, Model: modelName,
		FirstSeen: now, LastSeen: now, SeenCount: 1, UpdatedAt: now,
	}
	f.domains[domain] = item
	return item, action, changed, nil
}

func (f *fakeRecorder) RecordBrowserDetection(detection model.BrowserDetection) error {
	f.detections = append(f.detections, detection)
	return nil
}

func (f *fakeRecorder) SetBrowserScanStatus(now time.Time, message string) error {
	f.statusAt = now
	f.lastError = message
	return nil
}

type fakeController struct{ wakes int }

func (f *fakeController) Wake() { f.wakes++ }

type fakeAuditor struct{ events []model.AuditEvent }

func (f *fakeAuditor) AddEvent(event model.AuditEvent) error {
	f.events = append(f.events, event)
	return nil
}

func blockedWatchState() model.BrowserSettings {
	return model.BrowserSettings{Enabled: true, AutoBlock: true}
}

func TestWatcherBlocksSeedDomain(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{
		{Browser: "chrome", Domain: "www.poki.com", VisitedAt: now},
		{Browser: "edge", Domain: "google.com", VisitedAt: now},
	}}
	recorder := newFakeRecorder()
	controller := &fakeController{}
	auditor := &fakeAuditor{}
	watcher := New(history, &fakeState{settings: blockedWatchState()},
		&fakeSettings{}, &fakeClassifier{}, recorder, controller, auditor, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)

	if len(recorder.seeds) != 1 || recorder.seeds[0] != "poki.com" {
		t.Fatalf("seed calls = %v", recorder.seeds)
	}
	if _, ok := recorder.domains["google.com"]; ok {
		t.Fatal("non-game domain was stored")
	}
	if controller.wakes != 1 {
		t.Fatalf("controller wakes = %d", controller.wakes)
	}
	if len(recorder.detections) != 1 || recorder.detections[0].Action != "seed-blocked" {
		t.Fatalf("detections = %+v", recorder.detections)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("audit events = %+v", auditor.events)
	}
}

func TestWatcherUsesAIForUnknownDomainsAndCaches(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{{Browser: "chrome", Domain: "slither.io", VisitedAt: now}}}
	recorder := newFakeRecorder()
	classifier := &fakeClassifier{verdicts: []deepseek.DomainVerdict{{
		Domain: "slither.io", IsGame: true, Name: "Slither", Confidence: 0.95, Reason: "Browser game.",
	}}}
	controller := &fakeController{}
	watcher := New(history, &fakeState{settings: model.BrowserSettings{Enabled: true, AutoBlock: true, AIClassification: true}},
		&fakeSettings{settings: model.AISettings{Model: "deepseek-flash"}, key: "sk-test", ok: true},
		classifier, recorder, controller, &fakeAuditor{}, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)

	if classifier.calls != 1 || len(recorder.ai) != 1 {
		t.Fatalf("classifier calls = %d, ai recordings = %v", classifier.calls, recorder.ai)
	}
	if recorder.domains["slither.io"].Status != model.BrowserStatusBlocked {
		t.Fatalf("domain status = %s", recorder.domains["slither.io"].Status)
	}
	if controller.wakes != 1 {
		t.Fatalf("controller wakes = %d", controller.wakes)
	}

	// The overlapping cached visit is not reprocessed, and the already-blocked
	// domain is not sent to the classifier again.
	watcher.Tick(now.Add(30 * time.Second))
	if classifier.calls != 1 {
		t.Fatalf("classifier calls after cache = %d", classifier.calls)
	}
}

func TestWatcherAIReviewAndAllowlist(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{
		{Browser: "chrome", Domain: "example-game.net", VisitedAt: now},
		{Browser: "chrome", Domain: "poki.com", VisitedAt: now},
	}}
	recorder := newFakeRecorder()
	recorder.domains["poki.com"] = model.BrowserDomain{
		ID: "poki", Domain: "poki.com", Source: model.BrowserSourceSeed,
		Status: model.BrowserStatusAllowed, FirstSeen: now, LastSeen: now, SeenCount: 1,
	}
	classifier := &fakeClassifier{verdicts: []deepseek.DomainVerdict{{
		Domain: "example-game.net", IsGame: true, Name: "Example", Confidence: 0.80, Reason: "Likely game.",
	}}}
	watcher := New(history, &fakeState{settings: model.BrowserSettings{Enabled: true, AutoBlock: true, AIClassification: true}},
		&fakeSettings{settings: model.AISettings{Model: "deepseek-flash"}, key: "sk-test", ok: true},
		classifier, recorder, &fakeController{}, &fakeAuditor{}, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)

	if len(recorder.seeds) != 0 {
		t.Fatalf("allowlisted seed was re-recorded: %v", recorder.seeds)
	}
	if recorder.domains["example-game.net"].Status != model.BrowserStatusReview {
		t.Fatalf("status = %s", recorder.domains["example-game.net"].Status)
	}
}

func TestWatcherIdleWhenPlayAllowedOrDisabled(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{{Browser: "chrome", Domain: "poki.com", VisitedAt: now}}}
	recorder := newFakeRecorder()
	watcher := New(history, &fakeState{allowed: true, settings: blockedWatchState()},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{}, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)
	if history.calls != 0 {
		t.Fatalf("history was polled while play was allowed: %d", history.calls)
	}

	watcher = New(history, &fakeState{settings: model.BrowserSettings{Enabled: false}},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{}, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)
	if history.calls != 0 {
		t.Fatalf("history was polled while monitoring was disabled: %d", history.calls)
	}
}

func TestWatcherRecordsHistoryError(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{err: errors.New("history locked")}
	recorder := newFakeRecorder()
	watcher := New(history, &fakeState{settings: blockedWatchState()},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{}, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)
	if recorder.lastError == "" {
		t.Fatal("history error was not recorded")
	}
}

func TestWatcherCachesNegativeDomainVerdicts(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{{Browser: "chrome", Domain: "unknown.example", VisitedAt: now}}}
	recorder := newFakeRecorder()
	classifier := &fakeClassifier{verdicts: []deepseek.DomainVerdict{{
		Domain: "unknown.example", IsGame: false, Confidence: 0.1, Reason: "Not a game site.",
	}}}
	watcher := New(history, &fakeState{settings: model.BrowserSettings{Enabled: true, AutoBlock: true, AIClassification: true}},
		&fakeSettings{settings: model.AISettings{Model: "deepseek-flash"}, key: "sk-test", ok: true},
		classifier, recorder, &fakeController{}, &fakeAuditor{}, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)
	history.visits[0].VisitedAt = now.Add(30 * time.Second)
	watcher.Tick(now.Add(30 * time.Second))
	if classifier.calls != 1 {
		t.Fatalf("classifier calls = %d, want one cached call", classifier.calls)
	}
	if len(recorder.domains) != 0 {
		t.Fatalf("negative verdict stored a domain: %+v", recorder.domains)
	}
}
