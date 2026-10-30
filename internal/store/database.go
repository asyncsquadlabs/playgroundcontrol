package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the pure-Go SQLite driver
)

const (
	databaseFileName = "playground.db"
	backupFileName   = "playground.db.bak"
	auditMaxRows     = 50_000
	maxCorruptFiles  = 3
	openRetryWindow  = 15 * time.Second
	openRetryDelay   = 500 * time.Millisecond
)

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS app_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		play_allowed INTEGER NOT NULL DEFAULT 0 CHECK (play_allowed IN (0, 1)),
		last_changed_at TEXT,
		last_changed_by TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS games (
		id TEXT PRIMARY KEY,
		game_key TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		executable TEXT NOT NULL,
		image_path TEXT NOT NULL DEFAULT '',
		added_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS auth_record (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		version INTEGER NOT NULL,
		algorithm TEXT NOT NULL,
		salt TEXT NOT NULL,
		hash TEXT NOT NULL,
		time INTEGER NOT NULL,
		memory_kib INTEGER NOT NULL,
		threads INTEGER NOT NULL,
		key_len INTEGER NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS audit_events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		time TEXT NOT NULL,
		type TEXT NOT NULL,
		message TEXT NOT NULL,
		actor_ip TEXT NOT NULL DEFAULT '',
		game_id TEXT NOT NULL DEFAULT '',
		game_name TEXT NOT NULL DEFAULT '',
		pid INTEGER NOT NULL DEFAULT 0,
		result TEXT NOT NULL DEFAULT ''
	)`,
}

var schemaV2Statements = []string{
	`CREATE TABLE IF NOT EXISTS ai_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
		auto_block INTEGER NOT NULL DEFAULT 1,
		model TEXT NOT NULL DEFAULT 'deepseek-flash',
		api_key_enc BLOB NOT NULL DEFAULT X'',
		key_hint TEXT NOT NULL DEFAULT '',
		last_verified_at TEXT,
		last_error TEXT NOT NULL DEFAULT '',
		last_error_at TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS strange_activity (
		id TEXT PRIMARY KEY,
		activity_key TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		executable TEXT NOT NULL,
		image_path TEXT NOT NULL DEFAULT '',
		product_name TEXT NOT NULL DEFAULT '',
		company_name TEXT NOT NULL DEFAULT '',
		file_description TEXT NOT NULL DEFAULT '',
		confidence REAL NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		first_seen TEXT NOT NULL,
		last_seen TEXT NOT NULL,
		seen_count INTEGER NOT NULL DEFAULT 1,
		last_pid INTEGER NOT NULL DEFAULT 0,
		model TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_strange_activity_last_seen
		ON strange_activity(last_seen DESC, id DESC)`,
	`CREATE TABLE IF NOT EXISTS ai_ignored (
		activity_key TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		executable TEXT NOT NULL,
		image_path TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
}

func databasePath(dir string) string {
	return filepath.Join(dir, databaseFileName)
}

func backupPath(dir string) string {
	return filepath.Join(dir, backupFileName)
}

// Close checkpoints the WAL, refreshes the last-known-good backup, and closes
// the database. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.db == nil {
			return
		}
		_ = s.refreshBackup()
		_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		s.closeErr = s.db.Close()
		s.db = nil
	})
	return s.closeErr
}

// RefreshBackup writes a consistent snapshot of the current database to the
// last-known-good backup file.
func (s *Store) RefreshBackup() error {
	return s.refreshBackup()
}

