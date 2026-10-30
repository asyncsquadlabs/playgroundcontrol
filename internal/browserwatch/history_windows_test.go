//go:build windows

package browserwatch

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func createHistoryFixture(t *testing.T, withVisits bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "History")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE urls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		visit_count INTEGER NOT NULL DEFAULT 1,
		typed_count INTEGER NOT NULL DEFAULT 0,
		last_visit_time INTEGER NOT NULL,
		hidden INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		t.Fatal(err)
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
	chromeNow := now.UnixMicro() + chromeEpochOffsetMicros
	rows := []struct {
		url      string
		lastTime int64
		visit    int64
	}{
		{"https://www.poki.com/games?token=secret", chromeNow, chromeNow},
		{"https://google.com/search?q=secret", chromeNow - 1_000_000, chromeNow - 1_000_000},
		{"chrome://settings/", chromeNow, chromeNow},
		{"http://127.0.0.1/private", chromeNow, chromeNow},
	}
	for _, row := range rows {
		result, err := db.Exec(`INSERT INTO urls (url, last_visit_time) VALUES (?, ?)`, row.url, row.lastTime)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		if withVisits {
			if _, err := db.Exec(`INSERT INTO visits (url, visit_time) VALUES (?, ?)`, id, row.visit); err != nil {
				t.Fatal(err)
			}
		}
	}
	return path
}

func TestQueryHistoryNormalizesAndSkipsInternalURLs(t *testing.T) {
	path := createHistoryFixture(t, true)
	visits, err := queryHistory(context.Background(), path, "chrome", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, visit := range visits {
		seen[visit.Domain] = true
	}
	if !seen["poki.com"] || !seen["google.com"] {
		t.Fatalf("expected domains missing: %+v", visits)
	}
	for domain := range seen {
		if domain == "127.0.0.1" || domain == "" {
			t.Fatalf("internal host was returned: %s", domain)
		}
	}
	if len(visits) != 2 {
		t.Fatalf("visits = %+v", visits)
	}
}

func TestQueryHistoryFallsBackToURLTable(t *testing.T) {
	path := createHistoryFixture(t, false)
	visits, err := queryHistory(context.Background(), path, "edge", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(visits) != 2 {
		t.Fatalf("visits = %+v", visits)
	}
}

func TestDedupeVisitsKeepsLatestPerBrowserDomain(t *testing.T) {
	now := time.Now().UTC()
	visits := dedupeVisits([]Visit{
		{Browser: "chrome", Domain: "poki.com", VisitedAt: now.Add(-time.Minute)},
		{Browser: "chrome", Domain: "poki.com", VisitedAt: now},
		{Browser: "edge", Domain: "poki.com", VisitedAt: now},
	})
	if len(visits) != 2 {
		t.Fatalf("visits = %+v", visits)
	}
}

func TestSanitizeHistoryErrorRedactsUserAndSID(t *testing.T) {
	err := errors.New(`open C:\Users\kid\AppData\Local\Google\Chrome\User Data\Default\History: access denied for S-1-5-21-111-222-333-1001`)
	sanitized := sanitizeHistoryError(err).Error()
	if strings.Contains(sanitized, "kid") || strings.Contains(sanitized, "S-1-") {
		t.Fatalf("sensitive identity was not redacted: %s", sanitized)
	}
	if !strings.Contains(sanitized, "<user>") || !strings.Contains(sanitized, "<sid>") {
		t.Fatalf("redaction markers missing: %s", sanitized)
	}
}
