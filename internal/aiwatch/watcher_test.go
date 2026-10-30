package aiwatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/deepseek"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

type fakeState struct {
	mu      sync.Mutex
	allowed bool
	games   []model.Game
}

func (f *fakeState) PlayAllowed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowed
}

func (f *fakeState) Games() []model.Game {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Game(nil), f.games...)
}

func (f *fakeState) setAllowed(value bool) {
	f.mu.Lock()
	f.allowed = value
	f.mu.Unlock()
}

type fakeSettings struct {
	settings model.AISettings
	key      string
	ok       bool
}

func (f *fakeSettings) AICredentials() (model.AISettings, string, bool) {
	return f.settings, f.key, f.ok
}

type fakeSource struct {
	mu         sync.Mutex
	processes  []enforce.Process
	listCalls  int
	describeIn int
}

func (f *fakeSource) List() ([]enforce.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return append([]enforce.Process(nil), f.processes...), nil
}

func (f *fakeSource) Describe(process enforce.Process) enforce.Process {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeIn++
	if process.ProductName == "" {
		process.ProductName = "Example Product"
	}
	return process
}

func (f *fakeSource) setProcesses(processes ...enforce.Process) {
	f.mu.Lock()
	f.processes = append([]enforce.Process(nil), processes...)
	f.mu.Unlock()
}

func (f *fakeSource) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

type fakeClassifier struct {
	mu            sync.Mutex
	classifyCalls int
	testCalls     int
	lastObs       []deepseek.Observation
	verdicts      []deepseek.Verdict
	err           error
	onClassify    func()
}

func (f *fakeClassifier) Classify(_ context.Context, _, _ string, observations []deepseek.Observation) ([]deepseek.Verdict, error) {
	f.mu.Lock()
	f.classifyCalls++
	f.lastObs = append([]deepseek.Observation(nil), observations...)
	onClassify := f.onClassify
	err := f.err
	verdicts := append([]deepseek.Verdict(nil), f.verdicts...)
	f.mu.Unlock()
	if onClassify != nil {
		onClassify()
	}
	if err != nil {
		return nil, err
	}
	return verdicts, nil
}

func (f *fakeClassifier) Test(_ context.Context, _, _ string) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testCalls++
	return time.Millisecond, f.err
}

func (f *fakeClassifier) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.classifyCalls
}

func (f *fakeClassifier) observations() []deepseek.Observation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deepseek.Observation(nil), f.lastObs...)
}

func (f *fakeClassifier) setError(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

type fakeRecorder struct {
	mu        sync.Mutex
	state     *fakeState
	items     []model.StrangeActivity
	games     []model.Game
	ignored   map[string]struct{}
	statuses  []string
	recordErr error
	addErr    error
	ignoreErr error
}

func (f *fakeRecorder) RecordStrangeActivity(item model.StrangeActivity) (model.StrangeActivity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return model.StrangeActivity{}, f.recordErr
	}
	f.items = append(f.items, item)
	if item.ID == "" {
		item.ID = "finding"
	}
	return item, nil
}

func (f *fakeRecorder) AddAIGame(game model.Game) (model.Game, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return model.Game{}, false, f.addErr
	}
	if f.state != nil && f.state.PlayAllowed() {
		return model.Game{}, true, nil
	}
	key := model.StrangeActivityKey(game.Executable, game.ImagePath)
	if _, ignored := f.ignored[key]; ignored {
		return model.Game{}, true, nil
	}
	for _, existing := range f.games {
		if strings.EqualFold(existing.Executable, game.Executable) &&
			strings.EqualFold(existing.ImagePath, game.ImagePath) {
			return existing, true, nil
		}
	}
	if game.ID == "" {
		game.ID = "game"
	}
	game.Source = model.GameSourceAI
	f.games = append(f.games, game)
	if f.state != nil {
		f.state.mu.Lock()
		f.state.games = append(f.state.games, game)
		f.state.mu.Unlock()
	}
	return game, false, nil
}

func (f *fakeRecorder) AIIgnoredKeys() (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ignoreErr != nil {
		return nil, f.ignoreErr
	}
	out := make(map[string]struct{}, len(f.ignored))
	for key := range f.ignored {
		out[key] = struct{}{}
	}
	return out, nil
}

