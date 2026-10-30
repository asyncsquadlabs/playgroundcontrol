package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/domains"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	defaultBrowserDomains = 500
	// MaxBrowserDomains is the URLBlocklist-compatible blocked-domain cap.
	MaxBrowserDomains       = 1_000
	browserDetectionMaxRows = 5_000
	browserDetectionWindow  = 10 * time.Minute
	browserDomainColumns    = `id, domain, source, status, confidence, reason,
		first_seen, last_seen, seen_count, model, updated_at`
)

var (
	// ErrDuplicateBrowserDomain reports an existing blocked/manual domain.
	ErrDuplicateBrowserDomain = errors.New("that domain is already blocked")
	// ErrBrowserDomainNotFound reports a missing domain row.
	ErrBrowserDomainNotFound = errors.New("browser domain not found")
	// ErrBrowserDomainLimit reports that the blocked-domain cap was reached.
	ErrBrowserDomainLimit = model.ErrBrowserDomainLimit
)

// BrowserSettings returns the non-secret browser settings view. KeyConfigured
// reflects the shared DeepSeek key used for optional domain classification.
func (s *Store) BrowserSettings() model.BrowserSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	settings := s.browser
	settings.KeyConfigured = s.ai.KeyConfigured
	return settings
}

// SaveBrowserSettings persists browser monitoring configuration.
func (s *Store) SaveBrowserSettings(enabled, autoBlock, aiClassification bool) (model.BrowserSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return model.BrowserSettings{}, errors.New("database is not open")
	}
	if aiClassification && !s.ai.KeyConfigured {
		return model.BrowserSettings{}, ErrAIKeyRequired
	}
	if _, err := s.db.Exec(
		`UPDATE browser_settings SET enabled = ?, auto_block = ?, ai_classification = ? WHERE id = 1`,
		boolInt(enabled), boolInt(autoBlock), boolInt(aiClassification),
	); err != nil {
		return model.BrowserSettings{}, fmt.Errorf("save browser settings: %w", err)
	}
	settings := s.browser
	settings.Enabled = enabled
	settings.AutoBlock = autoBlock
	settings.AIClassification = aiClassification
	settings.KeyConfigured = s.ai.KeyConfigured
	s.browser = settings
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup after browser configuration change: %v", err))
	}
	return settings, nil
}

// SetBrowserScanStatus records the latest poll and provider health. A non-zero
// scan time advances last_scan_at; a non-empty lastError records an error,
// while an empty one clears the previous error.
func (s *Store) SetBrowserScanStatus(lastScan time.Time, lastError string) error {
	lastError = truncate(strings.TrimSpace(lastError), maxEventLength)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("database is not open")
	}

	settings := s.browser
	if !lastScan.IsZero() {
		settings.LastScanAt = lastScan.UTC()
		if _, err := s.db.Exec(`UPDATE browser_settings SET last_scan_at = ? WHERE id = 1`, formatTime(settings.LastScanAt)); err != nil {
			return fmt.Errorf("save browser scan status: %w", err)
		}
	}
	if lastError == "" {
		if settings.LastError != "" {
			if _, err := s.db.Exec(`UPDATE browser_settings SET last_error = '', last_error_at = NULL WHERE id = 1`); err != nil {
				return fmt.Errorf("clear browser status: %w", err)
			}
		}
		settings.LastError = ""
	} else {
		if _, err := s.db.Exec(`UPDATE browser_settings SET last_error = ?, last_error_at = ? WHERE id = 1`,
			lastError, formatTime(s.now().UTC())); err != nil {
			return fmt.Errorf("save browser status: %w", err)
		}
		settings.LastError = lastError
	}
	s.browser = settings
	return nil
}

func (s *Store) loadBrowserSettings() error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	var enabled, autoBlock, aiClassification int
	var lastScan, lastError sql.NullString
	err := s.db.QueryRow(
		`SELECT enabled, auto_block, ai_classification, last_scan_at, last_error
		 FROM browser_settings WHERE id = 1`,
	).Scan(&enabled, &autoBlock, &aiClassification, &lastScan, &lastError)
	if err != nil {
		return fmt.Errorf("load browser settings: %w", err)
	}
	settings := model.DefaultBrowserSettings()
	settings.Enabled = enabled != 0
	settings.AutoBlock = autoBlock != 0
	settings.AIClassification = aiClassification != 0
	settings.LastScanAt = parseTime(lastScan.String)
	settings.LastError = truncate(strings.TrimSpace(lastError.String), maxEventLength)
	settings.KeyConfigured = s.ai.KeyConfigured
	s.browser = settings
	return nil
}

