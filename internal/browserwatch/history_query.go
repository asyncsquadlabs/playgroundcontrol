package browserwatch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/domains"
	_ "modernc.org/sqlite" // register the pure-Go SQLite driver

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	chromiumEpochOffsetMicros = int64(11_644_473_600_000_000)
	gameHistoryQueryLimit     = 5_000
	journalHistoryQueryLimit  = 20_000
)

type profileSpec struct {
	Path         string
	Browser      string
	UserSID      string
	UserName     string
	ProfileKey   string
	ProfileLabel string
	Running      bool
}

type historyRecord struct {
	SourceID    int64
	VisitedAt   time.Time
	URL         string
	Title       string
	KeywordTerm string
}

func profileKeyForPath(path string) string {
	clean := strings.ToLower(filepath.Clean(path))
	sum := sha256.Sum256([]byte(clean))
	return hex.EncodeToString(sum[:])
}

func visitSourceKey(browser, profileKey string, sourceID int64, visitedAt time.Time, rawURL string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s|%s", browser, profileKey, sourceID,
		visitedAt.UTC().Format(time.RFC3339Nano), rawURL)))
	return hex.EncodeToString(sum[:])
}

func makeBrowserVisit(record historyRecord, spec profileSpec, detailed bool) (model.BrowserVisit, bool) {
	cleanURL, domain, engine, query, ok := normalizeHistoryURL(record.URL, record.KeywordTerm)
	if !ok {
		return model.BrowserVisit{}, false
	}
	visit := model.BrowserVisit{
		Browser:      spec.Browser,
		Domain:       domain,
		VisitedAt:    record.VisitedAt.UTC(),
		UserSID:      spec.UserSID,
		UserName:     spec.UserName,
		ProfileKey:   spec.ProfileKey,
		ProfileLabel: spec.ProfileLabel,
		SourceKey:    visitSourceKey(spec.Browser, spec.ProfileKey, record.SourceID, record.VisitedAt, record.URL),
		Running:      spec.Running,
	}
	if detailed {
		visit.URL = cleanURL
		visit.Title = sanitizeText(record.Title, 512)
		visit.SearchEngine = engine
		visit.SearchQuery = query
	}
	return visit, true
}

func normalizeHistoryURL(raw, keywordTerm string) (cleanURL, domain, engine, query string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", "", "", false
	}
	if parsed.Host == "" {
		return "", "", "", "", false
	}
	parsed.User = nil
	parsed.Fragment = ""
	cleanURL = parsed.String()
	domain, err = domains.Normalize(cleanURL)
	if err != nil {
		return "", "", "", "", false
	}
	engine, query = extractSearch(parsed, keywordTerm)
	return cleanURL, domain, engine, query, true
}

func extractSearch(parsed *url.URL, keywordTerm string) (string, string) {
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	path := strings.ToLower(parsed.Path)
	engine, parameter := searchEngineHost(host, path)
	if engine == "" {
		return "", ""
	}
	if term := sanitizeText(keywordTerm, 512); term != "" {
		return engine, term
	}
	if parameter == "" {
		return engine, ""
	}
	return engine, sanitizeText(parsed.Query().Get(parameter), 512)
}

