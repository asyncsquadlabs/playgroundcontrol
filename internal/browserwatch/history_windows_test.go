//go:build windows

package browserwatch

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

func createChromiumHistoryFixture(t *testing.T, withVisits bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "History")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE urls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			visit_count INTEGER NOT NULL DEFAULT 1,
			typed_count INTEGER NOT NULL DEFAULT 0,
			last_visit_time INTEGER NOT NULL,
			hidden INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE keyword_search_terms (
			keyword_id INTEGER NOT NULL,
			url_id INTEGER NOT NULL,
			term TEXT NOT NULL,
			normalized_term TEXT NOT NULL DEFAULT ''
		)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if withVisits {
		if _, err := db.Exec(`CREATE TABLE visits (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url INTEGER NOT NULL,
			visit_time INTEGER NOT NULL
		)`); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Add(-time.Minute)
	chromeNow := now.UnixMicro() + chromiumEpochOffsetMicros
	rows := []struct {
		url      string
		title    string
		lastTime int64
		keyword  string
	}{
		{"https://www.poki.com/games?token=secret", "Poki games", chromeNow, ""},
		{"https://www.google.com/search?q=algebra", "algebra - Google Search", chromeNow - 1_000_000, "algebra"},
		{"chrome://settings/", "Settings", chromeNow, ""},
		{"http://127.0.0.1/private", "Local", chromeNow, ""},
	}
	for _, row := range rows {
		result, err := db.Exec(`INSERT INTO urls (url, title, last_visit_time) VALUES (?, ?, ?)`, row.url, row.title, row.lastTime)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		if withVisits {
			if _, err := db.Exec(`INSERT INTO visits (url, visit_time) VALUES (?, ?)`, id, row.lastTime); err != nil {
				t.Fatal(err)
			}
		}
		if row.keyword != "" {
			if _, err := db.Exec(`INSERT INTO keyword_search_terms (keyword_id, url_id, term, normalized_term) VALUES (1, ?, ?, ?)`, id, row.keyword, row.keyword); err != nil {
				t.Fatal(err)
			}
		}
	}
	return path
}