// BrowserBlockedDomains returns normalized domains that must be enforced when
// play is blocked.
func (s *Store) BrowserBlockedDomains() ([]string, error) {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	rows, err := s.db.Query(`SELECT domain FROM browser_domains WHERE status = ? ORDER BY domain`, model.BrowserStatusBlocked)
	if err != nil {
		return nil, fmt.Errorf("read blocked browser domains: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var domain string
		if err := rows.Scan(&domain); err != nil {
			return nil, fmt.Errorf("read blocked browser domain: %w", err)
		}
		out = append(out, domain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read blocked browser domains: %w", err)
	}
	return out, nil
}

// BrowserDomainByDomain returns one domain row when present.
func (s *Store) BrowserDomainByDomain(raw string) (model.BrowserDomain, bool, error) {
	domain, err := domains.Normalize(raw)
	if err != nil {
		return model.BrowserDomain{}, false, err
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return model.BrowserDomain{}, false, errors.New("database is not open")
	}
	return s.browserDomainByDomainLocked(domain)
}

// BrowserDomains returns domains with an optional status filter.
func (s *Store) BrowserDomains(status string, limit int) ([]model.BrowserDomain, error) {
	if status != "" && !validBrowserStatus(status) {
		return nil, errors.New("invalid browser domain status")
	}
	if limit <= 0 {
		limit = defaultBrowserDomains
	}
	if limit > MaxBrowserDomains {
		limit = MaxBrowserDomains
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	var rows *sql.Rows
	var err error
	if status == "" {
		// #nosec G202 -- browserDomainColumns is a compile-time constant.
		rows, err = s.db.Query(`SELECT `+browserDomainColumns+` FROM browser_domains
			ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
	} else {
		// #nosec G202 -- browserDomainColumns is a compile-time constant.
		rows, err = s.db.Query(`SELECT `+browserDomainColumns+` FROM browser_domains
			WHERE status = ? ORDER BY updated_at DESC, id DESC LIMIT ?`, status, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("read browser domains: %w", err)
	}
	defer rows.Close()
	out := make([]model.BrowserDomain, 0)
	for rows.Next() {
		item, err := scanBrowserDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read browser domains: %w", err)
	}
	return out, nil
}

// BrowserDomainCounts returns counts for the dashboard summary.
func (s *Store) BrowserDomainCounts() (blocked, review, allowed int, err error) {
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return 0, 0, 0, errors.New("database is not open")
	}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM browser_domains GROUP BY status`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("count browser domains: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return 0, 0, 0, fmt.Errorf("count browser domains: %w", err)
		}
		switch status {
		case model.BrowserStatusBlocked:
			blocked = count
		case model.BrowserStatusReview:
			review = count
		case model.BrowserStatusAllowed:
			allowed = count
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("count browser domains: %w", err)
	}
	return blocked, review, allowed, nil
}

// AddBrowserDomain adds or re-blocks a parent-managed domain.
func (s *Store) AddBrowserDomain(raw string, now time.Time) (model.BrowserDomain, error) {
	domain, err := domains.Normalize(raw)
	if err != nil {
		return model.BrowserDomain{}, err
	}
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return model.BrowserDomain{}, errors.New("database is not open")
	}
	existing, found, err := s.browserDomainByDomainLocked(domain)
	if err != nil {
		return model.BrowserDomain{}, err
	}
	if found {
		if existing.Status == model.BrowserStatusBlocked {
			return model.BrowserDomain{}, ErrDuplicateBrowserDomain
		}
		if err := s.checkBlockedDomainCapacityLocked(existing.ID); err != nil {
			return model.BrowserDomain{}, err
		}
		if _, err := s.db.Exec(
			`UPDATE browser_domains SET source = ?, status = ?, confidence = 0, reason = ?, model = '', updated_at = ?
			 WHERE id = ?`,
			model.BrowserSourceManual, model.BrowserStatusBlocked, "Added by the administrator.", formatTime(now), existing.ID,
		); err != nil {
			return model.BrowserDomain{}, fmt.Errorf("block browser domain: %w", err)
		}
		item, _, err := s.browserDomainByDomainLocked(domain)
		if err != nil {
			return model.BrowserDomain{}, err
		}
		s.refreshBrowserBackup()
		return item, nil
	}

	if err := s.checkBlockedDomainCapacityLocked(""); err != nil {
		return model.BrowserDomain{}, err
	}
	id, err := model.NewID()
	if err != nil {
		return model.BrowserDomain{}, err
	}
	if _, err := s.db.Exec(
		`INSERT INTO browser_domains
		 (id, domain, source, status, confidence, reason, first_seen, last_seen, seen_count, model, updated_at)
		 VALUES (?, ?, ?, ?, 0, ?, ?, ?, 1, '', ?)`,
		id, domain, model.BrowserSourceManual, model.BrowserStatusBlocked,
		"Added by the administrator.", formatTime(now), formatTime(now), formatTime(now),
	); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return model.BrowserDomain{}, ErrDuplicateBrowserDomain
		}
		return model.BrowserDomain{}, fmt.Errorf("save browser domain: %w", err)
	}
	item, _, err := s.browserDomainByDomainLocked(domain)
	if err != nil {
		return model.BrowserDomain{}, err
	}
	s.refreshBrowserBackup()
	return item, nil
}

// SetBrowserDomainStatus changes a domain to blocked or allowed. A parent
// allowlist is persistent and overrides future seed/AI detections.
func (s *Store) SetBrowserDomainStatus(id, status string, now time.Time) (model.BrowserDomain, error) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status != model.BrowserStatusBlocked && status != model.BrowserStatusAllowed {
		return model.BrowserDomain{}, errors.New("browser domain status must be blocked or allowed")
	}
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return model.BrowserDomain{}, errors.New("database is not open")
	}
	item, found, err := s.browserDomainByIDLocked(id)
	if err != nil {
		return model.BrowserDomain{}, err
	}
	if !found {
		return model.BrowserDomain{}, ErrBrowserDomainNotFound
	}
	if status == model.BrowserStatusBlocked && item.Status != model.BrowserStatusBlocked {
		if err := s.checkBlockedDomainCapacityLocked(item.ID); err != nil {
			return model.BrowserDomain{}, err
		}
	}
	if _, err := s.db.Exec(
		`UPDATE browser_domains SET status = ?, updated_at = ? WHERE id = ?`,
		status, formatTime(now), id,
	); err != nil {
		return model.BrowserDomain{}, fmt.Errorf("set browser domain status: %w", err)
	}
	updated, found, err := s.browserDomainByIDLocked(id)
	if err != nil {
		return model.BrowserDomain{}, err
	}
	if !found {
		return model.BrowserDomain{}, ErrBrowserDomainNotFound
	}
	s.refreshBrowserBackup()
	return updated, nil
}

