package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

type testCodec struct {
	key byte
}

func (c testCodec) Seal(plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext)+2)
	out[0] = 'P'
	out[1] = c.key
	for i, b := range plaintext {
		out[i+2] = b ^ c.key
	}
	return out, nil
}

func (c testCodec) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 2 || ciphertext[0] != 'P' || ciphertext[1] != c.key {
		return nil, errors.New("test codec cannot open secret")
	}
	out := make([]byte, len(ciphertext)-2)
	for i, b := range ciphertext[2:] {
		out[i] = b ^ c.key
	}
	return out, nil
}

func TestAISettingsAreSealedPersistedAndRedacted(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	st, err := OpenWithCodec(dir, fixedClock(now), testCodec{key: 0x5a})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.SaveAISettings(false, true, "deepseek-flash", "", false); err != nil {
		t.Fatalf("save default AI settings without a key: %v", err)
	}
	settings, err := st.SaveAISettings(false, true, "deepseek-flash", "sk-test-key-1234", false)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.KeyConfigured || settings.KeyHint != "1234" || settings.Enabled || !settings.AutoBlock {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	var raw []byte
	if err := st.db.QueryRow(`SELECT api_key_enc FROM ai_settings WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || strings.Contains(string(raw), "sk-test-key") {
		t.Fatalf("key was not sealed: %q", raw)
	}
	if _, err := st.SaveAISettings(true, true, "deepseek-flash", "", false); err != nil {
		t.Fatalf("enable with existing key: %v", err)
	}
	if _, err := st.SaveAISettings(true, true, "deepseek-flash", "", false); err != nil {
		t.Fatalf("update with existing key: %v", err)
	}
	if _, err := st.SaveAISettings(true, true, "deepseek-flash", "", true); !errors.Is(err, ErrAIConfiguration) {
		t.Fatalf("clear while enabled error = %v", err)
	}

	reloaded, err := OpenWithCodec(dir, fixedClock(now), testCodec{key: 0x5a})
	if err != nil {
		t.Fatal(err)
	}
	view, key, ok := reloaded.AICredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != "sk-test-key-1234" || !view.KeyConfigured {
		t.Fatalf("reloaded credentials = %+v key=%q ok=%t", view, key, ok)
	}
	_ = reloaded.Close()

	wrongCodec, err := OpenWithCodec(dir, fixedClock(now), testCodec{key: 0x11})
	if err != nil {
		// A decrypt failure is non-fatal; the store should still start.
		t.Fatal(err)
	}
	defer wrongCodec.Close()
	view, key, ok = wrongCodec.AICredentials()
	if ok || key != "" || view.KeyConfigured {
		t.Fatalf("wrong codec exposed credentials: %+v key=%q ok=%t", view, key, ok)
	}
}

func TestStoreMigratesV1ToV3PreservingGames(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, databaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range schemaStatements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('legacy_imported', '1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO app_state (id, play_allowed, last_changed_at, last_changed_by) VALUES (1, 1, ?, 'parent')`, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO games (id, game_key, name, executable, image_path, added_at) VALUES ('g1', 'game.exe', 'Existing', 'game.exe', '', ?)`, formatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenWithCodec(dir, fixedClock(time.Now()), testCodec{key: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var version int
	if err := st.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("user_version = %d, want 3", version)
	}
	if cfg := st.Config(); len(cfg.Games) != 1 || cfg.Games[0].Name != "Existing" ||
		cfg.Games[0].Source != model.GameSourceParent || !cfg.PlayAllowed {
		t.Fatalf("migration did not preserve config: %+v", cfg)
	}
	if settings := st.AISettings(); settings.Model != model.DefaultAIModel || settings.Enabled || !settings.AutoBlock {
		t.Fatalf("unexpected default AI settings: %+v", settings)
	}
	var ignoredTable int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'ai_ignored'`).Scan(&ignoredTable); err != nil || ignoredTable != 1 {
		t.Fatalf("ai_ignored table missing: count=%d err=%v", ignoredTable, err)
	}
	var browserTable int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'browser_domains'`).Scan(&browserTable); err != nil || browserTable != 1 {
		t.Fatalf("browser_domains table missing: count=%d err=%v", browserTable, err)
	}
	if settings := st.BrowserSettings(); settings.Enabled || !settings.AutoBlock || settings.AIClassification {
		t.Fatalf("unexpected default browser settings: %+v", settings)
	}
}

func TestStrangeActivityDeduplicatesAndPromotes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	clock := now
	st, err := OpenWithCodec(dir, func() time.Time { return clock }, testCodec{key: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	item := model.StrangeActivity{
		Name:       "Example Game",
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
		Confidence: 0.91,
		Reason:     "Interactive game client.",
		Model:      model.DefaultAIModel,
		FirstSeen:  now,
		LastSeen:   now,
		SeenCount:  1,
		LastPID:    42,
	}
	first, err := st.RecordStrangeActivity(item)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.SeenCount != 1 {
		t.Fatalf("unexpected first finding: %+v", first)
	}
	clock = now.Add(time.Minute)
	item.Name = "Example Game Updated"
	item.Confidence = 0.97
	item.LastSeen = clock
	second, err := st.RecordStrangeActivity(item)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.SeenCount != 2 || second.Name != "Example Game Updated" {
		t.Fatalf("dedupe failed: %+v", second)
	}
	items, err := st.RecentStrangeActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SeenCount != 2 {
		t.Fatalf("unexpected findings: %+v", items)
	}

	key := model.StrangeActivityKey(item.Executable, item.ImagePath)
	if _, err := st.db.Exec(
		`INSERT INTO ai_ignored (activity_key, name, executable, image_path, created_at) VALUES (?, ?, ?, ?, ?)`,
		key, item.Name, item.Executable, item.ImagePath, formatTime(now),
	); err != nil {
		t.Fatal(err)
	}
	game, err := st.PromoteStrangeActivity(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if game.Executable != "game.exe" || game.ImagePath != `C:\Games\game.exe` || game.Source != model.GameSourceParent {
		t.Fatalf("unexpected promoted game: %+v", game)
	}
	ignoredKeys, err := st.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignoredKeys[key]; ok {
		t.Fatal("promotion did not clear the AI ignore entry")
	}
	items, err = st.RecentStrangeActivity(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("promoted finding remained: %+v", items)
	}

	// A duplicate promotion must leave the finding available for dismissal.
	if _, err := st.RecordStrangeActivity(item); err != nil {
		t.Fatal(err)
	}
	items, err = st.RecentStrangeActivity(10)
	if err != nil || len(items) != 1 {
		t.Fatalf("re-record finding: %+v %v", items, err)
	}
	if _, err := st.PromoteStrangeActivity(items[0].ID); !errors.Is(err, ErrDuplicateGame) {
		t.Fatalf("duplicate promotion error = %v", err)
	}
	if removed, err := st.IgnoreStrangeActivity(items[0].ID); err != nil || !removed {
		t.Fatalf("ignore = %t, %v", removed, err)
	}
	ignoredKeys, err = st.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignoredKeys[key]; !ok {
		t.Fatal("ignored finding did not create an AI ignore entry")
	}
	if _, err := st.PromoteStrangeActivity(items[0].ID); !errors.Is(err, ErrStrangeActivityNotFound) {
		t.Fatalf("missing promotion error = %v", err)
	}
}

func TestAIGameDeleteCreatesIgnoreAndManualAddClearsIt(t *testing.T) {
	st := openStore(t, t.TempDir(), fixedClock(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)))
	if _, err := st.SaveAISettings(true, true, model.DefaultAIModel, "sk-test-key-1234", false); err != nil {
		t.Fatal(err)
	}
	game, skipped, err := st.AddAIGame(model.Game{
		Name:       "Example Game",
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
		Source:     model.GameSourceAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("AI add was unexpectedly skipped")
	}
	if game.Source != model.GameSourceAI {
		t.Fatalf("game source = %q, want ai", game.Source)
	}
	key := model.StrangeActivityKey(game.Executable, game.ImagePath)
	if removed, err := st.DeleteGame(game.ID); err != nil || !removed {
		t.Fatalf("delete AI game = %t, %v", removed, err)
	}
	ignoredKeys, err := st.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignoredKeys[key]; !ok {
		t.Fatal("deleting an AI game did not create an ignore entry")
	}

	manualAdd, err := st.AddGame(model.Game{
		Name:       "Example Game",
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
		Source:     model.GameSourceAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manualAdd.Source != model.GameSourceParent {
		t.Fatalf("manual add source = %q, want parent", manualAdd.Source)
	}
	ignoredKeys, err = st.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignoredKeys[key]; ok {
		t.Fatal("manual add did not clear the AI ignore entry")
	}
	manual, err := st.AddGame(model.Game{
		Name:       "Manual Game",
		Executable: "manual.exe",
		ImagePath:  `C:\Games\manual.exe`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := st.DeleteGame(manual.ID); err != nil || !removed {
		t.Fatalf("delete manual game = %t, %v", removed, err)
	}
	ignoredKeys, err = st.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignoredKeys[model.StrangeActivityKey(manual.Executable, manual.ImagePath)]; ok {
		t.Fatal("deleting a manual game created an AI ignore entry")
	}
}

func TestAddAIGameAtomicallySkipsIgnoredOrDisallowed(t *testing.T) {
	st := openStore(t, t.TempDir(), fixedClock(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)))
	if _, err := st.SaveAISettings(true, true, model.DefaultAIModel, "sk-test-key-1234", false); err != nil {
		t.Fatal(err)
	}

	ignoredGame := model.Game{
		Name:       "Ignored Game",
		Executable: "ignored.exe",
		ImagePath:  `C:\Games\ignored.exe`,
	}
	if _, err := st.db.Exec(
		`INSERT INTO ai_ignored (activity_key, name, executable, image_path, created_at) VALUES (?, ?, ?, ?, ?)`,
		model.StrangeActivityKey(ignoredGame.Executable, ignoredGame.ImagePath),
		ignoredGame.Name, ignoredGame.Executable, ignoredGame.ImagePath, formatTime(time.Now()),
	); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := st.AddAIGame(ignoredGame); err != nil || !skipped {
		t.Fatalf("ignored AI add = skipped %t, err %v", skipped, err)
	}
	if len(st.Games()) != 0 {
		t.Fatalf("ignored AI game was added: %+v", st.Games())
	}

	allowedGame := model.Game{
		Name:       "Allowed Game",
		Executable: "allowed.exe",
		ImagePath:  `C:\Games\allowed.exe`,
	}
	if err := st.SetPlayAllowed(true, "test"); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := st.AddAIGame(allowedGame); err != nil || !skipped {
		t.Fatalf("allowed-play AI add = skipped %t, err %v", skipped, err)
	}
	if err := st.SetPlayAllowed(false, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveAISettings(true, false, model.DefaultAIModel, "", false); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := st.AddAIGame(allowedGame); err != nil || !skipped {
		t.Fatalf("disabled auto-block AI add = skipped %t, err %v", skipped, err)
	}
	if _, err := st.SaveAISettings(true, true, model.DefaultAIModel, "", false); err != nil {
		t.Fatal(err)
	}
	game, skipped, err := st.AddAIGame(allowedGame)
	if err != nil || skipped {
		t.Fatalf("enabled AI add = skipped %t, err %v", skipped, err)
	}
	if game.Source != model.GameSourceAI {
		t.Fatalf("AI game source = %q", game.Source)
	}
}

func TestAIStatusPersists(t *testing.T) {
	st := openStore(t, t.TempDir(), fixedClock(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)))
	if _, err := st.SaveAISettings(false, true, model.DefaultAIModel, "sk-test-key-1234", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAIStatus(time.Time{}, "provider unavailable"); err != nil {
		t.Fatal(err)
	}
	if got := st.AISettings().LastError; got != "provider unavailable" {
		t.Fatalf("last error = %q", got)
	}
	verified := time.Date(2026, 10, 28, 13, 0, 0, 0, time.UTC)
	if err := st.SetAIStatus(verified, ""); err != nil {
		t.Fatal(err)
	}
	settings := st.AISettings()
	if !settings.LastVerifiedAt.Equal(verified) || settings.LastError != "" {
		t.Fatalf("settings = %+v", settings)
	}
}