func TestQueryChromiumHistoryDetailed(t *testing.T) {
	path := createChromiumHistoryFixture(t, true)
	records, err := queryChromiumHistory(context.Background(), path, time.Time{}, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
	foundGoogle := false
	for _, record := range records {
		visit, ok := makeBrowserVisit(record, profileSpec{
			Browser: model.BrowserChrome, UserSID: "S-1-5-21-1", UserName: `PC\kid`,
			ProfileKey: profileKeyForPath(path), ProfileLabel: "Default", Running: true,
		}, true)
		if !ok {
			continue
		}
		if visit.Domain == "google.com" {
			foundGoogle = true
			if visit.SearchQuery != "algebra" || visit.SearchEngine != "Google" {
				t.Fatalf("search = %q/%q", visit.SearchEngine, visit.SearchQuery)
			}
			if visit.Title == "" || visit.URL == "" || visit.ProfileKey == "" || visit.SourceKey == "" {
				t.Fatalf("detailed visit missing fields: %+v", visit)
			}
		}
	}
	if !foundGoogle {
		t.Fatalf("google visit missing: %+v", records)
	}
}

func TestQueryChromiumHistoryFallsBackToURLTable(t *testing.T) {
	path := createChromiumHistoryFixture(t, false)
	records, err := queryChromiumHistory(context.Background(), path, time.Time{}, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
}

func TestQueryFirefoxHistoryDetailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "places.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE moz_places (
		id INTEGER PRIMARY KEY,
		url TEXT NOT NULL,
		title TEXT,
		last_visit_date INTEGER,
		hidden INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE moz_historyvisits (
		id INTEGER PRIMARY KEY,
		place_id INTEGER NOT NULL,
		visit_date INTEGER
	)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := db.Exec(`INSERT INTO moz_places (id, url, title, last_visit_date) VALUES (1, 'https://duckduckgo.com/?q=history', 'history at DuckDuckGo', ?)`, now.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO moz_historyvisits (id, place_id, visit_date) VALUES (7, 1, ?)`, now.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	records, err := queryFirefoxHistory(context.Background(), path, time.Time{}, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	visit, ok := makeBrowserVisit(records[0], profileSpec{
		Browser: model.BrowserFirefox, UserSID: "S-1-5-21-1", UserName: `PC\kid`,
		ProfileKey: profileKeyForPath(path), ProfileLabel: "default-release", Running: true,
	}, true)
	if !ok {
		t.Fatal("firefox visit was rejected")
	}
	if visit.Domain != "duckduckgo.com" || visit.SearchQuery != "history" || visit.SearchEngine != "DuckDuckGo" {
		t.Fatalf("visit = %+v", visit)
	}
	if visit.VisitedAt.Sub(now).Abs() > time.Second {
		t.Fatalf("visit time = %s, want %s", visit.VisitedAt, now)
	}
}

func TestNormalizeHistoryURLAndSearch(t *testing.T) {
	tests := []struct {
		raw    string
		engine string
		query  string
		ok     bool
	}{
		{"https://www.google.com/search?q=math+homework", "Google", "math homework", true},
		{"https://www.bing.com/search?q=science", "Bing", "science", true},
		{"https://duckduckgo.com/?q=history", "DuckDuckGo", "history", true},
		{"https://www.youtube.com/results?search_query=algebra", "YouTube", "algebra", true},
		{"chrome://settings/", "", "", false},
		{"http://127.0.0.1/private", "", "", false},
		{"https://example.com/path?q=not-a-search", "", "", true},
	}
	for _, test := range tests {
		_, _, engine, query, ok := normalizeHistoryURL(test.raw, "")
		if ok != test.ok || engine != test.engine || query != test.query {
			t.Fatalf("%q => ok=%t engine=%q query=%q", test.raw, ok, engine, query)
		}
	}
}

func TestChromiumAndFirefoxProfileDiscovery(t *testing.T) {
	root := t.TempDir()
	chromeBase := filepath.Join(root, "Chrome", "User Data")
	if err := os.MkdirAll(filepath.Join(chromeBase, "Default"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chromeBase, "Default", "History"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := chromiumProfileSpecs(chromeBase, model.BrowserChrome, "", "S-1-5-21-1", `PC\kid`, true)
	if len(specs) != 1 || specs[0].ProfileLabel != "Default" || specs[0].Browser != model.BrowserChrome {
		t.Fatalf("specs = %+v", specs)
	}

	firefoxRoot := filepath.Join(root, "Firefox")
	profileDir := filepath.Join(firefoxRoot, "Profiles", "abc.default-release")
	if err := os.MkdirAll(profileDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "places.sqlite"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firefoxRoot, "profiles.ini"), []byte("[Profile0]\nPath=Profiles/abc.default-release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firefox := firefoxProfileSpecs(firefoxRoot, "S-1-5-21-1", `PC\kid`, false)
	if len(firefox) != 1 || firefox[0].Browser != model.BrowserFirefox || firefox[0].ProfileLabel != "abc.default-release" {
		t.Fatalf("firefox specs = %+v", firefox)
	}
}

func TestSanitizeHistoryErrorRedactsUserSIDAndURL(t *testing.T) {
	err := errors.New(`open C:\Users\kid\AppData\Local\Google\Chrome\User Data\Default\History: access denied for S-1-5-21-111-222-333-1001 at https://example.com/private`)
	sanitized := sanitizeHistoryError(err).Error()
	if strings.Contains(sanitized, "kid") || strings.Contains(sanitized, "S-1-") || strings.Contains(sanitized, "example.com") {
		t.Fatalf("sensitive identity was not redacted: %s", sanitized)
	}
	if !strings.Contains(sanitized, "<user>") || !strings.Contains(sanitized, "<sid>") || !strings.Contains(sanitized, "<url>") {
		t.Fatalf("redaction markers missing: %s", sanitized)
	}
}