// RecordBrowserSeed records a deterministic seed match. It returns the stored
// domain and whether enforcement should be refreshed.
func (s *Store) RecordBrowserSeed(domain, reason string, now time.Time) (model.BrowserDomain, bool, error) {
	domain, err := domains.Normalize(domain)
	if err != nil {
		return model.BrowserDomain{}, false, err
	}
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	reason = truncate(strings.TrimSpace(reason), maxEventLength)
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return model.BrowserDomain{}, false, errors.New("database is not open")
	}
	existing, found, err := s.browserDomainByDomainLocked(domain)
	if err != nil {
		return model.BrowserDomain{}, false, err
	}
	if found {
		touch := `UPDATE browser_domains SET last_seen = ?, seen_count = seen_count + 1, updated_at = ? WHERE id = ?`
		if _, err := s.db.Exec(touch, formatTime(now), formatTime(now), existing.ID); err != nil {
			return model.BrowserDomain{}, false, fmt.Errorf("touch browser domain: %w", err)
		}
		if existing.Status == model.BrowserStatusAllowed {
			item, _, err := s.browserDomainByIDLocked(existing.ID)
			return item, false, err
		}
		if existing.Status != model.BrowserStatusBlocked {
			if err := s.checkBlockedDomainCapacityLocked(existing.ID); err != nil {
				return model.BrowserDomain{}, false, err
			}
			if _, err := s.db.Exec(
				`UPDATE browser_domains SET source = ?, status = ?, confidence = 1, reason = ?, model = '', updated_at = ? WHERE id = ?`,
				model.BrowserSourceSeed, model.BrowserStatusBlocked, reason, formatTime(now), existing.ID,
			); err != nil {
				return model.BrowserDomain{}, false, fmt.Errorf("block seed domain: %w", err)
			}
			s.refreshBrowserBackup()
			item, _, err := s.browserDomainByIDLocked(existing.ID)
			return item, true, err
		}
		item, _, err := s.browserDomainByIDLocked(existing.ID)
		return item, false, err
	}
	if err := s.checkBlockedDomainCapacityLocked(""); err != nil {
		return model.BrowserDomain{}, false, err
	}
	id, err := model.NewID()
	if err != nil {
		return model.BrowserDomain{}, false, err
	}
	if _, err := s.db.Exec(
		`INSERT INTO browser_domains
		 (id, domain, source, status, confidence, reason, first_seen, last_seen, seen_count, model, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?, 1, '', ?)`,
		id, domain, model.BrowserSourceSeed, model.BrowserStatusBlocked, reason,
		formatTime(now), formatTime(now), formatTime(now),
	); err != nil {
		return model.BrowserDomain{}, false, fmt.Errorf("save seed browser domain: %w", err)
	}
	item, _, err := s.browserDomainByIDLocked(id)
	if err != nil {
		return model.BrowserDomain{}, false, err
	}
	s.refreshBrowserBackup()
	return item, true, nil
}

