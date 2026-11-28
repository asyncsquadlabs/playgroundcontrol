package browserwatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/deepseek"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

type fakeHistory struct {
	visits       []Visit
	profiles     []model.BrowserProfileInfo
	users        []model.BrowserUser
	calls        int
	gameCalls    int
	journalCalls int
	err          error
}

func (f *fakeHistory) copyVisits(running bool) ([]Visit, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := append([]Visit(nil), f.visits...)
	if running {
		for i := range out {
			out[i].Running = true
		}
	}
	return out, nil
}

func (f *fakeHistory) RecentGameVisits(context.Context, time.Time) ([]Visit, error) {
	f.gameCalls++
	return f.copyVisits(true)
}

func (f *fakeHistory) RecentJournalScan(context.Context, time.Time, map[string]struct{}) ([]Visit, []model.BrowserProfileInfo, error) {
	f.journalCalls++
	visits, err := f.copyVisits(true)
	return visits, append([]model.BrowserProfileInfo(nil), f.profiles...), err
}

func (f *fakeHistory) BrowserUsers(context.Context) ([]model.BrowserUser, error) {
	return append([]model.BrowserUser(nil), f.users...), nil
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
	verdicts     []deepseek.DomainVerdict
	observations []deepseek.DomainObservation
	calls        int
	err          error
}

func (f *fakeClassifier) ClassifyDomains(_ context.Context, _, _ string, observations []deepseek.DomainObservation) ([]deepseek.DomainVerdict, error) {
	f.calls++
	f.observations = append(f.observations, observations...)
	if f.err != nil {
		return nil, f.err
	}
	return append([]deepseek.DomainVerdict(nil), f.verdicts...), nil
}

type fakeRecorder struct {
	mu            sync.Mutex
	domains       map[string]model.BrowserDomain
	seeds         []string
	ai            []string
	detections    []model.BrowserDetection
	activities    []model.BrowserVisit
	alerts        []model.BrowserHistoryAlert
	stats         map[string]model.BrowserHistoryStat
	selected      map[string]struct{}
	removedRecent int
	recentCount   int
	backfillAt    time.Time
	backfillCh    chan struct{}
	pruned        int
	lastError     string
	statusAt      time.Time
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{
		domains:    make(map[string]model.BrowserDomain),
		stats:      make(map[string]model.BrowserHistoryStat),
		selected:   make(map[string]struct{}),
		backfillCh: make(chan struct{}, 1),
	}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusAt = now
	f.lastError = message
	return nil
}

func (f *fakeRecorder) SelectedBrowserUserSIDs() (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.selected))
	for key := range f.selected {
		out[key] = struct{}{}
	}
	return out, nil
}

func (f *fakeRecorder) RecordBrowserActivity(_ time.Time, visits []model.BrowserVisit) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activities = append(f.activities, visits...)
	return len(visits), nil
}

func (f *fakeRecorder) MarkMissingBrowserActivity(string, time.Time, time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removedRecent, nil
}

func (f *fakeRecorder) CountBrowserActivityForProfile(string, time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recentCount, nil
}

func (f *fakeRecorder) BrowserHistoryStat(key string) (model.BrowserHistoryStat, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stat, ok := f.stats[key]
	return stat, ok, nil
}

func (f *fakeRecorder) SaveBrowserHistoryStat(stat model.BrowserHistoryStat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stats[stat.ProfileKey] = stat
	return nil
}

func (f *fakeRecorder) RecordBrowserHistoryAlert(alert model.BrowserHistoryAlert) (model.BrowserHistoryAlert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if alert.ID == "" {
		alert.ID = "alert"
	}
	f.alerts = append(f.alerts, alert)
	return alert, nil
}

