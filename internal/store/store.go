// Package store owns the persisted play configuration and the bounded audit
// log. All mutations are serialized and committed to disk before they become
// visible in memory.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

// ErrDuplicateGame is returned when an identical executable/path pair already
// exists.
var ErrDuplicateGame = errors.New("that executable and path are already configured")

const (
	configFile     = "config.json"
	configBackup   = "config.json.bak"
	auditFile      = "audit.jsonl"
	auditRotated   = "audit.1.jsonl"
	auditMaxBytes  = 5 << 20
	defaultRecent  = 50
	maxEventLength = 300
)

// Store is safe for concurrent use.
type Store struct {
	dir string
	now func() time.Time

	mu         sync.RWMutex
	cfg        model.Config
	loadNotice string

	auditMu sync.Mutex
}

// Open loads the configuration. A missing config creates a fail-closed default.
// A corrupt config is not overwritten; it is reported through LoadNotice and
// the service starts blocked. A valid backup is used when possible.
func Open(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	s := &Store{dir: dir, now: now, cfg: model.DefaultConfig()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	primary, primaryErr := s.readConfig(filepath.Join(s.dir, configFile))
	if primaryErr == nil {
		s.cfg = primary
		return nil
	}
	if !errors.Is(primaryErr, os.ErrNotExist) {
		s.loadNotice = fmt.Sprintf("config.json could not be loaded: %v", primaryErr)
	} else {
		s.cfg = model.DefaultConfig()
		return nil
	}

	backup, backupErr := s.readConfig(filepath.Join(s.dir, configBackup))
	if backupErr == nil {
		s.cfg = backup
		s.loadNotice = fmt.Sprintf("%s; recovered the last backup", s.loadNotice)
		return nil
	}

	s.cfg = model.DefaultConfig()
	s.loadNotice = fmt.Sprintf("%s; backup was not usable (%v); starting with play blocked", s.loadNotice, backupErr)
	return nil
}

func (s *Store) readConfig(path string) (model.Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- callers pass fixed file names under the protected data directory.
	if err != nil {
		return model.Config{}, err
	}
	var cfg model.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return model.Config{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return model.Config{}, fmt.Errorf("validate %s: %w", filepath.Base(path), err)
	}
	return cfg, nil
}

// DataDir returns the directory backing this store.
func (s *Store) DataDir() string { return s.dir }

// LoadNotice is a human-readable warning from the last load, if any.
func (s *Store) LoadNotice() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadNotice
}

// Config returns a copy of the current configuration.
func (s *Store) Config() model.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// PlayAllowed reports the current permission state.
func (s *Store) PlayAllowed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.PlayAllowed
}

// Games returns a copy of the configured game list.
func (s *Store) Games() []model.Game {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Game, len(s.cfg.Games))
	copy(out, s.cfg.Games)
	return out
}

// SetPlayAllowed persists and applies a permission change. Failed persistence
// leaves the previous state untouched.
func (s *Store) SetPlayAllowed(allowed bool, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneConfig(s.cfg)
	next.PlayAllowed = allowed
	next.LastChangedAt = s.now().UTC()
	next.LastChangedBy = actor
	if err := s.persistConfig(next); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

// AddGame validates, persists, and adds a game.
func (s *Store) AddGame(g model.Game) (model.Game, error) {
	normalized, err := model.NormalizeGame(g)
	if err != nil {
		return model.Game{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := model.GameKey(normalized)
	for _, existing := range s.cfg.Games {
		if model.GameKey(existing) == key {
			return model.Game{}, ErrDuplicateGame
		}
	}

	next := cloneConfig(s.cfg)
	next.Games = append(next.Games, normalized)
	if err := s.persistConfig(next); err != nil {
		return model.Game{}, err
	}
	s.cfg = next
	return normalized, nil
}

// DeleteGame removes a configured game. The returned bool reports whether an
// entry was found.
func (s *Store) DeleteGame(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneConfig(s.cfg)
	found := false
	out := next.Games[:0]
	for _, g := range next.Games {
		if g.ID == id {
			found = true
			continue
		}
		out = append(out, g)
	}
	if !found {
		return false, nil
	}
	next.Games = out
	if err := s.persistConfig(next); err != nil {
		return false, err
	}
	s.cfg = next
	return true, nil
}

func (s *Store) persistConfig(cfg model.Config) error {
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("refuse to persist invalid configuration: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data = append(data, '\n')

	current := filepath.Join(s.dir, configFile)
	backup := filepath.Join(s.dir, configBackup)
	if old, err := os.ReadFile(current); err == nil { // #nosec G304 -- fixed config file name under the protected data directory.
		if err := writeFileAtomic(backup, old, 0o600); err != nil {
			return fmt.Errorf("back up configuration: %w", err)
		}
	}
	if err := writeFileAtomic(current, data, 0o600); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

func cloneConfig(c model.Config) model.Config {
	out := c
	out.Games = append([]model.Game(nil), c.Games...)
	if out.Games == nil {
		out.Games = []model.Game{}
	}
	return out
}

// AddEvent appends one audit event. When the current log reaches its size
// limit, it retains the previous log as audit.1.jsonl and starts a new one.
func (s *Store) AddEvent(event model.AuditEvent) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	if event.Time.IsZero() {
		event.Time = s.now().UTC()
	} else {
		event.Time = event.Time.UTC()
	}
	event.Message = truncate(event.Message, maxEventLength)
	event.GameName = truncate(event.GameName, maxEventLength)
	event.ActorIP = truncate(event.ActorIP, 128)
	event.Result = truncate(event.Result, 128)
	if event.Type == "" {
		event.Type = model.EventSystem
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode audit event: %w", err)
	}
	data = append(data, '\n')

	current := filepath.Join(s.dir, auditFile)
	if info, err := os.Stat(current); err == nil && info.Size()+int64(len(data)) > auditMaxBytes {
		if err := rotateAudit(current, filepath.Join(s.dir, auditRotated)); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(current, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // #nosec G304 -- fixed audit log name under the protected data directory.
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("append audit log: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync audit log: %w", err)
	}
	return f.Close()
}

func rotateAudit(current, rotated string) error {
	_ = os.Remove(rotated)
	if err := os.Rename(current, rotated); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate audit log: %w", err)
	}
	return nil
}

// RecentEvents returns up to limit events in chronological order, reading the
// rotated log if needed.
func (s *Store) RecentEvents(limit int) ([]model.AuditEvent, error) {
	if limit <= 0 {
		limit = defaultRecent
	}
	if limit > 500 {
		limit = 500
	}

	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	var events []model.AuditEvent
	for _, name := range []string{auditRotated, auditFile} {
		batch, err := readEvents(filepath.Join(s.dir, name))
		if err != nil {
			return nil, err
		}
		events = append(events, batch...)
	}
	if len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events, nil
}

func readEvents(path string) ([]model.AuditEvent, error) {
	f, err := os.Open(path) // #nosec G304 -- callers pass fixed audit file names under the protected data directory.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()

	var events []model.AuditEvent
	scanner := bufio.NewScanner(io.LimitReader(f, auditMaxBytes+1))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		var event model.AuditEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	return events, nil
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}