// RecordBrowserAIVerdict records a DeepSeek domain verdict. It returns the
// stored row when one exists, the resulting action, and whether enforcement
// should be refreshed. Domains below the review floor return no row.
func (s *Store) RecordBrowserAIVerdict(domain string, confidence float64, reason, modelName string, autoBlock bool, now time.Time) (model.BrowserDomain, string, bool, error) {
	domain, err := domains.Normalize(domain)
	if err != nil {
		return model.BrowserDomain{}, "", false, err
	}
	if now.IsZero() {
		now = s.now().UTC()
	} else {
		now = now.UTC()
	}
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	reason = truncate(strings.TrimSpace(reason), maxEventLength)
	modelName = truncate(strings.TrimSpace(modelName), 64)

	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return model.BrowserDomain{}, "", false, errors.New("database is not open")
	}
	existing, found, err := s.browserDomainByDomainLocked(domain)
	if err != nil {
		return model.BrowserDomain{}, "", false, err
	}
	if found {
		if _, err := s.db.Exec(
			`UPDATE browser_domains SET last_seen = ?, seen_count = seen_count + 1, updated_at = ? WHERE id = ?`,
			formatTime(now), formatTime(now), existing.ID,
		); err != nil {
			return model.BrowserDomain{}, "", false, fmt.Errorf("touch browser domain: %w", err)
		}
		if existing.Status == model.BrowserStatusAllowed || existing.Status == model.BrowserStatusBlocked {
			item, _, err := s.browserDomainByIDLocked(existing.ID)
			return item, "existing", false, err
		}
		// A review row can be promoted or remain in review.
		status := model.BrowserStatusReview
		action := "review"
		if confidence >= model.BrowserAutoBlockConfidence && autoBlock {
			if err := s.checkBlockedDomainCapacityLocked(existing.ID); err == nil {
				status = model.BrowserStatusBlocked
				action = "ai-auto-blocked"
			}
		}
		if confidence < model.BrowserMinConfidence {
			return model.BrowserDomain{}, "", false, nil
		}
		if _, err := s.db.Exec(
			`UPDATE browser_domains SET source = ?, status = ?, confidence = ?, reason = ?, model = ?, updated_at = ? WHERE id = ?`,
			model.BrowserSourceAI, status, confidence, reason, modelName, formatTime(now), existing.ID,
		); err != nil {
			return model.BrowserDomain{}, "", false, fmt.Errorf("update AI browser domain: %w", err)
		}
		item, _, err := s.browserDomainByIDLocked(existing.ID)
		if err != nil {
			return model.BrowserDomain{}, "", false, err
		}
		if action == "ai-auto-blocked" {
			s.refreshBrowserBackup()
			return item, action, true, nil
		}
		return item, action, false, nil
	}

	switch {
	case confidence < model.BrowserMinConfidence:
		return model.BrowserDomain{}, "", false, nil
	case confidence >= model.BrowserAutoBlockConfidence && autoBlock:
		if err := s.checkBlockedDomainCapacityLocked(""); err != nil {
			return model.BrowserDomain{}, "", false, err
		}
		id, err := model.NewID()
		if err != nil {
			return model.BrowserDomain{}, "", false, err
		}
		if _, err := s.db.Exec(
			`INSERT INTO browser_domains
			 (id, domain, source, status, confidence, reason, first_seen, last_seen, seen_count, model, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			id, domain, model.BrowserSourceAI, model.BrowserStatusBlocked, confidence, reason,
			formatTime(now), formatTime(now), modelName, formatTime(now),
		); err != nil {
			return model.BrowserDomain{}, "", false, fmt.Errorf("save AI browser domain: %w", err)
		}
		item, _, err := s.browserDomainByIDLocked(id)
		if err != nil {
			return model.BrowserDomain{}, "", false, err
		}
		s.refreshBrowserBackup()
		return item, "ai-auto-blocked", true, nil
	default:
		id, err := model.NewID()
		if err != nil {
			return model.BrowserDomain{}, "", false, err
		}
		if _, err := s.db.Exec(
			`INSERT INTO browser_domains
			 (id, domain, source, status, confidence, reason, first_seen, last_seen, seen_count, model, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			id, domain, model.BrowserSourceAI, model.BrowserStatusReview, confidence, reason,
			formatTime(now), formatTime(now), modelName, formatTime(now),
		); err != nil {
			return model.BrowserDomain{}, "", false, fmt.Errorf("save AI browser review: %w", err)
		}
		item, _, err := s.browserDomainByIDLocked(id)
		if err != nil {
			return model.BrowserDomain{}, "", false, err
		}
		s.refreshBrowserBackup()
		return item, "review", false, nil
	}
}