func (f *fakeRecorder) SetAIStatus(_ time.Time, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, message)
	return nil
}

func (f *fakeRecorder) recorded() []model.StrangeActivity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.StrangeActivity(nil), f.items...)
}

func (f *fakeRecorder) addedGames() []model.Game {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Game(nil), f.games...)
}

func (f *fakeRecorder) setIgnored(executable, imagePath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ignored == nil {
		f.ignored = make(map[string]struct{})
	}
	f.ignored[model.StrangeActivityKey(executable, imagePath)] = struct{}{}
}

type fakeBlocker struct {
	mu    sync.Mutex
	count int
}

func (f *fakeBlocker) Wake() {
	f.mu.Lock()
	f.count++
	f.mu.Unlock()
}

func (f *fakeBlocker) wakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

type fakeAuditor struct {
	mu     sync.Mutex
	events []model.AuditEvent
}

func (f *fakeAuditor) AddEvent(event model.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakeAuditor) snapshot() []model.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.AuditEvent(nil), f.events...)
}

func testSettings(enabled bool, key string) *fakeSettings {
	return &fakeSettings{
		settings: model.AISettings{
			Enabled:       enabled,
			AutoBlock:     true,
			Model:         model.DefaultAIModel,
			KeyConfigured: key != "",
		},
		key: key,
		ok:  key != "",
	}
}

func oldProcess(name, path string, pid uint32) enforce.Process {
	return enforce.Process{
		PID:       pid,
		Name:      name,
		ImagePath: path,
		CreatedAt: time.Now().Add(-time.Hour),
		SessionID: 1,
	}
}

func newTestWatcher(state *fakeState, source *fakeSource, settings *fakeSettings, classifier *fakeClassifier, recorder *fakeRecorder) (*Watcher, *fakeBlocker) {
	recorder.state = state
	blocker := &fakeBlocker{}
	watcher := New(source, state, settings, classifier, recorder, &fakeAuditor{}, blocker, Options{
		PollInterval:        time.Second,
		MinRequestInterval:  time.Nanosecond,
		MaxRequestsPerHour:  100,
		TouchInterval:       time.Minute,
		MinProcessAge:       time.Second,
		Confidence:          0.70,
		AutoBlockConfidence: 0.90,
		BackoffMin:          time.Minute,
		BackoffMax:          time.Hour,
		Now:                 time.Now,
	})
	return watcher, blocker
}

func TestNoScanningWhilePlayAllowed(t *testing.T) {
	state := &fakeState{allowed: true}
	source := &fakeSource{}
	classifier := &fakeClassifier{}
	recorder := &fakeRecorder{}
	watcher, _ := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())
	if source.calls() != 0 || classifier.calls() != 0 {
		t.Fatalf("allowed play performed work: list=%d classify=%d", source.calls(), classifier.calls())
	}
}

func TestNoScanningWhenDisabledOrKeyMissing(t *testing.T) {
	for _, settings := range []*fakeSettings{testSettings(false, "sk-test"), testSettings(true, "")} {
		source := &fakeSource{}
		classifier := &fakeClassifier{}
		watcher, _ := newTestWatcher(&fakeState{}, source, settings, classifier, &fakeRecorder{})
		watcher.Tick(time.Now())
		if source.calls() != 0 || classifier.calls() != 0 {
			t.Fatalf("inactive monitoring performed work: list=%d classify=%d", source.calls(), classifier.calls())
		}
	}
}

func TestSkipsConfiguredRobloxSystemAndYoungProcesses(t *testing.T) {
	now := time.Now()
	state := &fakeState{games: []model.Game{{ID: "g1", Name: "Configured", Executable: "configured.exe"}}}
	source := &fakeSource{processes: []enforce.Process{
		oldProcess("configured.exe", `C:\Games\configured.exe`, 1),
		oldProcess("RobloxPlayerBeta.exe", `C:\Roblox\RobloxPlayerBeta.exe`, 2),
		oldProcess("game.exe", `C:\Windows\System32\game.exe`, 3),
		oldProcess("explorer.exe", `C:\Windows\explorer.exe`, 4),
		{PID: 5, Name: "session-zero.exe", ImagePath: `C:\Games\session-zero.exe`, CreatedAt: now.Add(-time.Hour)},
		{PID: 6, Name: "young.exe", ImagePath: `C:\Games\young.exe`, CreatedAt: now, SessionID: 1},
	}}
	classifier := &fakeClassifier{}
	watcher, _ := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, &fakeRecorder{})
	watcher.Tick(now)
	if classifier.calls() != 0 {
		t.Fatalf("classifier called %d times for excluded processes", classifier.calls())
	}
}

