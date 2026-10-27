// Package enforce watches for configured game processes while play is blocked,
// asks each process to close, and force-terminates it after a save grace period.
package enforce

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/protection"
)

const (
	DefaultPollInterval = 500 * time.Millisecond
	DefaultCloseGrace   = 10 * time.Second
	killRetryDelay      = 10 * time.Second
	listErrorInterval   = time.Minute
)

// ErrProcessGone lets a source report that the target disappeared between scan
// and termination.
var ErrProcessGone = errors.New("process is no longer running")

// Process is a discovered operating-system process.
type Process struct {
	PID       uint32
	Name      string
	ImagePath string
	CreatedAt time.Time
	SessionID uint32
}

// Source discovers and acts on processes.
type Source interface {
	List() ([]Process, error)
	RequestClose(Process) error
	Kill(Process) error
}

// State supplies the current permission flag and game list.
type State interface {
	PlayAllowed() bool
	Games() []model.Game
}

// Auditor stores enforcement events. Implementations must be safe for
// concurrent use.
type Auditor interface {
	AddEvent(model.AuditEvent) error
}

// Options configures an Enforcer. Zero values use production defaults.
type Options struct {
	PollInterval time.Duration
	CloseGrace   time.Duration
	Now          func() time.Time
}

// Match is a process currently covered by a configured game.
type Match struct {
	PID      uint32 `json:"pid"`
	Name     string `json:"name"`
	GameID   string `json:"gameId"`
	GameName string `json:"gameName"`
}

// Pending is a matched process for which a close was requested and a forced
// termination deadline is active.
type Pending struct {
	PID      uint32    `json:"pid"`
	Name     string    `json:"name"`
	GameName string    `json:"gameName"`
	ForceAt  time.Time `json:"forceAt"`
}

// Status is a read-only view for the dashboard.
type Status struct {
	RunningBlockedGames []Match   `json:"runningBlockedGames"`
	PendingClosures     []Pending `json:"pendingClosures"`
}

type processKey struct {
	pid         uint32
	createdNano int64
}

func keyFor(p Process) processKey {
	return processKey{pid: p.PID, createdNano: p.CreatedAt.UTC().UnixNano()}
}

func matchProcess(process Process, games []model.Game) (model.Game, bool) {
	for _, game := range games {
		if game.Matches(process.Name, process.ImagePath) {
			return game, true
		}
	}
	return protection.MatchRobloxProcess(process.Name, process.ImagePath)
}

type pendingProcess struct {
	process        Process
	game           model.Game
	forceAt        time.Time
	forcedAttempts int
}

// Enforcer is safe for concurrent use.
type Enforcer struct {
	source   Source
	state    State
	auditor  Auditor
	interval time.Duration
	grace    time.Duration
	now      func() time.Time

	wake chan struct{}

	mu              sync.Mutex
	pending         map[processKey]*pendingProcess
	active          map[processKey]Match
	lastListError   string
	lastListErrorAt time.Time
}

// New builds an Enforcer.
func New(source Source, state State, auditor Auditor, options Options) *Enforcer {
	if options.PollInterval <= 0 {
		options.PollInterval = DefaultPollInterval
	}
	if options.CloseGrace <= 0 {
		options.CloseGrace = DefaultCloseGrace
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Enforcer{
		source:   source,
		state:    state,
		auditor:  auditor,
		interval: options.PollInterval,
		grace:    options.CloseGrace,
		now:      options.Now,
		wake:     make(chan struct{}, 1),
		pending:  make(map[processKey]*pendingProcess),
		active:   make(map[processKey]Match),
	}
}

// Wake requests an immediate scan, coalescing multiple requests.
func (e *Enforcer) Wake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run processes until the context is canceled.
func (e *Enforcer) Run(ctx context.Context) {
	e.Tick(e.now())
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.Tick(e.now())
		case <-e.wake:
			e.Tick(e.now())
		}
	}
}

