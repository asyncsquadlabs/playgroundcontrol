package enforce

import (
	"errors"
	"sync"
	"testing"
	"time"

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

func (f *fakeState) setGames(games []model.Game) {
	f.mu.Lock()
	f.games = append([]model.Game(nil), games...)
	f.mu.Unlock()
}

type fakeSource struct {
	mu            sync.Mutex
	processes     []Process
	listErr       error
	closeErr      error
	killErr       error
	closeRequests []uint32
	kills         []uint32
}

func (f *fakeSource) List() ([]Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Process(nil), f.processes...), nil
}

func (f *fakeSource) RequestClose(process Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeRequests = append(f.closeRequests, process.PID)
	return f.closeErr
}

func (f *fakeSource) Kill(process Process) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills = append(f.kills, process.PID)
	return f.killErr
}

func (f *fakeSource) setProcesses(processes []Process) {
	f.mu.Lock()
	f.processes = append([]Process(nil), processes...)
	f.mu.Unlock()
}

func (f *fakeSource) counts() (closes, kills int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.closeRequests), len(f.kills)
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

func (f *fakeAuditor) resultCount(result string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, event := range f.events {
		if event.Result == result {
			count++
		}
	}
	return count
}

func testGame() model.Game {
	return model.Game{ID: "game-1", Name: "Example Game", Executable: "game.exe"}
}

func newTestEnforcer(state *fakeState, source *fakeSource, auditor *fakeAuditor) *Enforcer {
	return New(source, state, auditor, Options{
		PollInterval: time.Second,
		CloseGrace:   10 * time.Second,
		Now:          time.Now,
	})
}

func TestEnforcerCloseThenForceKill(t *testing.T) {
	start := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	process := Process{PID: 1234, Name: "game.exe", CreatedAt: start.Add(-time.Minute)}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{process}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	closes, kills := source.counts()
	if closes != 1 || kills != 0 {
		t.Fatalf("first tick closes=%d kills=%d", closes, kills)
	}
	status := engine.Status()
	if len(status.RunningBlockedGames) != 1 || len(status.PendingClosures) != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}

	engine.Tick(start.Add(9 * time.Second))
	_, kills = source.counts()
	if kills != 0 {
		t.Fatalf("game killed before grace period: %d", kills)
	}

	engine.Tick(start.Add(10 * time.Second))
	_, kills = source.counts()
	if kills != 1 {
		t.Fatalf("game was not force-killed at deadline: %d", kills)
	}
	status = engine.Status()
	if len(status.RunningBlockedGames) != 0 || len(status.PendingClosures) != 0 {
		t.Fatalf("status not cleared after kill: %+v", status)
	}
	if auditor.resultCount("terminated") != 1 {
		t.Fatalf("missing termination audit event: %+v", auditor.events)
	}
}

func TestEnforcerProtectsRobloxWithoutUserConfiguration(t *testing.T) {
	start := time.Now()
	state := &fakeState{}
	source := &fakeSource{processes: []Process{{
		PID:       777,
		Name:      "RobloxPlayerBeta.exe",
		ImagePath: `C:\Users\player\AppData\Local\Roblox\Versions\version-123\RobloxPlayerBeta.exe`,
		CreatedAt: start,
	}}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	closes, _ := source.counts()
	if closes != 1 {
		t.Fatalf("built-in Roblox protection did not request a close: %d", closes)
	}
	status := engine.Status()
	if len(status.RunningBlockedGames) != 1 || status.RunningBlockedGames[0].GameID != "builtin-roblox-player-beta" {
		t.Fatalf("unexpected built-in Roblox status: %+v", status)
	}

	source.setProcesses(nil)
	engine.Tick(start.Add(2 * time.Second))
	if auditor.resultCount("closed") != 1 {
		t.Fatalf("missing built-in Roblox close event: %+v", auditor.events)
	}
}

func TestEnforcerProtectsRenamedRobloxInsideInstallDirectory(t *testing.T) {
	start := time.Now()
	state := &fakeState{}
	source := &fakeSource{processes: []Process{{
		PID:       778,
		Name:      "renamed.exe",
		ImagePath: `C:\Users\player\AppData\Local\Roblox\Versions\version-123\renamed.exe`,
		CreatedAt: start,
	}}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)
	engine.Tick(start)
	closes, _ := source.counts()
	if closes != 1 {
		t.Fatalf("renamed Roblox install path was not protected: %d", closes)
	}
}

func TestEnforcerDoesNothingWhenPlayAllowed(t *testing.T) {
	now := time.Now()
	state := &fakeState{allowed: true, games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{{PID: 42, Name: "game.exe", CreatedAt: now}}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)
	engine.Tick(now)
	closes, kills := source.counts()
	if closes != 0 || kills != 0 {
		t.Fatalf("enforcement acted while allowed: closes=%d kills=%d", closes, kills)
	}
}

func TestEnforcerCancelsPendingKillWhenAllowed(t *testing.T) {
	start := time.Now()
	process := Process{PID: 42, Name: "game.exe", CreatedAt: start}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{process}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	state.setAllowed(true)
	engine.Tick(start.Add(time.Second))
	engine.Tick(start.Add(20 * time.Second))
	_, kills := source.counts()
	if kills != 0 {
		t.Fatalf("pending kill survived permission restore: %d", kills)
	}
	if len(engine.Status().PendingClosures) != 0 {
		t.Fatal("pending closure was not cleared")
	}
}

func TestEnforcerCancelsPendingKillWhenGameRemoved(t *testing.T) {
	start := time.Now()
	process := Process{PID: 42, Name: "game.exe", CreatedAt: start}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{process}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	state.setGames(nil)
	engine.Tick(start.Add(20 * time.Second))
	_, kills := source.counts()
	if kills != 0 {
		t.Fatalf("removed game was still force-killed: %d", kills)
	}
}

func TestEnforcerGracefulExitAudit(t *testing.T) {
	start := time.Now()
	process := Process{PID: 77, Name: "game.exe", CreatedAt: start}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{process}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	source.setProcesses(nil)
	engine.Tick(start.Add(2 * time.Second))
	if auditor.resultCount("closed") != 1 {
		t.Fatalf("expected graceful close audit, got %+v", auditor.events)
	}
	_, kills := source.counts()
	if kills != 0 {
		t.Fatal("exited game was force-killed")
	}
}

func TestEnforcerStillKillsWhenCloseHelperFails(t *testing.T) {
	start := time.Now()
	process := Process{PID: 88, Name: "game.exe", CreatedAt: start}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{process}, closeErr: errors.New("no user session")}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	if auditor.resultCount("close-unavailable") != 1 {
		t.Fatal("close failure was not audited")
	}
	engine.Tick(start.Add(10 * time.Second))
	_, kills := source.counts()
	if kills != 1 {
		t.Fatalf("game was not killed after close helper failure: %d", kills)
	}
}