func (s *Store) openDatabase() (*sql.DB, string, error) {
	var notices []string

	if !regularFileExists(s.dbPath) && regularFileExists(s.backupPath) {
		if err := s.restoreBackup(); err != nil {
			notices = append(notices, fmt.Sprintf("database backup could not be restored: %v", err))
		} else {
			notices = append(notices, "database restored from last-known-good backup")
		}
	}

	db, err := s.openPrimaryWithRetry()
	if err == nil {
		warnings, initErr := s.initializeOpened(db)
		if initErr != nil {
			_ = db.Close()
			return nil, "", initErr
		}
		notices = append(notices, warnings...)
		return db, joinNotices(notices), nil
	}

	openErr := err
	if regularFileExists(s.dbPath) {
		if archiveErr := s.archiveUnusableDatabase(); archiveErr != nil {
			return nil, "", fmt.Errorf("open database: %w; archive unusable database: %v", openErr, archiveErr)
		}
		notices = append(notices, fmt.Sprintf("unusable database archived after: %v", openErr))
	} else {
		_ = os.Remove(s.dbPath + "-wal")
		_ = os.Remove(s.dbPath + "-shm")
		notices = append(notices, fmt.Sprintf("database could not be opened: %v", openErr))
	}

	if regularFileExists(s.backupPath) {
		if restoreErr := s.restoreBackup(); restoreErr != nil {
			notices = append(notices, fmt.Sprintf("last-known-good backup was not usable: %v", restoreErr))
		} else {
			db, retryErr := s.openPrimaryWithRetry()
			if retryErr == nil {
				warnings, initErr := s.initializeOpened(db)
				if initErr != nil {
					_ = db.Close()
					return nil, "", initErr
				}
				notices = append(notices, "database restored from last-known-good backup")
				notices = append(notices, warnings...)
				return db, joinNotices(notices), nil
			}
			notices = append(notices, fmt.Sprintf("restored backup could not be opened: %v", retryErr))
		}
	}

	if err := s.createFreshDatabase(); err != nil {
		return nil, "", fmt.Errorf("create fresh database after open failure (%v): %w", openErr, err)
	}
	db, err = s.openPrimaryWithRetry()
	if err != nil {
		return nil, "", fmt.Errorf("open fresh database: %w", err)
	}
	warnings, initErr := s.initializeOpened(db)
	if initErr != nil {
		_ = db.Close()
		return nil, "", initErr
	}
	notices = append(notices, warnings...)
	return db, joinNotices(notices), nil
}

