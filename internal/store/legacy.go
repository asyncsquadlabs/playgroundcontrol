package store

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	legacyConfigFile   = "config.json"
	legacyConfigBackup = "config.json.bak"
	legacyAuthFile     = "auth.json"
	legacyAuditFile    = "audit.jsonl"
	legacyAuditRotated = "audit.1.jsonl"
	legacyMarkerKey    = "legacy_imported"
)

var legacyFiles = []string{
	legacyAuthFile,
	legacyConfigFile,
	legacyConfigBackup,
	legacyAuditFile,
	legacyAuditRotated,
}

// importLegacy performs a one-time import of the JSON/JSONL files used by
// versions before the SQLite store. The import and its marker are committed in
// a single transaction so a failed import can be retried safely.
func (s *Store) importLegacy(db *sql.DB) ([]string, error) {
	var marker string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, legacyMarkerKey).Scan(&marker)
	if err == nil {
		return s.archiveLegacyFiles(), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read legacy import marker: %w", err)
	}

	var warnings []string
	cfg, cfgWarnings := s.readLegacyConfig()
	warnings = append(warnings, cfgWarnings...)
	record, authWarnings := s.readLegacyAuth()
	warnings = append(warnings, authWarnings...)
	events, auditWarnings := s.readLegacyAudit()
	warnings = append(warnings, auditWarnings...)

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin legacy import: %w", err)
	}
	rollback := func(err error) ([]string, error) {
		_ = tx.Rollback()
		return nil, err
	}

	if _, err := tx.Exec(
		`INSERT INTO app_state (id, play_allowed, last_changed_at, last_changed_by)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   play_allowed = excluded.play_allowed,
		   last_changed_at = excluded.last_changed_at,
		   last_changed_by = excluded.last_changed_by`,
		boolInt(cfg.PlayAllowed),
		formatTime(cfg.LastChangedAt),
		cfg.LastChangedBy,
	); err != nil {
		return rollback(fmt.Errorf("import play state: %w", err))
	}
	for _, game := range cfg.Games {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO games (id, game_key, name, executable, image_path, added_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			game.ID,
			model.GameKey(game),
			game.Name,
			game.Executable,
			game.ImagePath,
			formatTime(game.AddedAt),
		); err != nil {
			return rollback(fmt.Errorf("import game %q: %w", game.Name, err))
		}
	}
	if record != nil {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO auth_record
			 (id, version, algorithm, salt, hash, time, memory_kib, threads, key_len, updated_at)
			 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.Version,
			record.Algorithm,
			record.Salt,
			record.Hash,
			record.Time,
			record.MemoryKiB,
			record.Threads,
			record.KeyLen,
			formatTime(record.UpdatedAt),
		); err != nil {
			return rollback(fmt.Errorf("import authentication record: %w", err))
		}
	}
	for _, event := range events {
		event = s.normalizeEvent(event)
		if _, err := tx.Exec(
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
		); err != nil {
			return rollback(fmt.Errorf("import audit event: %w", err))
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO meta (key, value) VALUES (?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		legacyMarkerKey,
	); err != nil {
		return rollback(fmt.Errorf("write legacy import marker: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit legacy import: %w", err)
	}

	_, _ = db.Exec(
		`DELETE FROM audit_events WHERE seq <= (SELECT MAX(seq) FROM audit_events) - ?`,
		auditMaxRows,
	)
	warnings = append(warnings, s.archiveLegacyFiles()...)
	return warnings, nil
}

func (s *Store) readLegacyConfig() (model.Config, []string) {
	primary, primaryErr := s.readLegacyConfigFile(filepath.Join(s.dir, legacyConfigFile))
	if primaryErr == nil {
		return primary, nil
	}
	var warnings []string
	if !errors.Is(primaryErr, os.ErrNotExist) {
		warnings = append(warnings, fmt.Sprintf("%s could not be imported: %v", legacyConfigFile, primaryErr))
	}
	backup, backupErr := s.readLegacyConfigFile(filepath.Join(s.dir, legacyConfigBackup))
	if backupErr == nil {
		warnings = append(warnings, "recovered the last configuration backup during SQLite migration")
		return backup, warnings
	}
	if !errors.Is(backupErr, os.ErrNotExist) {
		warnings = append(warnings, fmt.Sprintf("%s was not usable during SQLite migration: %v", legacyConfigBackup, backupErr))
	}
	return model.DefaultConfig(), warnings
}

func (s *Store) readLegacyConfigFile(path string) (model.Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixed legacy file names under the protected data directory.
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

func (s *Store) readLegacyAuth() (*auth.Record, []string) {
	data, err := os.ReadFile(filepath.Join(s.dir, legacyAuthFile)) // #nosec G304 -- fixed legacy file name under the protected data directory.
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("%s could not be imported: %v", legacyAuthFile, err)}
	}
	record, err := auth.DecodeRecord(data)
	if err != nil {
		return nil, []string{fmt.Sprintf("%s was not usable during SQLite migration; setup will be required", legacyAuthFile)}
	}
	return &record, nil
}

func (s *Store) readLegacyAudit() ([]model.AuditEvent, []string) {
	var events []model.AuditEvent
	var warnings []string
	for _, name := range []string{legacyAuditRotated, legacyAuditFile} {
		batch, malformed, err := readLegacyAuditFile(filepath.Join(s.dir, name))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				warnings = append(warnings, fmt.Sprintf("%s could not be imported: %v", name, err))
			}
			continue
		}
		events = append(events, batch...)
		if malformed > 0 {
			warnings = append(warnings, fmt.Sprintf("%s contained %d malformed event lines that were skipped", name, malformed))
		}
	}
	return events, warnings
}

func readLegacyAuditFile(path string) ([]model.AuditEvent, int, error) {
	f, err := os.Open(path) // #nosec G304 -- fixed legacy file names under the protected data directory.
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var events []model.AuditEvent
	malformed := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event model.AuditEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			malformed++
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, malformed, err
	}
	return events, malformed, nil
}

func (s *Store) normalizeEvent(event model.AuditEvent) model.AuditEvent {
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
	return event
}

func (s *Store) archiveLegacyFiles() []string {
	var warnings []string
	for _, name := range legacyFiles {
		source := filepath.Join(s.dir, name)
		if !regularFileExists(source) {
			continue
		}
		target := source + ".migrated"
		_ = os.Remove(target)
		if err := os.Rename(source, target); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not archive %s: %v", name, err))
		}
	}
	return warnings
}