func TestEnforcerRetriesFailedKill(t *testing.T) {
	start := time.Now()
	process := Process{PID: 99, Name: "game.exe", CreatedAt: start}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{process}, killErr: errors.New("access denied")}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	engine.Tick(start.Add(10 * time.Second))
	_, kills := source.counts()
	if kills != 1 {
		t.Fatalf("first kill attempt = %d", kills)
	}
	if auditor.resultCount("termination-failed") != 1 {
		t.Fatal("kill failure was not audited")
	}
	source.mu.Lock()
	source.killErr = nil
	source.mu.Unlock()
	engine.Tick(start.Add(20 * time.Second))
	_, kills = source.counts()
	if kills != 2 {
		t.Fatalf("failed kill was not retried: %d", kills)
	}
	if auditor.resultCount("terminated") != 1 {
		t.Fatal("successful retry was not audited")
	}
}

func TestEnforcerTracksPIDReuseByCreationTime(t *testing.T) {
	start := time.Now()
	oldProcess := Process{PID: 123, Name: "game.exe", CreatedAt: start}
	newProcess := Process{PID: 123, Name: "game.exe", CreatedAt: start.Add(2 * time.Second)}
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{processes: []Process{oldProcess}}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	source.setProcesses([]Process{newProcess})
	engine.Tick(start.Add(1 * time.Second))
	closes, kills := source.counts()
	if closes != 2 || kills != 0 {
		t.Fatalf("replacement process should receive a new close request: closes=%d kills=%d", closes, kills)
	}
	engine.Tick(start.Add(10 * time.Second))
	_, kills = source.counts()
	if kills != 0 {
		t.Fatalf("new process killed before its own grace period: %d", kills)
	}
	engine.Tick(start.Add(11 * time.Second))
	_, kills = source.counts()
	if kills != 1 {
		t.Fatalf("new process was not killed after its grace period: %d", kills)
	}
	if auditor.resultCount("closed") != 0 {
		t.Fatal("PID reuse was incorrectly reported as a graceful close")
	}
}

func TestEnforcerAuditsScanFailureWithoutSpamming(t *testing.T) {
	start := time.Now()
	state := &fakeState{games: []model.Game{testGame()}}
	source := &fakeSource{listErr: errors.New("snapshot failed")}
	auditor := &fakeAuditor{}
	engine := newTestEnforcer(state, source, auditor)

	engine.Tick(start)
	engine.Tick(start.Add(2 * time.Second))
	if auditor.resultCount("scan-failed") != 1 {
		t.Fatalf("scan failure should be logged once, events=%+v", auditor.events)
	}
	engine.Tick(start.Add(2 * time.Minute))
	if auditor.resultCount("scan-failed") != 2 {
		t.Fatalf("scan failure should be retried after the interval, events=%+v", auditor.events)
	}
}
