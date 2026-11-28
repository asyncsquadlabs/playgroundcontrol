package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/domains"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	// BrowserActivityRetention is how long detailed journal rows are kept.
	BrowserActivityRetention = 90 * 24 * time.Hour
	// MaxBrowserActivityRows bounds the journal even when 90 days is larger.
	MaxBrowserActivityRows = 500_000

	browserActivityInsertBatch = 250
	browserActivityPruneBatch  = 10_000
	activityBackupInterval     = 24 * time.Hour
	defaultActivityLimit       = 100
	maxActivityLimit           = 200
)

// ErrBrowserHistoryAlertNotFound reports a missing history-clear alert.
var ErrBrowserHistoryAlertNotFound = errors.New("browser history alert not found")

// SaveBrowserJournalUsers replaces the selected journal accounts and clears the
// backfill marker so newly selected accounts import their available history.
func (s *Store) SaveBrowserJournalUsers(users []model.BrowserUser, now time.Time) error {
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	s.activityMu.Lock()
	if s.db == nil {
		s.activityMu.Unlock()
		return errors.New("database is not open")
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.activityMu.Unlock()
		return fmt.Errorf("begin journal user update: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(`DELETE FROM browser_journal_users`); err != nil {
		s.activityMu.Unlock()
		return fmt.Errorf("clear journal users: %w", err)
	}
	for _, user := range users {
		if !user.Selected {
			continue
		}
		sid := strings.ToUpper(strings.TrimSpace(user.SID))
		if !strings.HasPrefix(sid, "S-1-") {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO browser_journal_users (sid, user_name, selected, updated_at) VALUES (?, ?, 1, ?)`,
			sid, truncateText(strings.TrimSpace(user.UserName), 128), formatTime(now),
		); err != nil {
			s.activityMu.Unlock()
			return fmt.Errorf("save journal user: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE browser_settings SET activity_backfill_at = NULL WHERE id = 1`); err != nil {
		s.activityMu.Unlock()
		return fmt.Errorf("reset activity backfill: %w", err)
	}
	if err := tx.Commit(); err != nil {
		s.activityMu.Unlock()
		return fmt.Errorf("commit journal user update: %w", err)
	}
	committed = true
	s.activityMu.Unlock()

	s.mu.Lock()
	s.browser.ActivityBackfillAt = time.Time{}
	s.mu.Unlock()
	return nil
}

// BrowserJournalUsers returns the selected journal accounts.
func (s *Store) BrowserJournalUsers() ([]model.BrowserUser, error) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	rows, err := s.db.Query(`SELECT sid, user_name FROM browser_journal_users WHERE selected = 1 ORDER BY user_name, sid`)
	if err != nil {
		return nil, fmt.Errorf("read journal users: %w", err)
	}
	defer rows.Close()
	out := make([]model.BrowserUser, 0)
	for rows.Next() {
		var user model.BrowserUser
		if err := rows.Scan(&user.SID, &user.UserName); err != nil {
			return nil, fmt.Errorf("read journal user: %w", err)
		}
		user.Selected = true
		user.Available = true
		if user.Browsers == nil {
			user.Browsers = []string{}
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read journal users: %w", err)
	}
	return out, nil
}

// SelectedBrowserUserSIDs returns the selected account SIDs as a set.
func (s *Store) SelectedBrowserUserSIDs() (map[string]struct{}, error) {
	users, err := s.BrowserJournalUsers()
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(users))
	for _, user := range users {
		out[strings.ToUpper(strings.TrimSpace(user.SID))] = struct{}{}
	}
	return out, nil
}

// BrowserJournalUserCount returns how many accounts are selected.
func (s *Store) BrowserJournalUserCount() (int, error) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return 0, errors.New("database is not open")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_journal_users WHERE selected = 1`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count journal users: %w", err)
	}
	return count, nil
}

// SetBrowserActivityBackfillDone records that the selected accounts have been
// backfilled at least once.
func (s *Store) SetBrowserActivityBackfillDone(at time.Time) error {
	if at.IsZero() {
		at = s.now().UTC()
	} else {
		at = at.UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("database is not open")
	}
	if _, err := s.db.Exec(`UPDATE browser_settings SET activity_backfill_at = ? WHERE id = 1`, formatActivityTime(at)); err != nil {
		return fmt.Errorf("save activity backfill marker: %w", err)
	}
	s.browser.ActivityBackfillAt = at
	return nil
}

// RecordBrowserActivity upserts detailed visits in bounded transactions and
// returns the number of rows written.
func (s *Store) RecordBrowserActivity(scanTime time.Time, visits []model.BrowserVisit) (int, error) {
	if scanTime.IsZero() {
		scanTime = s.now().UTC()
	} else {
		scanTime = scanTime.UTC()
	}
	if len(visits) == 0 {
		return 0, nil
	}
	recordedAt := s.now().UTC()
	s.activityMu.Lock()
	if s.db == nil {
		s.activityMu.Unlock()
		return 0, errors.New("database is not open")
	}
	written := 0
	for start := 0; start < len(visits); start += browserActivityInsertBatch {
		end := start + browserActivityInsertBatch
		if end > len(visits) {
			end = len(visits)
		}
		tx, err := s.db.Begin()
		if err != nil {
			s.activityMu.Unlock()
			return written, fmt.Errorf("begin activity batch: %w", err)
		}
		stmt, err := tx.Prepare(`INSERT INTO browser_activity
			(time, recorded_at, user_sid, user_name, browser, profile_key, profile_label, source_key,
			 url, title, domain, search_engine, search_query, last_verified_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(profile_key, source_key) DO UPDATE SET
			 time = excluded.time,
			 user_sid = excluded.user_sid,
			 user_name = excluded.user_name,
			 browser = excluded.browser,
			 profile_label = excluded.profile_label,
			 url = excluded.url,
			 title = CASE WHEN excluded.title <> '' THEN excluded.title ELSE browser_activity.title END,
			 domain = excluded.domain,
			 search_engine = CASE WHEN excluded.search_engine <> '' THEN excluded.search_engine ELSE browser_activity.search_engine END,
			 search_query = CASE WHEN excluded.search_query <> '' THEN excluded.search_query ELSE browser_activity.search_query END,
			 last_verified_at = excluded.last_verified_at`)
		if err != nil {
			_ = tx.Rollback()
			s.activityMu.Unlock()
			return written, fmt.Errorf("prepare activity insert: %w", err)
		}
		for _, visit := range visits[start:end] {
			browser, ok := model.NormalizeBrowser(visit.Browser)
			if !ok {
				continue
			}
			domain, err := domains.Normalize(visit.Domain)
			if err != nil {
				domain, err = domains.Normalize(visit.URL)
				if err != nil {
					continue
				}
			}
			profileKey := strings.TrimSpace(visit.ProfileKey)
			if profileKey == "" {
				continue
			}
			sourceKey := strings.TrimSpace(visit.SourceKey)
			if sourceKey == "" {
				sourceKey = activitySourceKey(browser, profileKey, visit.VisitedAt, visit.URL)
			}
			userName := truncateText(strings.TrimSpace(visit.UserName), 128)
			if userName == "" {
				userName = truncateText(strings.TrimSpace(visit.UserSID), 128)
			}
			if _, err := stmt.Exec(
				formatActivityTime(visit.VisitedAt), formatActivityTime(recordedAt),
				truncateText(strings.TrimSpace(visit.UserSID), 128), userName,
				browser, profileKey, truncateText(strings.TrimSpace(visit.ProfileLabel), 64), sourceKey,
				truncateText(visit.URL, 2048), truncateText(visit.Title, 512), domain,
				truncateText(visit.SearchEngine, 64), truncateText(visit.SearchQuery, 512),
				formatActivityTime(scanTime),
			); err != nil {
				_ = stmt.Close()
				_ = tx.Rollback()
				s.activityMu.Unlock()
				return written, fmt.Errorf("save activity visit: %w", err)
			}
			written++
		}
		_ = stmt.Close()
		if err := tx.Commit(); err != nil {
			s.activityMu.Unlock()
			return written, fmt.Errorf("commit activity batch: %w", err)
		}
	}
	if s.activityBackupAt.IsZero() {
		s.activityBackupAt = recordedAt
	}
	shouldBackup := recordedAt.Sub(s.activityBackupAt) >= activityBackupInterval
	if shouldBackup {
		s.activityBackupAt = recordedAt
	}
	s.activityMu.Unlock()
	if shouldBackup {
		if err := s.refreshBackup(); err != nil {
			s.appendNotice(fmt.Sprintf("could not refresh database backup after activity journal write: %v", err))
		}
	}
	return written, nil
}

// MarkMissingBrowserActivity sets removed_at on recent journal rows that were
// not present in the latest successful snapshot for a profile.
func (s *Store) MarkMissingBrowserActivity(profileKey string, scanTime, cutoff time.Time) (int, error) {
	profileKey = strings.TrimSpace(profileKey)
	if profileKey == "" {
		return 0, nil
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return 0, errors.New("database is not open")
	}
	result, err := s.db.Exec(
		`UPDATE browser_activity SET removed_at = ?
		 WHERE profile_key = ? AND time >= ? AND removed_at IS NULL AND last_verified_at < ?`,
		formatActivityTime(scanTime), profileKey, formatActivityTime(cutoff), formatActivityTime(scanTime),
	)
	if err != nil {
		return 0, fmt.Errorf("mark missing browser activity: %w", err)
	}
	affected, _ := result.RowsAffected()
	return int(affected), nil
}

// BrowserHistoryStat returns the stored count baseline for a profile.
func (s *Store) BrowserHistoryStat(profileKey string) (model.BrowserHistoryStat, bool, error) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return model.BrowserHistoryStat{}, false, errors.New("database is not open")
	}
	var stat model.BrowserHistoryStat
	var readable, checked sql.NullString
	err := s.db.QueryRow(
		`SELECT profile_key, browser, user_sid, user_name, profile_label, visit_count, last_readable_at, last_checked_at
		 FROM browser_history_stats WHERE profile_key = ?`, profileKey,
	).Scan(&stat.ProfileKey, &stat.Browser, &stat.UserSID, &stat.UserName, &stat.ProfileLabel,
		&stat.VisitCount, &readable, &checked)
	if errors.Is(err, sql.ErrNoRows) {
		return model.BrowserHistoryStat{}, false, nil
	}
	if err != nil {
		return model.BrowserHistoryStat{}, false, fmt.Errorf("read browser history stat: %w", err)
	}
	stat.LastReadableAt = parseTime(readable.String)
	stat.LastCheckedAt = parseTime(checked.String)
	return stat, true, nil
}

// SaveBrowserHistoryStat upserts a profile's browser history count baseline.
func (s *Store) SaveBrowserHistoryStat(stat model.BrowserHistoryStat) error {
	if strings.TrimSpace(stat.ProfileKey) == "" {
		return errors.New("profile key is required")
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return errors.New("database is not open")
	}
	if _, err := s.db.Exec(
		`INSERT INTO browser_history_stats
		 (profile_key, browser, user_sid, user_name, profile_label, visit_count, last_readable_at, last_checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(profile_key) DO UPDATE SET
		   browser = excluded.browser,
		   user_sid = excluded.user_sid,
		   user_name = excluded.user_name,
		   profile_label = excluded.profile_label,
		   visit_count = excluded.visit_count,
		   last_readable_at = excluded.last_readable_at,
		   last_checked_at = excluded.last_checked_at`,
		stat.ProfileKey, truncateText(stat.Browser, 32), truncateText(stat.UserSID, 128),
		truncateText(stat.UserName, 128), truncateText(stat.ProfileLabel, 64), stat.VisitCount,
		formatActivityTime(stat.LastReadableAt), formatActivityTime(stat.LastCheckedAt),
	); err != nil {
		return fmt.Errorf("save browser history stat: %w", err)
	}
	return nil
}

// RecordBrowserHistoryAlert inserts one history-clear alert.
func (s *Store) RecordBrowserHistoryAlert(alert model.BrowserHistoryAlert) (model.BrowserHistoryAlert, error) {
	if alert.Time.IsZero() {
		alert.Time = s.now().UTC()
	} else {
		alert.Time = alert.Time.UTC()
	}
	browser, ok := model.NormalizeBrowser(alert.Browser)
	if !ok {
		return model.BrowserHistoryAlert{}, errors.New("unsupported browser")
	}
	alert.Browser = browser
	alert.Reason = truncateText(strings.TrimSpace(alert.Reason), 64)
	if alert.Reason == "" {
		alert.Reason = model.BrowserHistoryAlertBulkRemoval
	}
	if alert.ID == "" {
		id, err := model.NewID()
		if err != nil {
			return model.BrowserHistoryAlert{}, err
		}
		alert.ID = id
	}
	alert.UserName = truncateText(strings.TrimSpace(alert.UserName), 128)
	alert.UserSID = truncateText(strings.TrimSpace(alert.UserSID), 128)
	alert.ProfileLabel = truncateText(strings.TrimSpace(alert.ProfileLabel), 64)
	if alert.RemovedCount < 0 {
		alert.RemovedCount = 0
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return model.BrowserHistoryAlert{}, errors.New("database is not open")
	}
	if _, err := s.db.Exec(
		`INSERT INTO browser_history_alerts
		 (id, time, user_sid, user_name, browser, profile_label, removed_count, reason, acknowledged_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		alert.ID, formatActivityTime(alert.Time), alert.UserSID, alert.UserName, alert.Browser,
		alert.ProfileLabel, alert.RemovedCount, alert.Reason,
	); err != nil {
		return model.BrowserHistoryAlert{}, fmt.Errorf("save browser history alert: %w", err)
	}
	return alert, nil
}

// BrowserHistoryAlerts returns the newest history-clear alerts.
func (s *Store) BrowserHistoryAlerts(limit int) ([]model.BrowserHistoryAlert, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxActivityLimit {
		limit = maxActivityLimit
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	rows, err := s.db.Query(
		`SELECT id, time, user_sid, user_name, browser, profile_label, removed_count, reason, acknowledged_at
		 FROM browser_history_alerts ORDER BY time DESC, id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read browser history alerts: %w", err)
	}
	defer rows.Close()
	out := make([]model.BrowserHistoryAlert, 0)
	for rows.Next() {
		var alert model.BrowserHistoryAlert
		var when, acknowledged sql.NullString
		if err := rows.Scan(&alert.ID, &when, &alert.UserSID, &alert.UserName, &alert.Browser,
			&alert.ProfileLabel, &alert.RemovedCount, &alert.Reason, &acknowledged); err != nil {
			return nil, fmt.Errorf("read browser history alert: %w", err)
		}
		alert.Time = parseTime(when.String)
		if parsed := parseTime(acknowledged.String); !parsed.IsZero() {
			alert.AcknowledgedAt = &parsed
		}
		out = append(out, alert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read browser history alerts: %w", err)
	}
	return out, nil
}

// UnacknowledgedBrowserHistoryAlertsCount counts alerts still visible in the
// dashboard warning stack.
func (s *Store) UnacknowledgedBrowserHistoryAlertsCount() (int, error) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return 0, errors.New("database is not open")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_history_alerts WHERE acknowledged_at IS NULL`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count browser history alerts: %w", err)
	}
	return count, nil
}

// AcknowledgeBrowserHistoryAlert marks one alert acknowledged.
func (s *Store) AcknowledgeBrowserHistoryAlert(id string, now time.Time) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, ErrBrowserHistoryAlertNotFound
	}
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return false, errors.New("database is not open")
	}
	result, err := s.db.Exec(
		`UPDATE browser_history_alerts SET acknowledged_at = ? WHERE id = ? AND acknowledged_at IS NULL`,
		formatActivityTime(now), id,
	)
	if err != nil {
		return false, fmt.Errorf("acknowledge browser history alert: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected > 0, nil
}

// BrowserActivity returns matching journal rows and whether another page
// exists.
func (s *Store) BrowserActivity(filter model.BrowserActivityFilter) ([]model.BrowserActivity, bool, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultActivityLimit
	}
	if limit > maxActivityLimit {
		limit = maxActivityLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	to := filter.To
	if to.IsZero() {
		to = s.now().UTC()
	} else {
		to = to.UTC()
	}
	from := filter.From
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	} else {
		from = from.UTC()
	}
	userName := truncateText(strings.TrimSpace(filter.UserName), 128)
	browser := ""
	if filter.Browser != "" {
		normalized, ok := model.NormalizeBrowser(filter.Browser)
		if !ok {
			return nil, false, errors.New("unsupported browser")
		}
		browser = normalized
	}
	queryText := truncateText(strings.TrimSpace(filter.Query), 200)

	var builder strings.Builder
	builder.WriteString(`SELECT seq, time, user_sid, user_name, browser, profile_label, url, title,
		domain, search_engine, search_query, removed_at
		FROM browser_activity WHERE time >= ? AND time < ?`)
	args := []any{formatActivityTime(from), formatActivityTime(to)}
	if userName != "" {
		builder.WriteString(` AND user_name = ?`)
		args = append(args, userName)
	}
	if browser != "" {
		builder.WriteString(` AND browser = ?`)
		args = append(args, browser)
	}
	if filter.RemovedOnly {
		builder.WriteString(` AND removed_at IS NOT NULL`)
	}
	if queryText != "" {
		builder.WriteString(` AND instr(lower(url || ' ' || title || ' ' || search_query || ' ' || domain), lower(?)) > 0`)
		args = append(args, queryText)
	}
	builder.WriteString(` ORDER BY time DESC, seq DESC LIMIT ? OFFSET ?`)
	args = append(args, limit+1, offset)

	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return nil, false, errors.New("database is not open")
	}
	rows, err := s.db.Query(builder.String(), args...)
	if err != nil {
		return nil, false, fmt.Errorf("read browser activity: %w", err)
	}
	defer rows.Close()
	out := make([]model.BrowserActivity, 0)
	for rows.Next() {
		var item model.BrowserActivity
		var seq int64
		var when string
		var removed sql.NullString
		if err := rows.Scan(&seq, &when, &item.UserSID, &item.UserName, &item.Browser, &item.ProfileLabel,
			&item.URL, &item.Title, &item.Domain, &item.SearchEngine, &item.SearchQuery, &removed); err != nil {
			return nil, false, fmt.Errorf("read browser activity row: %w", err)
		}
		item.ID = strconv.FormatInt(seq, 10)
		item.Time = parseTime(when)
		if parsed := parseTime(removed.String); !parsed.IsZero() {
			item.RemovedAt = &parsed
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read browser activity: %w", err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// BrowserActivityFilterValues returns the user names and browser IDs present in
// the retained journal.
func (s *Store) BrowserActivityFilterValues() ([]string, []string, error) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return nil, nil, errors.New("database is not open")
	}
	users := make([]string, 0)
	rows, err := s.db.Query(`SELECT DISTINCT user_name FROM browser_activity WHERE user_name <> '' ORDER BY user_name LIMIT 200`)
	if err != nil {
		return nil, nil, fmt.Errorf("read activity users: %w", err)
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("read activity user: %w", err)
		}
		users = append(users, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("read activity users: %w", err)
	}
	_ = rows.Close()

	browsers := make([]string, 0)
	rows, err = s.db.Query(`SELECT DISTINCT browser FROM browser_activity ORDER BY browser LIMIT 32`)
	if err != nil {
		return nil, nil, fmt.Errorf("read activity browsers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, nil, fmt.Errorf("read activity browser: %w", err)
		}
		browsers = append(browsers, value)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read activity browsers: %w", err)
	}
	return users, browsers, nil
}

// CountBrowserActivityForProfile counts journal rows for one profile since a
// cutoff. It is used to judge whether a removal batch is a large fraction of
// the profile's recent activity.
func (s *Store) CountBrowserActivityForProfile(profileKey string, since time.Time) (int, error) {
	profileKey = strings.TrimSpace(profileKey)
	if profileKey == "" {
		return 0, nil
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return 0, errors.New("database is not open")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_activity WHERE profile_key = ? AND time >= ?`,
		profileKey, formatActivityTime(since)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count profile browser activity: %w", err)
	}
	return count, nil
}

// BrowserActivityCounts returns the last-24-hour journal count and the number
// of unacknowledged history alerts.
func (s *Store) BrowserActivityCounts(now time.Time) (int, int, error) {
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return 0, 0, errors.New("database is not open")
	}
	var events, alerts int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_activity WHERE time >= ?`, formatActivityTime(now.Add(-24*time.Hour))).Scan(&events); err != nil {
		return 0, 0, fmt.Errorf("count browser activity: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_history_alerts WHERE acknowledged_at IS NULL`).Scan(&alerts); err != nil {
		return 0, 0, fmt.Errorf("count browser history alerts: %w", err)
	}
	return events, alerts, nil
}

// PruneBrowserActivity removes rows beyond the retention window and row cap.
func (s *Store) PruneBrowserActivity(now time.Time) (int64, error) {
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	cutoff := now.Add(-BrowserActivityRetention)
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	if s.db == nil {
		return 0, errors.New("database is not open")
	}
	var total int64
	for {
		result, err := s.db.Exec(
			`DELETE FROM browser_activity WHERE seq IN (
				SELECT seq FROM browser_activity WHERE time < ? ORDER BY time ASC LIMIT ?)`,
			formatActivityTime(cutoff), browserActivityPruneBatch,
		)
		if err != nil {
			return total, fmt.Errorf("prune browser activity: %w", err)
		}
		affected, _ := result.RowsAffected()
		total += affected
		if affected < browserActivityPruneBatch {
			break
		}
	}
	var count int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_activity`).Scan(&count); err != nil {
		return total, fmt.Errorf("count browser activity after prune: %w", err)
	}
	for count > MaxBrowserActivityRows {
		remove := count - MaxBrowserActivityRows
		if remove > browserActivityPruneBatch {
			remove = browserActivityPruneBatch
		}
		result, err := s.db.Exec(
			`DELETE FROM browser_activity WHERE seq IN (
				SELECT seq FROM browser_activity ORDER BY time ASC, seq ASC LIMIT ?)`, remove,
		)
		if err != nil {
			return total, fmt.Errorf("cap browser activity: %w", err)
		}
		affected, _ := result.RowsAffected()
		total += affected
		count -= affected
		if affected == 0 {
			break
		}
	}
	if _, err := s.db.Exec(`DELETE FROM browser_history_alerts WHERE time < ?`, formatActivityTime(cutoff)); err != nil {
		return total, fmt.Errorf("prune browser history alerts: %w", err)
	}
	return total, nil
}

func activitySourceKey(browser, profileKey string, visitedAt time.Time, url string) string {
	sum := sha256.Sum256([]byte(browser + "|" + profileKey + "|" + visitedAt.UTC().Format(time.RFC3339Nano) + "|" + url))
	return hex.EncodeToString(sum[:])
}

func truncateText(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	for len(value) > max {
		_, size := utf8.DecodeLastRuneInString(value)
		if size <= 0 {
			break
		}
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}

func formatActivityTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
