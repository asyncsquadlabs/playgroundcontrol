package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestStoreStartsBlockedAndPersistsChanges(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st, err := Open(dir, fixedClock(now))
	if err != nil {
		t.Fatal(err)
	}
	if st.PlayAllowed() {
		t.Fatal("fresh store must default to blocked")
	}
	if st.LoadNotice() != "" {
		t.Fatalf("unexpected load notice: %s", st.LoadNotice())
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

	reloaded, err := Open(dir, fixedClock(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
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
}

func TestStoreRejectsDuplicateAndInvalidGames(t *testing.T) {
	st, err := Open(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGame(model.Game{Name: "One", Executable: "game.exe"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGame(model.Game{Name: "Two", Executable: "GAME.EXE"}); err == nil {
		t.Fatal("expected duplicate error")
	}
	if _, err := st.AddGame(model.Game{Name: "Explorer", Executable: "explorer.exe"}); err == nil {
		t.Fatal("expected protected process error")
	}
}

func TestStoreDeletesGame(t *testing.T) {
	st, err := Open(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
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

func TestStoreRecoversBackupAndFailsClosedWithoutOne(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlayAllowed(true, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddGame(model.Game{Name: "Example", Executable: "game.exe"}); err != nil {
		t.Fatal(err)
	}
	// AddGame backed up the prior allowed/no-games configuration, then wrote
	// the current allowed/one-game configuration.
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.PlayAllowed() {
		t.Fatal("expected the backup's play permission")
	}
	if len(recovered.Games()) != 0 {
		t.Fatalf("expected the backup without games, got %+v", recovered.Games())
	}
	if recovered.LoadNotice() == "" {
		t.Fatal("expected a load warning")
	}

	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, configFile), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	failedClosed, err := Open(dir2, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if failedClosed.PlayAllowed() {
		t.Fatal("corrupt configuration must fail closed")
	}
	if len(failedClosed.Games()) != 0 {
		t.Fatal("corrupt configuration should start with no games")
	}
}

func TestAuditEventsAndLimit(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st, err := Open(dir, fixedClock(now))
	if err != nil {
		t.Fatal(err)
	}
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
	info, err := os.Stat(filepath.Join(dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("audit log is too permissive: %v", info.Mode())
	}
}

func TestAuditRotation(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, auditFile)
	line := `{"time":"2026-10-27T12:00:00Z","type":"system","message":"old"}` + "\n"
	repeats := auditMaxBytes/len(line) + 10
	payload := make([]byte, 0, repeats*len(line))
	for i := 0; i < repeats; i++ {
		payload = append(payload, line...)
	}
	if err := os.WriteFile(current, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEvent(model.AuditEvent{Type: model.EventSystem, Message: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, auditRotated)); err != nil {
		t.Fatalf("rotated audit file missing: %v", err)
	}
	events, err := st.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 10 {
		t.Fatalf("expected 10 recent events, got %d", len(events))
	}
	if events[len(events)-1].Message != "new" {
		t.Fatalf("latest event was not new: %+v", events[len(events)-1])
	}
}
