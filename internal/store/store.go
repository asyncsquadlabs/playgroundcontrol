// Package store owns the persisted play configuration, authentication record,
// and bounded audit log. All state is held in a single SQLite database. All
// mutations are serialized and committed to disk before they become visible in
// memory.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/secret"
)

// ErrDuplicateGame is returned when an identical executable/path pair already
// exists.
var ErrDuplicateGame = errors.New("that executable and path are already configured")

const (
	defaultRecent  = 50
	maxEventLength = 300
)

// Store is safe for concurrent use.
type Store struct {
	dir        string
	dbPath     string
	backupPath string
	now        func() time.Time
	codec      secret.Codec
	db         *sql.DB

	mu         sync.RWMutex
	cfg        model.Config
	ai         model.AISettings
	aiKey      string
	loadNotice string

	auditMu   sync.Mutex
	strangeMu sync.Mutex
	backupMu  sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// Open opens or creates the database in dir and loads the current state using
// the production secret codec.
func Open(dir string, now func() time.Time) (*Store, error) {
	return OpenWithCodec(dir, now, secret.DefaultCodec())
}

// OpenWithCodec opens the database with an injected secret codec. Tests use
// this to verify sealing without depending on Windows DPAPI.
func OpenWithCodec(dir string, now func() time.Time, codec secret.Codec) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if codec == nil {
		codec = secret.DefaultCodec()
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	s := &Store{
		dir:        dir,
		dbPath:     databasePath(dir),
		backupPath: backupPath(dir),
		now:        now,
		codec:      codec,
		cfg:        model.DefaultConfig(),
		ai:         model.DefaultAISettings(),
	}
	db, notice, err := s.openDatabase()
	if err != nil {
		return nil, err
	}
	s.db = db
	s.loadNotice = notice
	if err := s.loadConfig(); err != nil {
		_ = s.db.Close()
		s.db = nil
		return nil, err
	}
	if err := s.loadAISettings(); err != nil {
		s.appendNotice(fmt.Sprintf("AI settings could not be loaded: %v", err))
	}
	return s, nil
}

// EnsureBackup creates the last-known-good snapshot when it is missing. Normal
// mutations and clean shutdown refresh it; the service calls this in the
// background after it is running so startup does not wait on VACUUM.
func (s *Store) EnsureBackup() error {
	if regularFileExists(s.backupPath) {
		return nil
	}
	return s.refreshBackup()
}

// DataDir returns the directory backing this store.
func (s *Store) DataDir() string { return s.dir }

// DatabasePath returns the resolved primary database path.
func (s *Store) DatabasePath() string { return s.dbPath }

// LoadNotice is a human-readable warning from the last load or recovery, if any.
func (s *Store) LoadNotice() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadNotice
}

func (s *Store) appendNotice(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadNotice == "" {
		s.loadNotice = message
		return
	}
	s.loadNotice += "; " + message
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
	if err := s.persistPlayState(next); err != nil {
		return err
	}
	s.cfg = next
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup: %v", err))
	}
	return nil
}

func (s *Store) persistPlayState(next model.Config) error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	_, err := s.db.Exec(
		`UPDATE app_state SET play_allowed = ?, last_changed_at = ?, last_changed_by = ? WHERE id = 1`,
		boolInt(next.PlayAllowed),
		formatTime(next.LastChangedAt),
		next.LastChangedBy,
	)
	if err != nil {
		return fmt.Errorf("save play permission: %w", err)
	}
	return nil
}

// AddGame validates, persists, and adds an administrator-managed game.
func (s *Store) AddGame(g model.Game) (model.Game, error) {
	game, _, err := s.addGame(g, false)
	return game, err
}

// AddAIGame atomically adds an AI-blocked game only while play is blocked, AI
// auto-block is enabled, and the executable/path is not ignored. The bool
// reports that the add was intentionally skipped.
func (s *Store) AddAIGame(g model.Game) (model.Game, bool, error) {
	return s.addGame(g, true)
}