// RecordBrowserDetection inserts or refreshes one non-secret detection.
func (s *Store) RecordBrowserDetection(detection model.BrowserDetection) error {
	detection.Browser = strings.ToLower(strings.TrimSpace(detection.Browser))
	if detection.Browser != "chrome" && detection.Browser != "edge" {
		return errors.New("browser must be chrome or edge")
	}
	domain, err := domains.Normalize(detection.Domain)
	if err != nil {
		return err
	}
	if detection.Time.IsZero() {
		detection.Time = s.now().UTC()
	} else {
		detection.Time = detection.Time.UTC()
	}
	detection.Domain = domain
	detection.Action = truncate(strings.TrimSpace(detection.Action), 64)
	detection.Reason = truncate(strings.TrimSpace(detection.Reason), maxEventLength)
	if detection.Action == "" {
		detection.Action = "detected"
	}

	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return errors.New("database is not open")
	}
	var existing int64
	err = s.db.QueryRow(
		`SELECT seq FROM browser_detections
		 WHERE browser = ? AND domain = ? AND action = ? AND time >= ?
		 ORDER BY seq DESC LIMIT 1`,
		detection.Browser, detection.Domain, detection.Action, formatTime(detection.Time.Add(-browserDetectionWindow)),
	).Scan(&existing)
	switch {
	case err == nil:
		if _, err := s.db.Exec(
			`UPDATE browser_detections SET time = ?, source = ?, confidence = ?, reason = ? WHERE seq = ?`,
			formatTime(detection.Time), detection.Source, detection.Confidence, detection.Reason, existing,
		); err != nil {
			return fmt.Errorf("refresh browser detection: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.Exec(
			`INSERT INTO browser_detections (time, browser, domain, source, action, confidence, reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			formatTime(detection.Time), detection.Browser, detection.Domain,
			detection.Source, detection.Action, detection.Confidence, detection.Reason,
		); err != nil {
			return fmt.Errorf("save browser detection: %w", err)
		}
	default:
		return fmt.Errorf("check browser detection: %w", err)
	}
	_, _ = s.db.Exec(
		`DELETE FROM browser_detections WHERE seq <= (SELECT MAX(seq) FROM browser_detections) - ?`,
		browserDetectionMaxRows,
	)
	return nil
}

// RecentBrowserDetections returns newest detections first.
func (s *Store) RecentBrowserDetections(limit int) ([]model.BrowserDetection, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	s.browserMu.Lock()
	defer s.browserMu.Unlock()
	if s.db == nil {
		return nil, errors.New("database is not open")
	}
	rows, err := s.db.Query(
		`SELECT seq, time, browser, domain, source, action, confidence, reason
		 FROM browser_detections ORDER BY time DESC, seq DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("read browser detections: %w", err)
	}
	defer rows.Close()
	out := make([]model.BrowserDetection, 0)
	for rows.Next() {
		var item model.BrowserDetection
		var seq int64
		var when string
		if err := rows.Scan(&seq, &when, &item.Browser, &item.Domain, &item.Source, &item.Action, &item.Confidence, &item.Reason); err != nil {
			return nil, fmt.Errorf("read browser detection: %w", err)
		}
		item.ID = strconv.FormatInt(seq, 10)
		item.Time = parseTime(when)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read browser detections: %w", err)
	}
	return out, nil
}

func (s *Store) checkBlockedDomainCapacityLocked(excludeID string) error {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_domains WHERE status = ?`, model.BrowserStatusBlocked).Scan(&count); err != nil {
		return fmt.Errorf("count blocked browser domains: %w", err)
	}
	if excludeID != "" {
		var current int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM browser_domains WHERE status = ? AND id = ?`, model.BrowserStatusBlocked, excludeID).Scan(&current); err != nil {
			return fmt.Errorf("check blocked browser domain: %w", err)
		}
		count -= current
	}
	if count >= MaxBrowserDomains {
		return ErrBrowserDomainLimit
	}
	return nil
}

