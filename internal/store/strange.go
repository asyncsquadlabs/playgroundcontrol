package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	defaultStrangeActivity = 50
	maxStrangeActivity     = 200
	strangeActivityMaxRows = 5_000
	strangeActivityColumns = `id, name, executable, image_path, product_name, company_name,
		file_description, confidence, reason, first_seen, last_seen, seen_count, last_pid, model`
)

// ErrStrangeActivityNotFound reports that a finding was already ignored or promoted.
var ErrStrangeActivityNotFound = errors.New("strange activity not found")

// RecordStrangeActivity inserts a new finding or updates an existing finding
// with the same executable/path identity.
func (s *Store) RecordStrangeActivity(item model.StrangeActivity) (model.StrangeActivity, error) {
	now := s.now().UTC()
	if item.FirstSeen.IsZero() {
		item.FirstSeen = now
	}
	if item.LastSeen.IsZero() || item.LastSeen.Before(item.FirstSeen) {
		item.LastSeen = now
	}
	normalized, err := model.NormalizeStrangeActivity(item)
	if err != nil {
		return model.StrangeActivity{}, err
	}
	key := model.StrangeActivityKey(normalized.Executable, normalized.ImagePath)

	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()
	if s.db == nil {
		return model.StrangeActivity{}, errors.New("database is not open")
	}

	_, err = s.db.Exec(
		`INSERT INTO strange_activity
		 (id, activity_key, name, executable, image_path, product_name, company_name,
		  file_description, confidence, reason, first_seen, last_seen, seen_count, last_pid, model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(activity_key) DO UPDATE SET
		   name = excluded.name,
		   executable = excluded.executable,
		   image_path = excluded.image_path,
		   product_name = excluded.product_name,
		   company_name = excluded.company_name,
		   file_description = excluded.file_description,
		   confidence = excluded.confidence,
		   reason = excluded.reason,
		   last_seen = excluded.last_seen,
		   seen_count = strange_activity.seen_count + 1,
		   last_pid = excluded.last_pid,
		   model = excluded.model`,
		normalized.ID,
		key,
		normalized.Name,
		normalized.Executable,
		normalized.ImagePath,
		normalized.ProductName,
		normalized.CompanyName,
		normalized.FileDescription,
		normalized.Confidence,
		normalized.Reason,
		formatTime(normalized.FirstSeen),
		formatTime(normalized.LastSeen),
		normalized.SeenCount,
		int64(normalized.LastPID),
		normalized.Model,
	)
	if err != nil {
		return model.StrangeActivity{}, fmt.Errorf("save strange activity: %w", err)
	}
	_, _ = s.db.Exec(
		`DELETE FROM strange_activity WHERE id NOT IN (
			SELECT id FROM strange_activity ORDER BY last_seen DESC, id DESC LIMIT ?
		)`,
		strangeActivityMaxRows,
	)
	return s.strangeActivityByKey(key)
}