func (s *Store) initializeOpened(db *sql.DB) ([]string, error) {
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return s.importLegacy(db)
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > 2 {
		return fmt.Errorf("database schema version %d is newer than the supported version 2", version)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	statements := append([]string(nil), schemaStatements...)
	statements = append(statements, schemaV2Statements...)
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	if err := ensureColumn(tx, "games", "source",
		`ALTER TABLE games ADD COLUMN source TEXT NOT NULL DEFAULT 'parent'`); err != nil {
		return err
	}
	if err := ensureColumn(tx, "ai_settings", "auto_block",
		`ALTER TABLE ai_settings ADD COLUMN auto_block INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO app_state (id, play_allowed, last_changed_at, last_changed_by) VALUES (1, 0, NULL, '')`); err != nil {
		return fmt.Errorf("initialize app state: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO ai_settings (id, enabled, auto_block, model, api_key_enc, key_hint, last_verified_at, last_error, last_error_at) VALUES (1, 0, 1, ?, X'', '', NULL, '', NULL)`, "deepseek-flash"); err != nil {
		return fmt.Errorf("initialize AI settings: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	committed = true
	return nil
}

func ensureColumn(tx *sql.Tx, table, column, alter string) error {
	// #nosec G201 -- table is a compile-time constant supplied by this file.
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("inspect %s schema: %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	if _, err := tx.Exec(alter); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) openPrimaryWithRetry() (*sql.DB, error) {
	deadline := time.Now().Add(openRetryWindow)
	for {
		db, err := s.openPath(s.dbPath)
		if err == nil {
			return db, nil
		}
		if !isTransientOpenError(err) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(openRetryDelay)
	}
}

func (s *Store) openPath(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	closeWith := func(err error) (*sql.DB, error) {
		_ = db.Close()
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return closeWith(fmt.Errorf("ping SQLite: %w", err))
	}
	for _, pragma := range []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA synchronous = FULL`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			return closeWith(fmt.Errorf("%s: %w", pragma, err))
		}
	}
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode); err != nil {
		return closeWith(fmt.Errorf("enable WAL mode: %w", err))
	}
	if !strings.EqualFold(journalMode, "wal") {
		return closeWith(fmt.Errorf("SQLite refused WAL mode (got %q)", journalMode))
	}

	rows, err := db.Query(`PRAGMA quick_check`)
	if err != nil {
		return closeWith(fmt.Errorf("run database quick check: %w", err))
	}
	var problems []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			_ = rows.Close()
			return closeWith(fmt.Errorf("read database quick check: %w", err))
		}
		if !strings.EqualFold(result, "ok") {
			problems = append(problems, result)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return closeWith(fmt.Errorf("read database quick check: %w", err))
	}
	_ = rows.Close()
	if len(problems) > 0 {
		return closeWith(fmt.Errorf("database quick check failed: %s", strings.Join(problems, "; ")))
	}

	// BEGIN IMMEDIATE verifies that the service can write, not merely read.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return closeWith(fmt.Errorf("database is not writable: %w", err))
	}
	if _, err := db.Exec(`ROLLBACK`); err != nil {
		return closeWith(fmt.Errorf("database write probe failed: %w", err))
	}
	return db, nil
}

func (s *Store) restoreBackup() error {
	temp := s.dbPath + ".restore"
	for _, path := range []string{temp, temp + "-wal", temp + "-shm"} {
		_ = os.Remove(path)
	}
	if err := copyFile(s.backupPath, temp, 0o600); err != nil {
		return fmt.Errorf("copy backup: %w", err)
	}
	db, err := s.openPath(temp)
	if err != nil {
		_ = os.Remove(temp)
		_ = os.Remove(temp + "-wal")
		_ = os.Remove(temp + "-shm")
		return fmt.Errorf("validate backup: %w", err)
	}
	_ = db.Close()
	_ = os.Remove(temp + "-wal")
	_ = os.Remove(temp + "-shm")

	for _, path := range []string{s.dbPath, s.dbPath + "-wal", s.dbPath + "-shm"} {
		if err := removePath(path); err != nil {
			return fmt.Errorf("remove old database before restore: %w", err)
		}
	}
	if err := os.Rename(temp, s.dbPath); err != nil {
		return fmt.Errorf("install restored backup: %w", err)
	}
	return nil
}

func (s *Store) createFreshDatabase() error {
	for _, path := range []string{s.dbPath, s.dbPath + "-wal", s.dbPath + "-shm"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) archiveUnusableDatabase() error {
	if !regularFileExists(s.dbPath) {
		return nil
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	archive := s.dbPath + ".corrupt-" + stamp
	if err := os.Rename(s.dbPath, archive); err != nil {
		return fmt.Errorf("archive database: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := s.dbPath + suffix
		if !regularFileExists(sidecar) {
			continue
		}
		if err := os.Rename(sidecar, archive+suffix); err != nil {
			if removeErr := removePath(sidecar); removeErr != nil {
				return fmt.Errorf("archive database sidecar %s: %w (remove failed: %v)", sidecar, err, removeErr)
			}
		}
	}
	s.pruneCorruptArchives()
	return nil
}

func (s *Store) pruneCorruptArchives() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	prefix := databaseFileName + ".corrupt-"
	var archives []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			continue
		}
		archives = append(archives, filepath.Join(s.dir, name))
	}
	sort.Strings(archives)
	for len(archives) > maxCorruptFiles {
		remove := archives[0]
		archives = archives[1:]
		_ = os.Remove(remove)
		_ = os.Remove(remove + "-wal")
		_ = os.Remove(remove + "-shm")
	}
}

func (s *Store) refreshBackup() error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	temp := s.backupPath + ".tmp"
	_ = os.Remove(temp)
	if _, err := s.db.Exec(`VACUUM INTO ?`, temp); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("write database snapshot: %w", err)
	}
	if err := os.Chmod(temp, 0o600); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("secure database snapshot: %w", err)
	}
	if err := os.Rename(temp, s.backupPath); err != nil {
		_ = os.Remove(temp)
		return fmt.Errorf("install database snapshot: %w", err)
	}
	return nil
}

func isTransientOpenError(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"locked",
		"busy",
		"sharing violation",
		"unable to open database file",
		"access is denied",
		"permission denied",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func removePath(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	src, err := os.Open(source) // #nosec G304 -- callers pass fixed backup paths under the protected data directory.
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) // #nosec G304 -- callers pass fixed restore paths under the protected data directory.
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, mode)
}

func joinNotices(notices []string) string {
	out := make([]string, 0, len(notices))
	for _, notice := range notices {
		notice = strings.TrimSpace(notice)
		if notice != "" {
			out = append(out, notice)
		}
	}
	return strings.Join(out, "; ")
}