func searchEngineHost(host, path string) (string, string) {
	switch {
	case path == "/search" && (host == "google.com" || strings.HasPrefix(host, "google.") || strings.Contains(host, ".google.")):
		return "Google", "q"
	case path == "/search" && (host == "bing.com" || strings.HasSuffix(host, ".bing.com")):
		return "Bing", "q"
	case (host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")) && (path == "/" || path == "/html" || path == "/html/"):
		return "DuckDuckGo", "q"
	case (host == "search.yahoo.com" || strings.HasSuffix(host, ".search.yahoo.com") || strings.HasPrefix(host, "search.yahoo.")) && path == "/search":
		return "Yahoo", "p"
	case strings.Contains(host, "yandex.") && (path == "/search" || path == "/search/"):
		return "Yandex", "text"
	case (host == "ecosia.org" || strings.HasSuffix(host, ".ecosia.org")) && path == "/search":
		return "Ecosia", "q"
	case host == "search.brave.com" && path == "/search":
		return "Brave Search", "q"
	case (host == "startpage.com" || strings.HasSuffix(host, ".startpage.com")) && strings.Contains(path, "search"):
		return "Startpage", "query"
	case (host == "qwant.com" || strings.HasSuffix(host, ".qwant.com")) && path == "/search":
		return "Qwant", "q"
	case strings.Contains(host, "searx") && path == "/search":
		return "SearX", "q"
	case (host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")) && path == "/results":
		return "YouTube", "search_query"
	default:
		return "", ""
	}
}

func sanitizeText(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= max {
		return value
	}
	for len(value) > max {
		_, size := lastRune(value)
		if size <= 0 {
			break
		}
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}

func lastRune(value string) (rune, int) {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i]&0xc0 != 0x80 {
			r := []rune(value[i:])
			if len(r) == 0 {
				return 0, 0
			}
			return r[0], len(value) - i
		}
	}
	return 0, 0
}

func queryChromiumHistory(ctx context.Context, path string, since time.Time, detailed bool, limit int) ([]historyRecord, error) {
	if limit <= 0 {
		limit = gameHistoryQueryLimit
	}
	records, err := queryChromiumHistoryMode(ctx, path, since, detailed, true, limit)
	if err == nil {
		return records, nil
	}
	// Older or unusual profiles may lack the keyword_search_terms table.
	return queryChromiumHistoryMode(ctx, path, since, detailed, false, limit)
}

func queryChromiumHistoryMode(ctx context.Context, path string, since time.Time, detailed, withKeywords bool, limit int) ([]historyRecord, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open browser history snapshot: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cutoff := since.UTC().UnixMicro() + chromiumEpochOffsetMicros
	if since.IsZero() {
		cutoff = 0
	}
	keywordSelect := "''"
	if detailed && withKeywords {
		keywordSelect = `COALESCE((SELECT k.term FROM keyword_search_terms k WHERE k.url_id = u.id LIMIT 1), '')`
	}
	titleSelect := "''"
	if detailed {
		titleSelect = "COALESCE(u.title, '')"
	}
	query := fmt.Sprintf(`SELECT v.id, v.visit_time, u.url, %s, %s
		FROM visits v JOIN urls u ON v.url = u.id
		WHERE v.visit_time >= ? ORDER BY v.visit_time DESC LIMIT ?`, titleSelect, keywordSelect)
	rows, err := db.QueryContext(ctx, query, cutoff, limit)
	if err != nil {
		if rows != nil {
			_ = rows.Close()
		}
		query = fmt.Sprintf(`SELECT 0, u.last_visit_time, u.url, %s, %s
			FROM urls u WHERE u.last_visit_time >= ?
			ORDER BY u.last_visit_time DESC LIMIT ?`, titleSelect, keywordSelect)
		rows, err = db.QueryContext(ctx, query, cutoff, limit)
		if err != nil {
			return nil, fmt.Errorf("query browser history: %w", err)
		}
	}
	defer rows.Close()
	out := make([]historyRecord, 0)
	for rows.Next() {
		var record historyRecord
		var chrometime int64
		if err := rows.Scan(&record.SourceID, &chrometime, &record.URL, &record.Title, &record.KeywordTerm); err != nil {
			return nil, fmt.Errorf("read browser history row: %w", err)
		}
		record.VisitedAt = time.UnixMicro(chrometime - chromiumEpochOffsetMicros).UTC()
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read browser history: %w", err)
	}
	return out, nil
}

//lint:ignore U1000 used by the Windows history source
func queryFirefoxHistory(ctx context.Context, path string, since time.Time, detailed bool, limit int) ([]historyRecord, error) {
	if limit <= 0 {
		limit = gameHistoryQueryLimit
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open Firefox history snapshot: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cutoff := since.UTC().UnixMicro()
	if since.IsZero() {
		cutoff = 0
	}
	titleSelect := "''"
	if detailed {
		titleSelect = "COALESCE(p.title, '')"
	}
	query := fmt.Sprintf(`SELECT h.id, COALESCE(h.visit_date, p.last_visit_date, 0), p.url, %s
		FROM moz_historyvisits h JOIN moz_places p ON h.place_id = p.id
		WHERE COALESCE(h.visit_date, p.last_visit_date, 0) >= ?
		ORDER BY COALESCE(h.visit_date, p.last_visit_date, 0) DESC LIMIT ?`, titleSelect)
	rows, err := db.QueryContext(ctx, query, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query Firefox history: %w", err)
	}
	defer rows.Close()
	out := make([]historyRecord, 0)
	for rows.Next() {
		var record historyRecord
		var firefoxTime int64
		if err := rows.Scan(&record.SourceID, &firefoxTime, &record.URL, &record.Title); err != nil {
			return nil, fmt.Errorf("read Firefox history row: %w", err)
		}
		record.VisitedAt = time.UnixMicro(firefoxTime).UTC()
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Firefox history: %w", err)
	}
	return out, nil
}

//lint:ignore U1000 used by the Windows history source
func queryHistoryCount(ctx context.Context, path, browser string) (int64, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, fmt.Errorf("open history count snapshot: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var queries []string
	switch browser {
	case model.BrowserFirefox:
		queries = []string{`SELECT COUNT(*) FROM moz_historyvisits`, `SELECT COUNT(*) FROM moz_places`}
	default:
		queries = []string{`SELECT COUNT(*) FROM visits`, `SELECT COUNT(*) FROM urls`}
	}
	var lastErr error
	for _, query := range queries {
		var count int64
		if err := db.QueryRowContext(ctx, query).Scan(&count); err == nil {
			return count, nil
		} else {
			lastErr = err
		}
	}
	return 0, fmt.Errorf("count browser history: %w", lastErr)
}