// RecentStrangeActivity returns newest findings first.
func (s *Store) RecentStrangeActivity(limit int) ([]model.StrangeActivity, error) {
	if limit <= 0 {
		limit = defaultStrangeActivity
	}
	if limit > maxStrangeActivity {
		limit = maxStrangeActivity
	}

	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	// #nosec G202 -- strangeActivityColumns is a compile-time constant and all values are bound parameters.
	rows, err := s.db.Query(
		`SELECT `+strangeActivityColumns+` FROM strange_activity
		 ORDER BY last_seen DESC, id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read strange activity: %w", err)
	}
	defer rows.Close()

	out := make([]model.StrangeActivity, 0, limit)
	for rows.Next() {
		item, err := scanStrangeActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read strange activity: %w", err)
	}
	return out, nil
}

// PromoteStrangeActivity adds a finding to the blocked game list and removes
// the finding in one database transaction.
func (s *Store) PromoteStrangeActivity(id string) (model.Game, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strangeMu.Lock()
	defer s.strangeMu.Unlock()
	if s.db == nil {
		return model.Game{}, errors.New("database is not open")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return model.Game{}, fmt.Errorf("begin strange activity promotion: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// #nosec G202 -- strangeActivityColumns is a compile-time constant and the ID is a bound parameter.
	item, err := scanStrangeActivity(tx.QueryRow(
		`SELECT `+strangeActivityColumns+` FROM strange_activity WHERE id = ?`,
		id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Game{}, ErrStrangeActivityNotFound
	}
	if err != nil {
		return model.Game{}, err
	}

	game, err := normalisePromotedGame(item, s.now().UTC())
	if err != nil {
		return model.Game{}, err
	}
	key := model.GameKey(game)
	var exists int
	err = tx.QueryRow(`SELECT 1 FROM games WHERE game_key = ?`, key).Scan(&exists)
	if err == nil {
		return model.Game{}, ErrDuplicateGame
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Game{}, fmt.Errorf("check existing game: %w", err)
	}

	game.Source = model.GameSourceParent
	if _, err := tx.Exec(
		`INSERT INTO games (id, game_key, name, executable, image_path, source, added_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		game.ID,
		key,
		game.Name,
		game.Executable,
		game.ImagePath,
		game.Source,
		formatTime(game.AddedAt),
	); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return model.Game{}, ErrDuplicateGame
		}
		return model.Game{}, fmt.Errorf("save promoted game: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM ai_ignored WHERE activity_key = ?`,
		model.StrangeActivityKey(game.Executable, game.ImagePath)); err != nil {
		return model.Game{}, fmt.Errorf("clear promoted AI ignore entry: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM strange_activity WHERE id = ?`, id); err != nil {
		return model.Game{}, fmt.Errorf("remove promoted strange activity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Game{}, fmt.Errorf("commit strange activity promotion: %w", err)
	}
	committed = true

	next := cloneConfig(s.cfg)
	next.Games = append(next.Games, game)
	s.cfg = next
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup after game promotion: %v", err))
	}
	return game, nil
}

func (s *Store) strangeActivityByKey(key string) (model.StrangeActivity, error) {
	// #nosec G202 -- strangeActivityColumns is a compile-time constant and the key is a bound parameter.
	item, err := scanStrangeActivity(s.db.QueryRow(
		`SELECT `+strangeActivityColumns+` FROM strange_activity WHERE activity_key = ?`,
		key,
	))
	if err != nil {
		return model.StrangeActivity{}, fmt.Errorf("read saved strange activity: %w", err)
	}
	return item, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanStrangeActivity(scanner rowScanner) (model.StrangeActivity, error) {
	var item model.StrangeActivity
	var firstSeen, lastSeen string
	var lastPID int64
	err := scanner.Scan(
		&item.ID,
		&item.Name,
		&item.Executable,
		&item.ImagePath,
		&item.ProductName,
		&item.CompanyName,
		&item.FileDescription,
		&item.Confidence,
		&item.Reason,
		&firstSeen,
		&lastSeen,
		&item.SeenCount,
		&lastPID,
		&item.Model,
	)
	if err != nil {
		return model.StrangeActivity{}, err
	}
	item.FirstSeen = parseTime(firstSeen)
	item.LastSeen = parseTime(lastSeen)
	if lastPID < 0 {
		lastPID = 0
	}
	item.LastPID = uint32(lastPID)
	return item, nil
}

func normalisePromotedGame(item model.StrangeActivity, now time.Time) (model.Game, error) {
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = strings.TrimSpace(item.ProductName)
	}
	if name == "" {
		name = strings.TrimSuffix(item.Executable, ".exe")
	}
	if name == "" {
		return model.Game{}, errors.New("the finding does not contain a usable game name")
	}
	executable := strings.TrimSpace(item.Executable)
	if executable == "" {
		return model.Game{}, errors.New("the finding does not contain an executable name")
	}
	paths := []string{strings.TrimSpace(item.ImagePath), ""}
	var firstErr error
	for _, imagePath := range paths {
		game, err := model.NormalizeGame(model.Game{
			Name:       name,
			Executable: executable,
			ImagePath:  imagePath,
			AddedAt:    now,
		})
		if err == nil {
			return game, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return model.Game{}, fmt.Errorf("the finding cannot be added to the game list: %w", firstErr)
}
