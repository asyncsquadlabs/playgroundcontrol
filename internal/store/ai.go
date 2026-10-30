package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

var (
	// ErrAIConfiguration reports invalid AI settings.
	ErrAIConfiguration = errors.New("invalid AI monitoring configuration")
	// ErrAIKeyRequired reports that AI monitoring cannot be enabled without a
	// saved DeepSeek API key.
	ErrAIKeyRequired = errors.New("a DeepSeek API key is required")
)

// AISettings returns the non-secret AI settings view.
func (s *Store) AISettings() model.AISettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ai
}

// AICredentials returns the safe settings view plus the in-memory API key.
// Callers must never log or serialize the key.
func (s *Store) AICredentials() (model.AISettings, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ai, s.aiKey, s.ai.KeyConfigured && s.aiKey != ""
}

// SaveAISettings persists AI monitoring configuration. An empty apiKey keeps
// the existing key. clearKey removes it and requires enabled to be false.
func (s *Store) SaveAISettings(enabled, autoBlock bool, modelName, apiKey string, clearKey bool) (model.AISettings, error) {
	modelName, err := normalizeAIModel(modelName)
	if err != nil {
		return model.AISettings{}, err
	}
	apiKey = strings.TrimSpace(apiKey)
	if clearKey && apiKey != "" {
		return model.AISettings{}, fmt.Errorf("%w: provide a new key or clear the existing key, not both", ErrAIConfiguration)
	}
	if clearKey && enabled {
		return model.AISettings{}, fmt.Errorf("%w: disable AI monitoring before removing the API key", ErrAIConfiguration)
	}
	if apiKey != "" {
		if err := validateAPIKey(apiKey); err != nil {
			return model.AISettings{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if clearKey && s.browser.AIClassification {
		return model.AISettings{}, fmt.Errorf("%w: disable browser AI classification before removing the API key", ErrAIConfiguration)
	}

	if s.db == nil {
		return model.AISettings{}, errors.New("database is not open")
	}

	var currentBlob []byte
	var currentHint string
	if err := s.db.QueryRow(`SELECT api_key_enc, key_hint FROM ai_settings WHERE id = 1`).Scan(&currentBlob, &currentHint); err != nil {
		return model.AISettings{}, fmt.Errorf("read AI settings: %w", err)
	}

	newBlob := append([]byte(nil), currentBlob...)
	if newBlob == nil {
		newBlob = []byte{}
	}
	newHint := currentHint
	newKey := s.aiKey
	keyChanged := false
	switch {
	case clearKey:
		newBlob = []byte{}
		newHint = ""
		newKey = ""
		keyChanged = true
	case apiKey != "":
		sealed, err := s.codec.Seal([]byte(apiKey))
		if err != nil {
			return model.AISettings{}, fmt.Errorf("protect DeepSeek API key: %w", err)
		}
		newBlob = sealed
		newHint = keyHint(apiKey)
		newKey = apiKey
		keyChanged = true
	}
	if enabled && newKey == "" {
		return model.AISettings{}, ErrAIKeyRequired
	}

	settings := s.ai
	settings.Enabled = enabled
	settings.AutoBlock = autoBlock
	settings.Model = modelName
	settings.KeyConfigured = newKey != ""
	settings.KeyHint = ""
	if settings.KeyConfigured {
		settings.KeyHint = newHint
	}
	resetStatus := keyChanged || modelName != s.ai.Model
	if resetStatus {
		settings.LastVerifiedAt = time.Time{}
		settings.LastError = ""
	}

	var lastVerified any
	if !settings.LastVerifiedAt.IsZero() {
		lastVerified = formatTime(settings.LastVerifiedAt)
	}
	_, err = s.db.Exec(
		`UPDATE ai_settings SET
			enabled = ?, auto_block = ?, model = ?, api_key_enc = ?, key_hint = ?,
			last_verified_at = ?, last_error = ?,
			last_error_at = CASE WHEN ? THEN NULL ELSE last_error_at END
		 WHERE id = 1`,
		boolInt(settings.Enabled),
		boolInt(settings.AutoBlock),
		settings.Model,
		newBlob,
		settings.KeyHint,
		lastVerified,
		settings.LastError,
		resetStatus,
	)
	if err != nil {
		return model.AISettings{}, fmt.Errorf("save AI settings: %w", err)
	}

	s.ai = settings
	s.aiKey = newKey
	if err := s.refreshBackup(); err != nil {
		s.loadNotice = appendNoticeText(s.loadNotice, fmt.Sprintf("could not refresh database backup after AI configuration change: %v", err))
	}
	return settings, nil
}

// SetAIStatus updates provider health. A non-zero lastVerified marks a success
// and clears the current error; a zero time records an error.
func (s *Store) SetAIStatus(lastVerified time.Time, lastError string) error {
	lastError = truncate(strings.TrimSpace(lastError), maxEventLength)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return errors.New("database is not open")
	}

	settings := s.ai
	settings.LastError = lastError
	if !lastVerified.IsZero() {
		settings.LastVerifiedAt = lastVerified.UTC()
		_, err := s.db.Exec(
			`UPDATE ai_settings SET last_verified_at = ?, last_error = '', last_error_at = NULL WHERE id = 1`,
			formatTime(settings.LastVerifiedAt),
		)
		if err != nil {
			return fmt.Errorf("save AI status: %w", err)
		}
	} else {
		_, err := s.db.Exec(
			`UPDATE ai_settings SET last_error = ?, last_error_at = ? WHERE id = 1`,
			lastError,
			formatTime(s.now().UTC()),
		)
		if err != nil {
			return fmt.Errorf("save AI status: %w", err)
		}
	}
	s.ai = settings
	return nil
}

func (s *Store) loadAISettings() error {
	if s.db == nil {
		return errors.New("database is not open")
	}
	var enabled, autoBlock int
	var modelName string
	var sealed []byte
	var hint string
	var lastVerified, lastError sql.NullString
	err := s.db.QueryRow(
		`SELECT enabled, auto_block, model, api_key_enc, key_hint, last_verified_at, last_error
		 FROM ai_settings WHERE id = 1`,
	).Scan(&enabled, &autoBlock, &modelName, &sealed, &hint, &lastVerified, &lastError)
	if err != nil {
		return fmt.Errorf("load AI settings: %w", err)
	}

	normalized, err := normalizeAIModel(modelName)
	if err != nil {
		normalized = model.DefaultAIModel
	}
	settings := model.DefaultAISettings()
	settings.Enabled = enabled != 0
	settings.AutoBlock = autoBlock != 0
	settings.Model = normalized
	settings.LastVerifiedAt = parseTime(lastVerified.String)
	settings.LastError = truncate(strings.TrimSpace(lastError.String), maxEventLength)
	settings.KeyHint = hint

	if len(sealed) > 0 {
		plain, err := s.codec.Open(sealed)
		if err != nil {
			settings.KeyConfigured = false
			settings.KeyHint = ""
			settings.LastError = "the saved DeepSeek API key could not be decrypted; re-enter it"
			s.ai = settings
			s.aiKey = ""
			return fmt.Errorf("decrypt saved DeepSeek API key: %w", err)
		}
		s.aiKey = string(plain)
		settings.KeyConfigured = s.aiKey != ""
	} else {
		s.aiKey = ""
		settings.KeyConfigured = false
	}
	if !settings.KeyConfigured {
		settings.KeyHint = ""
	}
	s.ai = settings
	return nil
}

func normalizeAIModel(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return model.DefaultAIModel, nil
	}
	if len(value) > 64 {
		return "", fmt.Errorf("%w: model name is too long", ErrAIConfiguration)
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return "", fmt.Errorf("%w: model name contains invalid characters", ErrAIConfiguration)
	}
	return value, nil
}

func validateAPIKey(value string) error {
	if len(value) < 8 || len(value) > 512 {
		return fmt.Errorf("%w: API key must be 8 to 512 characters", ErrAIConfiguration)
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%w: API key contains invalid characters", ErrAIConfiguration)
		}
	}
	return nil
}

func keyHint(key string) string {
	if len(key) <= 4 {
		return key
	}
	return key[len(key)-4:]
}
