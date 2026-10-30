package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const aiIgnoredMaxRows = 5_000

// AIIgnoredKeys returns the executable/path identities that AI must skip.
func (s *Store) AIIgnoredKeys() (map[string]struct{}, error) {
	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	rows, err := s.db.Query(`SELECT activity_key FROM ai_ignored`)
	if err != nil {
		return nil, fmt.Errorf("read AI ignore list: %w", err)
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read AI ignore entry: %w", err)
		}
		out[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read AI ignore list: %w", err)
	}
	return out, nil
}

// IgnoreStrangeActivity marks a finding as not-a-game, persists an AI ignore
// entry, and removes the finding in one transaction.
func (s *Store) IgnoreStrangeActivity(id string) (bool, error) {
	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()
	if s.db == nil {
		return false, errors.New("database is not open")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("begin strange activity ignore: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var name, executable, imagePath string
	err = tx.QueryRow(
		`SELECT name, executable, image_path FROM strange_activity WHERE id = ?`,
		id,
	).Scan(&name, &executable, &imagePath)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read strange activity for ignore: %w", err)
	}
	if err := insertAIIgnored(tx, name, executable, imagePath, s.now().UTC()); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM strange_activity WHERE id = ?`, id); err != nil {
		return false, fmt.Errorf("remove ignored strange activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit strange activity ignore: %w", err)
	}
	committed = true
	return true, nil
}

func insertAIIgnored(tx *sql.Tx, name, executable, imagePath string, now time.Time) error {
	name = strings.TrimSpace(name)
	executable = strings.TrimSpace(executable)
	imagePath = strings.TrimSpace(imagePath)
	if executable == "" {
		return errors.New("AI ignore entry requires an executable name")
	}
	if _, err := tx.Exec(
		`INSERT INTO ai_ignored (activity_key, name, executable, image_path, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(activity_key) DO UPDATE SET
		   name = excluded.name,
		   executable = excluded.executable,
		   image_path = excluded.image_path,
		   created_at = excluded.created_at`,
		model.StrangeActivityKey(executable, imagePath),
		name,
		executable,
		imagePath,
		formatTime(now),
	); err != nil {
		return fmt.Errorf("save AI ignore entry: %w", err)
	}
	_, _ = tx.Exec(
		`DELETE FROM ai_ignored WHERE activity_key NOT IN (
			SELECT activity_key FROM ai_ignored ORDER BY created_at DESC, activity_key DESC LIMIT ?
		)`,
		aiIgnoredMaxRows,
	)
	return nil
}
