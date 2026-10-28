package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func openStore(t *testing.T, dir string, now func() time.Time) *Store {
	t.Helper()
	st, err := Open(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestStoreStartsBlockedAndPersistsChanges(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st := openStore(t, dir, fixedClock(now))
	if st.PlayAllowed() {
		t.Fatal("fresh store must default to blocked")
	}
	if _, err := os.Stat(st.DatabasePath()); err != nil {
		t.Fatalf("database was not created: %v", err)
	}

	if err := st.SetPlayAllowed(true, "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	game, err := st.AddGame(model.Game{Name: "Example", Executable: "game.exe"})
	if err != nil {
		t.Fatal(err)
	}
	if game.ID == "" {
		t.Fatal("game ID was not generated")
	}
	if err := st.SetPlayAllowed(false, "192.168.1.21"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded := openStore(t, dir, fixedClock(now.Add(time.Hour)))
	cfg := reloaded.Config()
	if cfg.PlayAllowed {
		t.Fatal("blocked state did not persist")
	}
	if len(cfg.Games) != 1 || cfg.Games[0].Name != "Example" {
		t.Fatalf("games did not persist: %+v", cfg.Games)
	}
	if cfg.LastChangedAt.IsZero() {
		t.Fatal("last changed timestamp was not persisted")
	}
	if _, err := os.Stat(filepath.Join(dir, backupFileName)); err != nil {
		t.Fatalf("database backup was not created: %v", err)
	}
}

func TestStoreRejectsDuplicateAndInvalidGames(t *testing.T) {
	st := openStore(t, t.TempDir(), time.Now)
	if _, err := st.AddGame(model.Game{Name: "One", Executable: "game.exe"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGame(model.Game{Name: "Two", Executable: "GAME.EXE"}); !errors.Is(err, ErrDuplicateGame) {
		t.Fatalf("expected duplicate error, got %v", err)
	}
	if _, err := st.AddGame(model.Game{Name: "Explorer", Executable: "explorer.exe"}); err == nil {
		t.Fatal("expected protected process error")
	}
}

func TestStoreDeletesGame(t *testing.T) {
	st := openStore(t, t.TempDir(), time.Now)
	game, err := st.AddGame(model.Game{Name: "Example", Executable: "game.exe"})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := st.DeleteGame(game.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("game was not removed")
	}
	if len(st.Games()) != 0 {
		t.Fatal("game list was not updated")
	}
	removed, err = st.DeleteGame("missing")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("unknown game reported as removed")
	}
}

func TestInvalidSavedGameIsSkippedWithWarning(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir, time.Now)
	if _, err := st.db.Exec(
		`INSERT INTO games (id, game_key, name, executable, image_path, added_at) VALUES (?, ?, ?, ?, '', ?)`,
		"bad-id", "bad-key", "Explorer", "explorer.exe", formatTime(time.Now()),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reloaded := openStore(t, dir, time.Now)
	if len(reloaded.Games()) != 0 {
		t.Fatalf("invalid game was loaded: %+v", reloaded.Games())
	}
	if !strings.Contains(reloaded.LoadNotice(), "ignored invalid saved game") {
		t.Fatalf("missing invalid-game warning: %q", reloaded.LoadNotice())
	}
}

func TestAuditEventsAndLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st := openStore(t, dir, fixedClock(now))
	for i, message := range []string{"one", "two", "three"} {
		if err := st.AddEvent(model.AuditEvent{
			Type:    model.EventSystem,
			Message: message,
			Time:    now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := st.RecentEvents(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Message != "two" || events[1].Message != "three" {
		t.Fatalf("unexpected events: %+v", events)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("audit count = %d, want 3", count)
	}
}

func TestAuditRetentionKeepsNewestRows(t *testing.T) {
	st := openStore(t, t.TempDir(), time.Now)
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare(`INSERT INTO audit_events (time, type, message) VALUES (?, 'system', 'old')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < auditMaxRows+10; i++ {
		if _, err := statement.Exec(formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	_ = statement.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEvent(model.AuditEvent{Type: model.EventSystem, Message: "newest"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > auditMaxRows {
		t.Fatalf("audit count = %d, want at most %d", count, auditMaxRows)
	}
	events, err := st.RecentEvents(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Message != "newest" {
		t.Fatalf("unexpected newest event: %+v", events)
	}
}

func TestAuthRecordPersistenceAndClear(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir, time.Now)
	record, err := auth.HashPIN("123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAuthRecord(record); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := st.LoadAuthRecord()
	if err != nil || !ok {
		t.Fatalf("LoadAuthRecord = %+v, %t, %v", loaded, ok, err)
	}
	if loaded.Hash != record.Hash {
		t.Fatal("auth record did not round-trip")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded := openStore(t, dir, time.Now)
	loaded, ok, err = reloaded.LoadAuthRecord()
	if err != nil || !ok {
		t.Fatalf("reloaded LoadAuthRecord = %+v, %t, %v", loaded, ok, err)
	}
	if err := reloaded.ClearAuthRecord(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := reloaded.LoadAuthRecord(); err != nil || ok {
		t.Fatalf("record still present after clear: %t, %v", ok, err)
	}
}

func TestLegacyImportMigratesAllState(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	game, err := model.NormalizeGame(model.Game{Name: "Example", Executable: "game.exe"})
	if err != nil {
		t.Fatal(err)
	}
	game.ID = "legacy-game"
	game.AddedAt = now
	cfg := model.DefaultConfig()
	cfg.PlayAllowed = true
	cfg.LastChangedAt = now
	cfg.LastChangedBy = "192.168.1.20"
	cfg.Games = []model.Game{game}
	writeJSON(t, filepath.Join(dir, legacyConfigFile), cfg)

	record, err := auth.HashPIN("123456")
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, legacyAuthFile), record)

	for name, message := range map[string]string{legacyAuditRotated: "old", legacyAuditFile: "new"} {
		writeJSONLine(t, filepath.Join(dir, name), model.AuditEvent{
			Time:    now,
			Type:    model.EventSystem,
			Message: message,
		})
	}

	st := openStore(t, dir, fixedClock(now))
	cfg = st.Config()
	if !cfg.PlayAllowed || len(cfg.Games) != 1 || cfg.Games[0].ID != "legacy-game" {
		t.Fatalf("legacy config did not migrate: %+v", cfg)
	}
	if _, ok, err := st.LoadAuthRecord(); err != nil || !ok {
		t.Fatalf("legacy auth did not migrate: %t, %v", ok, err)
	}
	events, err := st.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Message != "old" || events[1].Message != "new" {
		t.Fatalf("legacy audit did not migrate: %+v", events)
	}
	for _, name := range []string{legacyConfigFile, legacyAuthFile, legacyAuditFile, legacyAuditRotated} {
		if _, err := os.Stat(filepath.Join(dir, name+".migrated")); err != nil {
			t.Fatalf("%s was not archived: %v", name, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded := openStore(t, dir, fixedClock(now))
	if len(reloaded.Games()) != 1 {
		t.Fatalf("legacy import was repeated: %+v", reloaded.Games())
	}
	events, err = reloaded.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("legacy audit was imported twice: %+v", events)
	}
}

func TestLegacyConfigBackupRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyConfigFile), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := model.DefaultConfig()
	cfg.PlayAllowed = true
	writeJSON(t, filepath.Join(dir, legacyConfigBackup), cfg)

	st := openStore(t, dir, time.Now)
	if !st.PlayAllowed() {
		t.Fatal("legacy configuration backup was not imported")
	}
	if !strings.Contains(st.LoadNotice(), "recovered the last configuration backup") {
		t.Fatalf("missing configuration backup warning: %q", st.LoadNotice())
	}
	for _, name := range []string{legacyConfigFile, legacyConfigBackup} {
		if _, err := os.Stat(filepath.Join(dir, name+".migrated")); err != nil {
			t.Fatalf("%s was not archived: %v", name, err)
		}
	}
}

func TestCorruptDatabaseRestoresBackup(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir, time.Now)
	if err := st.SetPlayAllowed(true, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGame(model.Game{Name: "Example", Executable: "game.exe"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAuthRecord(mustRecord(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(st.DatabasePath(), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(st.DatabasePath() + "-wal")
	_ = os.Remove(st.DatabasePath() + "-shm")

	recovered := openStore(t, dir, time.Now)
	if !strings.Contains(recovered.LoadNotice(), "restored from last-known-good backup") {
		t.Fatalf("missing recovery notice: %q", recovered.LoadNotice())
	}
	if !recovered.PlayAllowed() {
		t.Fatal("recovered play state is blocked")
	}
	if len(recovered.Games()) != 1 {
		t.Fatalf("recovered games = %+v", recovered.Games())
	}
	if _, ok, err := recovered.LoadAuthRecord(); err != nil || !ok {
		t.Fatalf("recovered auth record = %t, %v", ok, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, databaseFileName+".corrupt-*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("corrupt database was not archived: %v, %v", matches, err)
	}
}

func TestCorruptDatabaseWithoutBackupStartsFresh(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir, time.Now)
	if err := st.SetPlayAllowed(true, "local"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAuthRecord(mustRecord(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, backupFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.DatabasePath(), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(st.DatabasePath() + "-wal")
	_ = os.Remove(st.DatabasePath() + "-shm")

	fresh := openStore(t, dir, time.Now)
	if !strings.Contains(fresh.LoadNotice(), "unusable database archived") {
		t.Fatalf("missing reset notice: %q", fresh.LoadNotice())
	}
	if fresh.PlayAllowed() {
		t.Fatal("fresh database should start blocked")
	}
	if _, ok, err := fresh.LoadAuthRecord(); err != nil || ok {
		t.Fatalf("fresh database should require setup: %t, %v", ok, err)
	}
}

func TestCorruptDatabaseAndBackupStartsFresh(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir, time.Now)
	if err := st.SetPlayAllowed(true, "local"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.DatabasePath(), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupFileName), []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(st.DatabasePath() + "-wal")
	_ = os.Remove(st.DatabasePath() + "-shm")

	fresh := openStore(t, dir, time.Now)
	if !strings.Contains(fresh.LoadNotice(), "last-known-good backup was not usable") {
		t.Fatalf("missing unusable-backup notice: %q", fresh.LoadNotice())
	}
	if fresh.PlayAllowed() {
		t.Fatal("fresh database should start blocked")
	}
}

func TestMissingDatabaseRestoresBackup(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir, time.Now)
	if err := st.SetPlayAllowed(true, "local"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(st.DatabasePath() + suffix)
	}
	recovered := openStore(t, dir, time.Now)
	if !recovered.PlayAllowed() {
		t.Fatal("backup restore did not preserve play state")
	}
	if !strings.Contains(recovered.LoadNotice(), "restored from last-known-good backup") {
		t.Fatalf("missing restore notice: %q", recovered.LoadNotice())
	}
}

func TestInvalidLegacyAuthRequiresSetupWithWarning(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, legacyAuthFile), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := openStore(t, dir, time.Now)
	if _, ok, err := st.LoadAuthRecord(); err != nil || ok {
		t.Fatalf("invalid legacy auth should not import: %t, %v", ok, err)
	}
	if !strings.Contains(st.LoadNotice(), "setup will be required") {
		t.Fatalf("missing invalid-auth warning: %q", st.LoadNotice())
	}
}

func TestConcurrentReadsEventsAndWrites(t *testing.T) {
	st := openStore(t, t.TempDir(), time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = st.AddEvent(model.AuditEvent{Type: model.EventSystem, Message: "concurrent"})
				_ = st.Games()
				_ = st.PlayAllowed()
				_, _ = st.RecentEvents(5)
			}
		}()
	}
	wg.Wait()
	events, err := st.RecentEvents(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("recent events = %d, want 5", len(events))
	}
}

func mustRecord(t *testing.T) auth.Record {
	t.Helper()
	record, err := auth.HashPIN("123456")
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONLine(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