func TestClassifiesUnlistedGameWithRedactedPathAndRecordsFinding(t *testing.T) {
	now := time.Now()
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Users\Alice\Games\game.exe`, 42)
	process.CompanyName = "Example Studio"
	process.FileDescription = "Example Game"
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  `C:\Users\<user>\Games\game.exe`,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.94,
		Reason:     "Interactive game client.",
	}}}
	recorder := &fakeRecorder{}
	settings := testSettings(true, "sk-test")
	settings.settings.AutoBlock = false
	watcher, _ := newTestWatcher(state, source, settings, classifier, recorder)
	watcher.Tick(now)

	observations := classifier.observations()
	if len(observations) != 1 {
		t.Fatalf("observations = %+v", observations)
	}
	if strings.Contains(observations[0].ImagePath, "Alice") {
		t.Fatalf("username was not redacted: %s", observations[0].ImagePath)
	}
	items := recorder.recorded()
	if len(items) != 1 {
		t.Fatalf("recorded=%d items=%+v", len(items), items)
	}
	if items[0].Name != "Example Game" || items[0].ImagePath != process.ImagePath || items[0].Confidence != 0.94 {
		t.Fatalf("unexpected finding: %+v", items[0])
	}
	if items[0].LastPID != 42 || items[0].CompanyName != "Example Studio" {
		t.Fatalf("finding lost process identity: %+v", items[0])
	}
}

func TestCacheUpdatesSeenCountWithoutReclassifying(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.94,
	}}}
	recorder := &fakeRecorder{}
	settings := testSettings(true, "sk-test")
	settings.settings.AutoBlock = false
	watcher, _ := newTestWatcher(state, source, settings, classifier, recorder)
	start := time.Now()
	watcher.Tick(start)
	watcher.Tick(start.Add(61 * time.Second))
	if classifier.calls() != 1 {
		t.Fatalf("classifier calls = %d, want 1", classifier.calls())
	}
	if len(recorder.recorded()) != 2 {
		t.Fatalf("recorded = %+v", recorder.recorded())
	}
}

func TestLowConfidenceVerdictIsNotRecorded(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
		IsGame:     true,
		Name:       "Maybe Game",
		Confidence: 0.4,
	}}}
	recorder := &fakeRecorder{}
	watcher, _ := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())
	if len(recorder.recorded()) != 0 {
		t.Fatalf("low-confidence verdict was recorded: %+v", recorder.recorded())
	}
}

func TestDoesNotRecordIfPermissionChangesDuringClassification(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{
		verdicts: []deepseek.Verdict{{
			Executable: "game.exe",
			ImagePath:  `C:\Games\game.exe`,
			IsGame:     true,
			Name:       "Example Game",
			Confidence: 0.94,
		}},
		onClassify: func() { state.setAllowed(true) },
	}
	recorder := &fakeRecorder{}
	watcher, blocker := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())
	if len(recorder.recorded()) != 0 {
		t.Fatalf("finding was recorded after permission changed: %+v", recorder.recorded())
	}
	if len(recorder.addedGames()) != 0 || blocker.wakes() != 0 {
		t.Fatalf("auto-block ran after permission changed: games=%+v wakes=%d", recorder.addedGames(), blocker.wakes())
	}
}

func TestRateLimitPreventsSecondBatch(t *testing.T) {
	state := &fakeState{}
	source := &fakeSource{processes: []enforce.Process{oldProcess("one.exe", `C:\Games\one.exe`, 1)}}
	classifier := &fakeClassifier{}
	recorder := &fakeRecorder{}
	watcher := New(source, state, testSettings(true, "sk-test"), classifier, recorder, &fakeAuditor{}, &fakeBlocker{}, Options{
		PollInterval:       time.Second,
		MinRequestInterval: time.Nanosecond,
		MaxRequestsPerHour: 1,
		MinProcessAge:      time.Second,
		Confidence:         0.70,
		BackoffMin:         time.Minute,
		BackoffMax:         time.Hour,
	})
	start := time.Now()
	watcher.Tick(start)
	source.setProcesses(oldProcess("two.exe", `C:\Games\two.exe`, 2))
	watcher.Tick(start.Add(time.Second))
	if classifier.calls() != 1 {
		t.Fatalf("classifier calls = %d, want 1", classifier.calls())
	}
}

func TestProviderFailureBacksOffThenRecovers(t *testing.T) {
	state := &fakeState{}
	source := &fakeSource{processes: []enforce.Process{oldProcess("game.exe", `C:\Games\game.exe`, 1)}}
	classifier := &fakeClassifier{err: errors.New("provider unavailable")}
	recorder := &fakeRecorder{}
	watcher := New(source, state, testSettings(true, "sk-test"), classifier, recorder, &fakeAuditor{}, &fakeBlocker{}, Options{
		PollInterval:       time.Second,
		MinRequestInterval: time.Nanosecond,
		MaxRequestsPerHour: 100,
		MinProcessAge:      time.Second,
		Confidence:         0.70,
		BackoffMin:         time.Minute,
		BackoffMax:         time.Hour,
	})
	start := time.Now()
	watcher.Tick(start)
	watcher.Tick(start.Add(30 * time.Second))
	if classifier.calls() != 1 {
		t.Fatalf("classifier calls during backoff = %d, want 1", classifier.calls())
	}
	classifier.setError(nil)
	classifier.verdicts = []deepseek.Verdict{}
	watcher.Tick(start.Add(61 * time.Second))
	if classifier.calls() != 2 {
		t.Fatalf("classifier calls after backoff = %d, want 2", classifier.calls())
	}
	recorder.mu.Lock()
	statuses := append([]string(nil), recorder.statuses...)
	recorder.mu.Unlock()
	if len(statuses) < 2 || statuses[len(statuses)-1] != "" {
		t.Fatalf("statuses = %+v", statuses)
	}
}

func TestWatcherTestConnectionUpdatesStatus(t *testing.T) {
	settings := testSettings(true, "sk-test")
	classifier := &fakeClassifier{}
	recorder := &fakeRecorder{}
	watcher, _ := newTestWatcher(&fakeState{}, &fakeSource{}, settings, classifier, recorder)
	if _, err := watcher.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	classifier.mu.Lock()
	testCalls := classifier.testCalls
	classifier.mu.Unlock()
	if testCalls != 1 {
		t.Fatalf("test calls = %d", testCalls)
	}
	recorder.mu.Lock()
	statuses := append([]string(nil), recorder.statuses...)
	recorder.mu.Unlock()
	if len(statuses) == 0 || statuses[len(statuses)-1] != "" {
		t.Fatalf("statuses = %+v", statuses)
	}
}

func TestHighConfidenceAutoBlocksGameAndWakesEnforcer(t *testing.T) {
	now := time.Now()
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  process.ImagePath,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.95,
		Reason:     "Interactive game client.",
	}}}
	recorder := &fakeRecorder{}
	watcher, blocker := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(now)

	games := recorder.addedGames()
	if len(games) != 1 {
		t.Fatalf("added games = %+v", games)
	}
	if games[0].Source != model.GameSourceAI || games[0].ImagePath != process.ImagePath {
		t.Fatalf("unexpected auto-blocked game: %+v", games[0])
	}
	if len(recorder.recorded()) != 0 {
		t.Fatalf("auto-blocked game was also recorded as a finding: %+v", recorder.recorded())
	}
	if blocker.wakes() != 1 {
		t.Fatalf("enforcer wakes = %d, want 1", blocker.wakes())
	}
	found := false
	for _, event := range watcher.auditor.(*fakeAuditor).snapshot() {
		if event.Result == "ai-auto-blocked" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing ai-auto-blocked audit event")
	}
}

func TestMediumConfidenceRecordsFindingWithoutAutoBlock(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  process.ImagePath,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.85,
	}}}
	recorder := &fakeRecorder{}
	watcher, blocker := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())

	if len(recorder.recorded()) != 1 || len(recorder.addedGames()) != 0 || blocker.wakes() != 0 {
		t.Fatalf("medium confidence handling: findings=%d games=%d wakes=%d",
			len(recorder.recorded()), len(recorder.addedGames()), blocker.wakes())
	}
}

func TestAutoBlockDisabledRecordsFinding(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  process.ImagePath,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.99,
	}}}
	settings := testSettings(true, "sk-test")
	settings.settings.AutoBlock = false
	recorder := &fakeRecorder{}
	watcher, blocker := newTestWatcher(state, source, settings, classifier, recorder)
	watcher.Tick(time.Now())

	if len(recorder.recorded()) != 1 || len(recorder.addedGames()) != 0 || blocker.wakes() != 0 {
		t.Fatalf("disabled auto-block handling: findings=%d games=%d wakes=%d",
			len(recorder.recorded()), len(recorder.addedGames()), blocker.wakes())
	}
}

func TestIgnoredProcessIsSkippedWithoutClassification(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  process.ImagePath,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.99,
	}}}
	recorder := &fakeRecorder{}
	recorder.setIgnored(process.Name, process.ImagePath)
	watcher, blocker := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())

	if classifier.calls() != 0 || len(recorder.recorded()) != 0 || len(recorder.addedGames()) != 0 || blocker.wakes() != 0 {
		t.Fatalf("ignored process work: classify=%d findings=%d games=%d wakes=%d",
			classifier.calls(), len(recorder.recorded()), len(recorder.addedGames()), blocker.wakes())
	}
}

func TestAutoBlockFailureFallsBackToFinding(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  process.ImagePath,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.95,
	}}}
	recorder := &fakeRecorder{addErr: errors.New("database unavailable")}
	watcher, blocker := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())

	if len(recorder.recorded()) != 1 || len(recorder.addedGames()) != 0 || blocker.wakes() != 0 {
		t.Fatalf("auto-block failure handling: findings=%d games=%d wakes=%d",
			len(recorder.recorded()), len(recorder.addedGames()), blocker.wakes())
	}
	recorder.mu.Lock()
	statuses := append([]string(nil), recorder.statuses...)
	recorder.mu.Unlock()
	if len(statuses) == 0 || statuses[len(statuses)-1] == "" {
		t.Fatalf("auto-block failure did not set an AI status error: %+v", statuses)
	}
}

func TestEmptyImagePathFallsBackToFinding(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", "", 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.95,
	}}}
	recorder := &fakeRecorder{}
	watcher, blocker := newTestWatcher(state, source, testSettings(true, "sk-test"), classifier, recorder)
	watcher.Tick(time.Now())

	if len(recorder.recorded()) != 1 || len(recorder.addedGames()) != 0 || blocker.wakes() != 0 {
		t.Fatalf("empty path handling: findings=%d games=%d wakes=%d",
			len(recorder.recorded()), len(recorder.addedGames()), blocker.wakes())
	}
}

func TestCachedHighConfidenceRetriesAutoBlockWithoutReclassifying(t *testing.T) {
	state := &fakeState{}
	process := oldProcess("game.exe", `C:\Games\game.exe`, 42)
	source := &fakeSource{processes: []enforce.Process{process}}
	classifier := &fakeClassifier{verdicts: []deepseek.Verdict{{
		Executable: "game.exe",
		ImagePath:  process.ImagePath,
		IsGame:     true,
		Name:       "Example Game",
		Confidence: 0.95,
	}}}
	settings := testSettings(true, "sk-test")
	settings.settings.AutoBlock = false
	recorder := &fakeRecorder{}
	watcher, blocker := newTestWatcher(state, source, settings, classifier, recorder)
	start := time.Now()
	watcher.Tick(start)
	if len(recorder.recorded()) != 1 || len(recorder.addedGames()) != 0 {
		t.Fatalf("advisory tick: findings=%d games=%d", len(recorder.recorded()), len(recorder.addedGames()))
	}

	settings.settings.AutoBlock = true
	watcher.Tick(start.Add(time.Second))
	if classifier.calls() != 1 {
		t.Fatalf("classifier calls = %d, want cached result reuse", classifier.calls())
	}
	if len(recorder.addedGames()) != 1 || blocker.wakes() != 1 {
		t.Fatalf("cached auto-block: games=%d wakes=%d", len(recorder.addedGames()), blocker.wakes())
	}
}