// Tick performs one enforcement pass. Tests call it directly with a fake clock.
func (e *Enforcer) Tick(now time.Time) {
	allowed := e.state.PlayAllowed()
	games := append([]model.Game(nil), e.state.Games()...)
	games = append(games, protection.RobloxGames()...)

	e.mu.Lock()
	defer e.mu.Unlock()

	if allowed {
		// A requested close cannot be recalled, but no force-kill may happen
		// after permission is restored.
		e.pending = make(map[processKey]*pendingProcess)
		e.active = make(map[processKey]Match)
		return
	}
	if len(games) == 0 {
		e.pending = make(map[processKey]*pendingProcess)
		e.active = make(map[processKey]Match)
		return
	}

	processes, err := e.source.List()
	if err != nil {
		if e.lastListError != err.Error() || now.Sub(e.lastListErrorAt) >= listErrorInterval {
			e.audit(model.AuditEvent{
				Time:    now,
				Type:    model.EventEnforcement,
				Message: fmt.Sprintf("Unable to enumerate processes: %v", err),
				Result:  "scan-failed",
			})
			e.lastListError = err.Error()
			e.lastListErrorAt = now
		}
		return
	}
	e.lastListError = ""
	e.lastListErrorAt = time.Time{}

	present := make(map[processKey]Process, len(processes))
	pidsPresent := make(map[uint32]struct{}, len(processes))
	matches := make(map[processKey]struct {
		process Process
		game    model.Game
	})
	for _, process := range processes {
		key := keyFor(process)
		present[key] = process
		pidsPresent[process.PID] = struct{}{}
		if game, matched := matchProcess(process, games); matched {
			matches[key] = struct {
				process Process
				game    model.Game
			}{process: process, game: game}
		}
	}

	// Forget matches that exited or were removed from the game list. A
	// graceful close is recorded when the process vanished while still listed.
	for key, p := range e.pending {
		if _, stillMatched := matches[key]; stillMatched {
			continue
		}
		_, stillRunning := present[key]
		_, pidReused := pidsPresent[p.process.PID]
		if !stillRunning && !pidReused && p.forcedAttempts == 0 {
			e.audit(model.AuditEvent{
				Time:     now,
				Type:     model.EventEnforcement,
				Message:  fmt.Sprintf("Game closed after close request: %s (PID %d)", p.game.Name, p.process.PID),
				GameID:   p.game.ID,
				GameName: p.game.Name,
				PID:      p.process.PID,
				Result:   "closed",
			})
		}
		delete(e.pending, key)
		delete(e.active, key)
	}
	for key := range e.active {
		if _, ok := matches[key]; !ok {
			delete(e.active, key)
		}
	}

	for key, match := range matches {
		nowMatch := Match{
			PID:      match.process.PID,
			Name:     match.process.Name,
			GameID:   match.game.ID,
			GameName: match.game.Name,
		}
		e.active[key] = nowMatch

		p := e.pending[key]
		if p == nil {
			p = &pendingProcess{
				process: match.process,
				game:    match.game,
				forceAt: now.Add(e.grace),
			}
			e.pending[key] = p
			e.audit(model.AuditEvent{
				Time:     now,
				Type:     model.EventEnforcement,
				Message:  fmt.Sprintf("Blocked game detected: %s (PID %d)", match.game.Name, match.process.PID),
				GameID:   match.game.ID,
				GameName: match.game.Name,
				PID:      match.process.PID,
				Result:   "detected",
			})
			if err := e.source.RequestClose(match.process); err != nil {
				e.audit(model.AuditEvent{
					Time:     now,
					Type:     model.EventEnforcement,
					Message:  fmt.Sprintf("Could not request a clean close for %s (PID %d)", match.game.Name, match.process.PID),
					GameID:   match.game.ID,
					GameName: match.game.Name,
					PID:      match.process.PID,
					Result:   "close-unavailable",
				})
			} else {
				e.audit(model.AuditEvent{
					Time:     now,
					Type:     model.EventEnforcement,
					Message:  fmt.Sprintf("Requested a clean close of %s (PID %d)", match.game.Name, match.process.PID),
					GameID:   match.game.ID,
					GameName: match.game.Name,
					PID:      match.process.PID,
					Result:   "close-requested",
				})
			}
			continue
		}

		p.process = match.process
		p.game = match.game
		if now.Before(p.forceAt) {
			continue
		}
		// Re-check the authoritative state immediately before the destructive
		// action so a just-granted permission or just-removed game is honored.
		if e.state.PlayAllowed() {
			e.pending = make(map[processKey]*pendingProcess)
			e.active = make(map[processKey]Match)
			return
		}
		stillConfigured := false
		for _, current := range e.state.Games() {
			if current.ID == match.game.ID {
				stillConfigured = true
				break
			}
		}
		if !stillConfigured {
			delete(e.pending, key)
			delete(e.active, key)
			continue
		}
		if err := e.source.Kill(match.process); err != nil {
			if errors.Is(err, ErrProcessGone) {
				if p.forcedAttempts == 0 {
					e.audit(model.AuditEvent{
						Time:     now,
						Type:     model.EventEnforcement,
						Message:  fmt.Sprintf("Game closed after close request: %s (PID %d)", match.game.Name, match.process.PID),
						GameID:   match.game.ID,
						GameName: match.game.Name,
						PID:      match.process.PID,
						Result:   "closed",
					})
				}
				delete(e.pending, key)
				delete(e.active, key)
				continue
			}
			p.forcedAttempts++
			p.forceAt = now.Add(killRetryDelay)
			e.audit(model.AuditEvent{
				Time:     now,
				Type:     model.EventEnforcement,
				Message:  fmt.Sprintf("Unable to terminate blocked game %s (PID %d): %v", match.game.Name, match.process.PID, err),
				GameID:   match.game.ID,
				GameName: match.game.Name,
				PID:      match.process.PID,
				Result:   "termination-failed",
			})
			continue
		}
		p.forcedAttempts++
		e.audit(model.AuditEvent{
			Time:     now,
			Type:     model.EventEnforcement,
			Message:  fmt.Sprintf("Force-closed blocked game %s (PID %d)", match.game.Name, match.process.PID),
			GameID:   match.game.ID,
			GameName: match.game.Name,
			PID:      match.process.PID,
			Result:   "terminated",
		})
		// The next scan normally finds the process gone. Retaining no pending
		// state avoids suppressing a genuinely new matching process.
		delete(e.pending, key)
		delete(e.active, key)
	}
}

// Status returns a sorted snapshot for the dashboard.
func (e *Enforcer) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()

	status := Status{
		RunningBlockedGames: make([]Match, 0, len(e.active)),
		PendingClosures:     make([]Pending, 0, len(e.pending)),
	}
	for _, match := range e.active {
		status.RunningBlockedGames = append(status.RunningBlockedGames, match)
	}
	for _, pending := range e.pending {
		status.PendingClosures = append(status.PendingClosures, Pending{
			PID:      pending.process.PID,
			Name:     pending.process.Name,
			GameName: pending.game.Name,
			ForceAt:  pending.forceAt.UTC(),
		})
	}
	sort.Slice(status.RunningBlockedGames, func(i, j int) bool {
		return status.RunningBlockedGames[i].PID < status.RunningBlockedGames[j].PID
	})
	sort.Slice(status.PendingClosures, func(i, j int) bool {
		return status.PendingClosures[i].PID < status.PendingClosures[j].PID
	})
	return status
}

func (e *Enforcer) audit(event model.AuditEvent) {
	if e.auditor == nil {
		return
	}
	_ = e.auditor.AddEvent(event)
}
