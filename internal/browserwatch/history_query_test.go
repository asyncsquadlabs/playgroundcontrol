package browserwatch

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

func createQueryChromiumFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "History")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE urls (id INTEGER PRIMARY KEY AUTOINCREMENT, url TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', last_visit_time INTEGER NOT NULL)`,
		`CREATE TABLE visits (id INTEGER PRIMARY KEY AUTOINCREMENT, url INTEGER NOT NULL, visit_time INTEGER NOT NULL)`,
		`CREATE TABLE keyword_search_terms (keyword_id INTEGER NOT NULL, url_id INTEGER NOT NULL, term TEXT NOT NULL, normalized_term TEXT NOT NULL DEFAULT '')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Add(-time.Minute)
	chromeNow := now.UnixMicro() + chromiumEpochOffsetMicros
	result, err := db.Exec(`INSERT INTO urls (url, title, last_visit_time) VALUES ('https://www.google.com/search?q=algebra', 'algebra - Google Search', ?)`, chromeNow)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if _, err := db.Exec(`INSERT INTO visits (url, visit_time) VALUES (?, ?)`, id, chromeNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO keyword_search_terms (keyword_id, url_id, term, normalized_term) VALUES (1, ?, 'algebra', 'algebra')`, id); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestQueryChromiumHistoryWithKeywordTerm(t *testing.T) {
	path := createQueryChromiumFixture(t)
	records, err := queryChromiumHistory(context.Background(), path, time.Time{}, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v", records)
	}
	visit, ok := makeBrowserVisit(records[0], profileSpec{
		Browser: model.BrowserChrome, UserSID: "S-1-5-21-1", UserName: `PC\kid`,
		ProfileKey: profileKeyForPath(path), ProfileLabel: "Default", Running: true,
	}, true)
	if !ok {
		t.Fatal("visit was rejected")
	}
	if visit.SearchEngine != "Google" || visit.SearchQuery != "algebra" {
		t.Fatalf("search = %q/%q", visit.SearchEngine, visit.SearchQuery)
	}
	if visit.Title != "algebra - Google Search" || visit.Domain != "google.com" {
		t.Fatalf("visit = %+v", visit)
	}
}

func TestNormalizeHistoryURLSearchEngines(t *testing.T) {
	tests := []struct {
		raw, engine, query string
		ok                 bool
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

func TestProfileKeyIsStableAndDoesNotExposePath(t *testing.T) {
	path := `C:\Users\kid\AppData\Local\Google\Chrome\User Data\Default\History`
	key := profileKeyForPath(path)
	if len(key) != 64 {
		t.Fatalf("key = %q", key)
	}
	if key != profileKeyForPath(`c:\users\kid\appdata\local\google\chrome\user data\default\history`) {
		t.Fatal("profile key changed with case")
	}
	if key == path {
		t.Fatal("profile key exposed the path")
	}
}

func TestSanitizeTextBoundsAndStripsControls(t *testing.T) {
	got := sanitizeText("  hello\nworld\x00  ", 64)
	if got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if len(sanitizeText("abcdef", 3)) > 3 {
		t.Fatal("truncation exceeded bound")
	}
}

func TestQueryFirefoxHistoryUnixEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "places.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE moz_places (id INTEGER PRIMARY KEY, url TEXT NOT NULL, title TEXT, last_visit_date INTEGER, hidden INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE moz_historyvisits (id INTEGER PRIMARY KEY, place_id INTEGER NOT NULL, visit_date INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := db.Exec(`INSERT INTO moz_places (id, url, title, last_visit_date) VALUES (1, 'https://duckduckgo.com/?q=history', 'history at DuckDuckGo', ?)`, now.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO moz_historyvisits (id, place_id, visit_date) VALUES (7, 1, ?)`, now.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	records, err := queryFirefoxHistory(context.Background(), path, time.Time{}, true, 100)
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	visit, ok := makeBrowserVisit(records[0], profileSpec{
		Browser: model.BrowserFirefox, UserSID: "S-1-5-21-1", UserName: `PC\kid`,
		ProfileKey: profileKeyForPath(path), ProfileLabel: "default-release", Running: true,
	}, true)
	if !ok || visit.Domain != "duckduckgo.com" || visit.SearchQuery != "history" {
		t.Fatalf("visit=%+v ok=%t", visit, ok)
	}
	if visit.VisitedAt.Sub(now).Abs() > time.Second {
		t.Fatalf("visit time=%s want=%s", visit.VisitedAt, now)
	}
}