func (s *Store) addGame(g model.Game, ai bool) (model.Game, bool, error) {
	if ai {
		g.Source = model.GameSourceAI
	} else {
		g.Source = model.GameSourceParent
	}
	normalized, err := model.NormalizeGame(g)
	if err != nil {
		return model.Game{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()

	if ai {
		if s.cfg.PlayAllowed || !s.ai.Enabled || !s.ai.AutoBlock {
			return model.Game{}, true, nil
		}
	}

	key := model.GameKey(normalized)
	for _, existing := range s.cfg.Games {
		if model.GameKey(existing) == key {
			if ai {
				return existing, true, nil
			}
			return model.Game{}, false, ErrDuplicateGame
		}
	}

	if s.db == nil {
		return model.Game{}, false, errors.New("database is not open")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return model.Game{}, false, fmt.Errorf("begin add game: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	activityKey := model.StrangeActivityKey(normalized.Executable, normalized.ImagePath)
	if ai {
		var ignored int
		err := tx.QueryRow(`SELECT 1 FROM ai_ignored WHERE activity_key = ?`, activityKey).Scan(&ignored)
		if err == nil {
			return model.Game{}, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return model.Game{}, false, fmt.Errorf("check AI ignore entry: %w", err)
		}
	}
	_, err = tx.Exec(
		`INSERT INTO games (id, game_key, name, executable, image_path, source, added_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		normalized.ID,
		key,
		normalized.Name,
		normalized.Executable,
		normalized.ImagePath,
		normalized.Source,
		formatTime(normalized.AddedAt),
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			if ai {
				return model.Game{}, true, nil
			}
			return model.Game{}, false, ErrDuplicateGame
		}
		return model.Game{}, false, fmt.Errorf("save game: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM ai_ignored WHERE activity_key = ?`, activityKey); err != nil {
		return model.Game{}, false, fmt.Errorf("clear AI ignore entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Game{}, false, fmt.Errorf("commit add game: %w", err)
	}
	committed = true

	next := cloneConfig(s.cfg)
	next.Games = append(next.Games, normalized)
	s.cfg = next
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup: %v", err))
	}
	return normalized, false, nil
}

// DeleteGame removes a configured game. The returned bool reports whether an
// entry was found.
func (s *Store) DeleteGame(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()

	if s.db == nil {
		return false, errors.New("database is not open")
	}
	var found model.Game
	for _, game := range s.cfg.Games {
		if game.ID == id {
			found = game
			break
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin delete game: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.Exec(`DELETE FROM games WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete game: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete game: %w", err)
	}
	if affected == 0 {
		return false, nil
	}
	if found.ID != "" && found.Source == model.GameSourceAI {
		if err := insertAIIgnored(tx, found.Name, found.Executable, found.ImagePath, s.now().UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit delete game: %w", err)
	}
	committed = true

	next := cloneConfig(s.cfg)
	out := next.Games[:0]
	for _, g := range next.Games {
		if g.ID != id {
			out = append(out, g)
		}
	}
	next.Games = out
	s.cfg = next
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup: %v", err))
	}
	return true, nil
}

func (s *Store) loadConfig() error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	var play int
	var lastChangedAt, lastChangedBy sql.NullString
	if err := s.db.QueryRow(
		`SELECT play_allowed, last_changed_at, last_changed_by FROM app_state WHERE id = 1`,
	).Scan(&play, &lastChangedAt, &lastChangedBy); err != nil {
		return fmt.Errorf("load play state: %w", err)
	}

	cfg := model.DefaultConfig()
	cfg.PlayAllowed = play != 0
	cfg.LastChangedAt = parseTime(lastChangedAt.String)
	cfg.LastChangedBy = lastChangedBy.String

	rows, err := s.db.Query(`SELECT id, name, executable, image_path, source, added_at FROM games ORDER BY added_at, id`)
	if err != nil {
		return fmt.Errorf("load games: %w", err)
	}
	defer rows.Close()
	var warnings []string
	for rows.Next() {
		var game model.Game
		var addedAt string
		if err := rows.Scan(&game.ID, &game.Name, &game.Executable, &game.ImagePath, &game.Source, &addedAt); err != nil {
			return fmt.Errorf("load game: %w", err)
		}
		game.AddedAt = parseTime(addedAt)
		normalized, err := model.NormalizeGame(game)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("ignored invalid saved game %q: %v", game.Name, err))
			continue
		}
		cfg.Games = append(cfg.Games, normalized)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load games: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		warnings = append(warnings, fmt.Sprintf("saved configuration was invalid (%v); starting with play blocked and no games", err))
		cfg = model.DefaultConfig()
	}
	s.cfg = cfg
	for _, warning := range warnings {
		s.loadNotice = appendNoticeText(s.loadNotice, warning)
	}
	return nil
}

// AddEvent appends one audit event. The database retains a bounded number of
// the newest events.
func (s *Store) AddEvent(event model.AuditEvent) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	if s.db == nil {
		return errors.New("database is not open")
	}
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

	_, err := s.db.Exec(
		`INSERT INTO audit_events (time, type, message, actor_ip, game_id, game_name, pid, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		formatTime(event.Time),
		event.Type,
		event.Message,
		event.ActorIP,
		event.GameID,
		event.GameName,
		int64(event.PID),
		event.Result,
	)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	_, _ = s.db.Exec(
		`DELETE FROM audit_events WHERE seq <= (SELECT MAX(seq) FROM audit_events) - ?`,
		auditMaxRows,
	)
	return nil
}

// RecentEvents returns up to limit events in chronological order.
func (s *Store) RecentEvents(limit int) ([]model.AuditEvent, error) {
	if limit <= 0 {
		limit = defaultRecent
	}
	if limit > 500 {
		limit = 500
	}
	if s.db == nil {
		return nil, errors.New("database is not open")
	}

	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	rows, err := s.db.Query(
		`SELECT time, type, message, actor_ip, game_id, game_name, pid, result
		 FROM audit_events ORDER BY seq DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read audit events: %w", err)
	}
	defer rows.Close()

	var reversed []model.AuditEvent
	for rows.Next() {
		var event model.AuditEvent
		var when string
		var pid int64
		if err := rows.Scan(&when, &event.Type, &event.Message, &event.ActorIP, &event.GameID, &event.GameName, &pid, &event.Result); err != nil {
			return nil, fmt.Errorf("read audit event: %w", err)
		}
		event.Time = parseTime(when)
		if pid < 0 {
			pid = 0
		}
		event.PID = uint32(pid)
		reversed = append(reversed, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read audit events: %w", err)
	}
	events := make([]model.AuditEvent, len(reversed))
	for i := range reversed {
		events[len(reversed)-1-i] = reversed[i]
	}
	return events, nil
}

// LoadAuthRecord returns the persisted Argon2id record, if one exists.
func (s *Store) LoadAuthRecord() (auth.Record, bool, error) {
	if s.db == nil {
		return auth.Record{}, false, errors.New("database is not open")
	}
	var record auth.Record
	var updatedAt string
	err := s.db.QueryRow(
		`SELECT version, algorithm, salt, hash, time, memory_kib, threads, key_len, updated_at
		 FROM auth_record WHERE id = 1`,
	).Scan(
		&record.Version,
		&record.Algorithm,
		&record.Salt,
		&record.Hash,
		&record.Time,
		&record.MemoryKiB,
		&record.Threads,
		&record.KeyLen,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Record{}, false, nil
	}
	if err != nil {
		return auth.Record{}, false, fmt.Errorf("load authentication record: %w", err)
	}
	record.UpdatedAt = parseTime(updatedAt)
	return record, true, nil
}

// SaveAuthRecord persists the authentication record.
func (s *Store) SaveAuthRecord(record auth.Record) error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	_, err := s.db.Exec(
		`INSERT INTO auth_record
		 (id, version, algorithm, salt, hash, time, memory_kib, threads, key_len, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   version = excluded.version,
		   algorithm = excluded.algorithm,
		   salt = excluded.salt,
		   hash = excluded.hash,
		   time = excluded.time,
		   memory_kib = excluded.memory_kib,
		   threads = excluded.threads,
		   key_len = excluded.key_len,
		   updated_at = excluded.updated_at`,
		record.Version,
		record.Algorithm,
		record.Salt,
		record.Hash,
		record.Time,
		record.MemoryKiB,
		record.Threads,
		record.KeyLen,
		formatTime(record.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("save authentication record: %w", err)
	}
	s.refreshAuthBackup()
	return nil
}

// ClearAuthRecord removes the administrator PIN record while preserving all
// other state.
func (s *Store) ClearAuthRecord() error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	if _, err := s.db.Exec(`DELETE FROM auth_record WHERE id = 1`); err != nil {
		return fmt.Errorf("clear authentication record: %w", err)
	}
	s.refreshAuthBackup()
	return nil
}

// refreshAuthBackup refreshes the snapshot after an authentication change. If
// the snapshot cannot be refreshed, the stale copy is removed so it cannot
// later restore a previous PIN.
func (s *Store) refreshAuthBackup() {
	if err := s.refreshBackup(); err == nil {
		return
	} else if removeErr := removePath(s.backupPath); removeErr == nil {
		s.appendNotice(fmt.Sprintf("could not refresh database backup after an authentication change (%v); the stale backup was removed", err))
	} else {
		s.appendNotice(fmt.Sprintf("could not refresh or remove database backup after an authentication change (%v); stale backup removal failed: %v", err, removeErr))
	}
}

func cloneConfig(c model.Config) model.Config {
	out := c
	out.Games = append([]model.Game(nil), c.Games...)
	if out.Games == nil {
		out.Games = []model.Game{}
	}
	return out
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func appendNoticeText(existing, addition string) string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return existing
	}
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