func (s *Store) browserDomainByDomainLocked(domain string) (model.BrowserDomain, bool, error) {
	// #nosec G202 -- browserDomainColumns is a compile-time constant.
	item, err := scanBrowserDomain(s.db.QueryRow(
		`SELECT `+browserDomainColumns+` FROM browser_domains WHERE domain = ?`, domain,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return model.BrowserDomain{}, false, nil
	}
	if err != nil {
		return model.BrowserDomain{}, false, fmt.Errorf("read browser domain: %w", err)
	}
	return item, true, nil
}

func (s *Store) browserDomainByIDLocked(id string) (model.BrowserDomain, bool, error) {
	// #nosec G202 -- browserDomainColumns is a compile-time constant.
	item, err := scanBrowserDomain(s.db.QueryRow(
		`SELECT `+browserDomainColumns+` FROM browser_domains WHERE id = ?`, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return model.BrowserDomain{}, false, nil
	}
	if err != nil {
		return model.BrowserDomain{}, false, fmt.Errorf("read browser domain: %w", err)
	}
	return item, true, nil
}

func scanBrowserDomain(scanner rowScanner) (model.BrowserDomain, error) {
	var item model.BrowserDomain
	var firstSeen, lastSeen, updatedAt string
	err := scanner.Scan(
		&item.ID, &item.Domain, &item.Source, &item.Status, &item.Confidence, &item.Reason,
		&firstSeen, &lastSeen, &item.SeenCount, &item.Model, &updatedAt,
	)
	if err != nil {
		return model.BrowserDomain{}, err
	}
	item.FirstSeen = parseTime(firstSeen)
	item.LastSeen = parseTime(lastSeen)
	item.UpdatedAt = parseTime(updatedAt)
	return item, nil
}

func (s *Store) refreshBrowserBackup() {
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup after browser domain change: %v", err))
	}
}

func validBrowserStatus(status string) bool {
	switch status {
	case model.BrowserStatusBlocked, model.BrowserStatusReview, model.BrowserStatusAllowed:
		return true
	default:
		return false
	}
}