func (f *fakeRecorder) SetBrowserActivityBackfillDone(at time.Time) error {
	f.mu.Lock()
	f.backfillAt = at
	f.mu.Unlock()
	select {
	case f.backfillCh <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeRecorder) PruneBrowserActivity(time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(f.pruned), nil
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

func TestWatcherJournalsOnlySelectedUsers(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{
		{Browser: "chrome", Domain: "google.com", VisitedAt: now, UserSID: "S-1-5-21-1", UserName: `PC\kid`, ProfileKey: "p1", SourceKey: "s1", URL: "https://google.com/search?q=algebra", Title: "algebra - Google Search", SearchEngine: "Google", SearchQuery: "algebra"},
		{Browser: "edge", Domain: "example.com", VisitedAt: now, UserSID: "S-1-5-21-2", UserName: `PC\parent`, ProfileKey: "p2", SourceKey: "s2", URL: "https://example.com/private", Title: "Private"},
	}}
	recorder := newFakeRecorder()
	recorder.selected["S-1-5-21-1"] = struct{}{}
	settings := model.BrowserSettings{Enabled: true, ActivityLogging: true, ActivityBackfillAt: now}
	watcher := New(history, &fakeState{allowed: true, settings: settings},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{},
		Options{Now: func() time.Time { return now }})
	watcher.lastAudit = now
	watcher.Tick(now)

	if history.journalCalls != 1 {
		t.Fatalf("journal scans = %d", history.journalCalls)
	}
	if len(recorder.activities) != 1 {
		t.Fatalf("journaled visits = %+v", recorder.activities)
	}
	if recorder.activities[0].UserSID != "S-1-5-21-1" {
		t.Fatalf("wrong user journaled: %+v", recorder.activities[0])
	}
	if len(recorder.detections) != 0 {
		t.Fatalf("journal-only tick produced detections: %+v", recorder.detections)
	}
}

func TestWatcherClassifierReceivesOnlyDomains(t *testing.T) {
	now := time.Now().UTC()
	history := &fakeHistory{visits: []Visit{{
		Browser: "chrome", Domain: "unknown.example", VisitedAt: now,
		URL: "https://unknown.example/game?token=secret", Title: "Secret title",
		SearchEngine: "Google", SearchQuery: "secret search", Running: true,
	}}}
	recorder := newFakeRecorder()
	classifier := &fakeClassifier{verdicts: []deepseek.DomainVerdict{{
		Domain: "unknown.example", IsGame: false, Confidence: 0.1, Reason: "Not a game.",
	}}}
	auditor := &fakeAuditor{}
	watcher := New(history, &fakeState{settings: model.BrowserSettings{Enabled: true, AutoBlock: true, AIClassification: true}},
		&fakeSettings{settings: model.AISettings{Model: "deepseek-flash"}, key: "sk-test", ok: true},
		classifier, recorder, &fakeController{}, auditor, Options{Now: func() time.Time { return now }})
	watcher.Tick(now)

	if len(classifier.observations) != 1 {
		t.Fatalf("observations = %+v", classifier.observations)
	}
	if classifier.observations[0].Domain != "unknown.example" {
		t.Fatalf("observation = %+v", classifier.observations[0])
	}
	encoded := classifier.observations[0]
	if encoded.Domain == "" || encoded.Domain != "unknown.example" {
		t.Fatalf("unexpected observation: %+v", encoded)
	}
	for _, event := range recorder.detections {
		if strings.Contains(event.Reason, "token") || strings.Contains(event.Reason, "secret") {
			t.Fatalf("sensitive data reached detection event: %+v", event)
		}
	}
	for _, event := range auditor.events {
		if strings.Contains(event.Message, "token") || strings.Contains(event.Message, "Secret title") || strings.Contains(event.Message, "secret search") {
			t.Fatalf("sensitive data reached audit event: %+v", event)
		}
	}
}

func TestWatcherRaisesHistoryClearAlert(t *testing.T) {
	now := time.Now().UTC()
	profile := model.BrowserProfileInfo{
		ProfileKey: "profile-1", Browser: "chrome", UserSID: "S-1-5-21-1",
		UserName: `PC\kid`, ProfileLabel: "Default", HistoryCount: 90, Readable: true,
	}
	history := &fakeHistory{profiles: []model.BrowserProfileInfo{profile}}
	recorder := newFakeRecorder()
	recorder.selected["S-1-5-21-1"] = struct{}{}
	recorder.stats["profile-1"] = model.BrowserHistoryStat{
		ProfileKey: "profile-1", Browser: "chrome", UserSID: "S-1-5-21-1",
		UserName: `PC\kid`, ProfileLabel: "Default", VisitCount: 100,
		LastReadableAt: now.Add(-time.Hour), LastCheckedAt: now.Add(-time.Hour),
	}
	recorder.removedRecent = 6
	recorder.recentCount = 10
	settings := model.BrowserSettings{Enabled: true, ActivityLogging: true, ActivityBackfillAt: now}
	watcher := New(history, &fakeState{settings: settings},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{},
		Options{Now: func() time.Time { return now }})
	watcher.Tick(now)

	if len(recorder.alerts) != 1 {
		t.Fatalf("alerts = %+v", recorder.alerts)
	}
	if recorder.alerts[0].Reason != model.BrowserHistoryAlertBulkRemoval || recorder.alerts[0].RemovedCount < 6 {
		t.Fatalf("alert = %+v", recorder.alerts[0])
	}
}

func TestWatcherBackfillMarksDone(t *testing.T) {
	now := time.Now().UTC()
	profile := model.BrowserProfileInfo{
		ProfileKey: "profile-1", Browser: "chrome", UserSID: "S-1-5-21-1",
		UserName: `PC\kid`, ProfileLabel: "Default", HistoryCount: 2, Readable: true,
	}
	history := &fakeHistory{
		visits:   []Visit{{Browser: "chrome", Domain: "google.com", VisitedAt: now, UserSID: "S-1-5-21-1", UserName: `PC\kid`, ProfileKey: "profile-1", SourceKey: "s1"}},
		profiles: []model.BrowserProfileInfo{profile},
	}
	recorder := newFakeRecorder()
	recorder.selected["S-1-5-21-1"] = struct{}{}
	settings := model.BrowserSettings{Enabled: true, ActivityLogging: true}
	watcher := New(history, &fakeState{settings: settings},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{},
		Options{Now: func() time.Time { return now }, ActivityBackfillWindow: 90 * 24 * time.Hour})
	watcher.Tick(now)

	select {
	case <-recorder.backfillCh:
	case <-time.After(2 * time.Second):
		t.Fatal("backfill did not complete")
	}
	recorder.mu.Lock()
	written := len(recorder.activities)
	recorder.mu.Unlock()
	if written == 0 {
		t.Fatal("backfill did not write activity")
	}
}

func TestWatcherRaisesMissingHistoryDatabaseAlert(t *testing.T) {
	now := time.Now().UTC()
	profile := model.BrowserProfileInfo{
		ProfileKey: "profile-1", Browser: "firefox", UserSID: "S-1-5-21-1",
		UserName: `PC\kid`, ProfileLabel: "default-release", Readable: false, Error: "history file missing",
	}
	history := &fakeHistory{profiles: []model.BrowserProfileInfo{profile}}
	recorder := newFakeRecorder()
	recorder.selected["S-1-5-21-1"] = struct{}{}
	recorder.stats["profile-1"] = model.BrowserHistoryStat{
		ProfileKey: "profile-1", Browser: "firefox", UserSID: "S-1-5-21-1",
		UserName: `PC\kid`, ProfileLabel: "default-release", VisitCount: 20,
		LastReadableAt: now.Add(-time.Hour), LastCheckedAt: now.Add(-time.Hour),
	}
	settings := model.BrowserSettings{Enabled: true, ActivityLogging: true, ActivityBackfillAt: now}
	watcher := New(history, &fakeState{settings: settings},
		&fakeSettings{}, &fakeClassifier{}, recorder, &fakeController{}, &fakeAuditor{},
		Options{Now: func() time.Time { return now }})
	watcher.Tick(now)

	if len(recorder.alerts) != 1 || recorder.alerts[0].Reason != model.BrowserHistoryAlertDatabaseMissing {
		t.Fatalf("alerts = %+v", recorder.alerts)
	}
}
