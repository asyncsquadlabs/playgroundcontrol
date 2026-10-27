// Package auth manages the single admin PIN, one-time setup token, login
// throttling, and in-memory dashboard sessions.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	AuthFileName       = "auth.json"
	SetupTokenFileName = "setup.token"

	setupTokenLifetime = 15 * time.Minute
	sessionIdle        = 30 * time.Minute
	sessionAbsolute    = 12 * time.Hour
	loginWindow        = 15 * time.Minute
	maxLoginFailures   = 5
	loginLockDuration  = 15 * time.Minute
	maxSessions        = 20
)

var (
	ErrSetupRequired  = errors.New("initial local setup is required")
	ErrSetupComplete  = errors.New("administrator PIN is already configured")
	ErrInvalidPIN     = errors.New("incorrect PIN")
	ErrInvalidSession = errors.New("session is missing or expired")
	ErrSetupToken     = errors.New("setup token is invalid or expired")
)

// LockedError reports a temporary login lock for one source IP.
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many failed attempts; try again in %s", e.RetryAfter.Round(time.Second))
}

type session struct {
	csrf     string
	created  time.Time
	lastSeen time.Time
}

type attempt struct {
	windowStart time.Time
	failures    int
	lockedUntil time.Time
}

// Session is the safe subset returned to the HTTP layer.
type Session struct {
	Token string
	CSRF  string
}

// Manager is safe for concurrent use.
type Manager struct {
	dir       string
	authPath  string
	tokenPath string
	now       func() time.Time

	mu          sync.Mutex
	record      *Record
	loadNotice  string
	setupToken  string
	setupExpiry time.Time
	sessions    map[string]*session
	attempts    map[string]*attempt
}

// Open loads the PIN record if present. A corrupt or unreadable record is
// treated as not configured; setup remains token- and loopback-protected.
func Open(dir string, now func() time.Time) (*Manager, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create auth directory: %w", err)
	}
	m := &Manager{
		dir:       dir,
		authPath:  filepath.Join(dir, AuthFileName),
		tokenPath: filepath.Join(dir, SetupTokenFileName),
		now:       now,
		sessions:  make(map[string]*session),
		attempts:  make(map[string]*attempt),
	}
	m.load()
	return m, nil
}

func (m *Manager) load() {
	data, err := os.ReadFile(m.authPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		m.loadNotice = fmt.Sprintf("auth.json could not be read: %v", err)
		return
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		m.loadNotice = fmt.Sprintf("auth.json could not be parsed: %v", err)
		return
	}
	if !record.valid() {
		m.loadNotice = "auth.json contains invalid authentication parameters"
		return
	}
	m.record = &record
}

// SetupTokenPath is where a fresh setup token is made available to the
// elevated local installer.
func (m *Manager) SetupTokenPath() string { return m.tokenPath }

// LoadNotice reports a non-fatal authentication load problem.
func (m *Manager) LoadNotice() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadNotice
}

// SetupRequired reports whether no valid admin PIN is configured.
func (m *Manager) SetupRequired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.record == nil
}

// Prepare creates a new one-time setup token when no PIN exists, or removes a
// stale token when authentication is already configured.
func (m *Manager) Prepare() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.record != nil {
		m.setupToken = ""
		m.setupExpiry = time.Time{}
		_ = os.Remove(m.tokenPath)
		return nil
	}

	token, err := randomToken(32)
	if err != nil {
		return fmt.Errorf("generate setup token: %w", err)
	}
	m.setupToken = token
	m.setupExpiry = m.now().Add(setupTokenLifetime)
	if err := writeFileAtomic(m.tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write setup token: %w", err)
	}
	return nil
}

// SetupTokenValid reports token state without changing it. It is used by
// diagnostics and tests; setup itself performs the authoritative check.
func (m *Manager) SetupTokenValid(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setupTokenValidLocked(token)
}

func (m *Manager) setupTokenValidLocked(token string) bool {
	if m.setupToken == "" || m.now().After(m.setupExpiry) || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(m.setupToken)) == 1
}

// Setup consumes the one-time token, stores a new PIN, and invalidates any
// existing sessions. It does not itself create a session.
func (m *Manager) Setup(token, pin string) error {
	if err := ValidatePINFormat(pin); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.record != nil {
		return ErrSetupComplete
	}
	if !m.setupTokenValidLocked(token) {
		return ErrSetupToken
	}
	record, err := HashPIN(pin)
	if err != nil {
		return err
	}
	if err := writeFileAtomicJSON(m.authPath, record); err != nil {
		return fmt.Errorf("save PIN: %w", err)
	}
	m.record = &record
	m.setupToken = ""
	m.setupExpiry = time.Time{}
	m.sessions = make(map[string]*session)
	_ = os.Remove(m.tokenPath)
	m.loadNotice = ""
	return nil
}

// Login validates the PIN, applies per-source-IP throttling, and creates a
// session on success.
func (m *Manager) Login(ip, pin string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if m.record == nil {
		return Session{}, ErrSetupRequired
	}

	a := m.attempts[ip]
	if a != nil && now.Before(a.lockedUntil) {
		return Session{}, &LockedError{RetryAfter: a.lockedUntil.Sub(now)}
	}
	if a != nil && !a.windowStart.IsZero() && now.Sub(a.windowStart) >= loginWindow {
		delete(m.attempts, ip)
		a = nil
	}

	if !verifyPIN(*m.record, pin) {
		if a == nil {
			a = &attempt{windowStart: now}
			m.attempts[ip] = a
		}
		a.failures++
		if a.failures >= maxLoginFailures {
			a.lockedUntil = now.Add(loginLockDuration)
			return Session{}, &LockedError{RetryAfter: loginLockDuration}
		}
		return Session{}, ErrInvalidPIN
	}

	delete(m.attempts, ip)
	return m.newSessionLocked(now)
}

// CheckLoginLock reports an existing lock without attempting authentication.
func (m *Manager) CheckLoginLock(ip string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.attempts[ip]
	if a == nil {
		return 0
	}
	remaining := a.lockedUntil.Sub(m.now())
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// NewSession creates an authenticated session. Callers must have established
// authorization through setup or login.
func (m *Manager) NewSession() (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record == nil {
		return Session{}, ErrSetupRequired
	}
	return m.newSessionLocked(m.now())
}

func (m *Manager) newSessionLocked(now time.Time) (Session, error) {
	token, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return Session{}, err
	}
	m.sessions[token] = &session{csrf: csrf, created: now, lastSeen: now}
	m.pruneSessionsLocked(now)
	m.enforceSessionLimitLocked()
	return Session{Token: token, CSRF: csrf}, nil
}

// ValidateSession checks idle and absolute expiry, and returns the session's
// CSRF token.
func (m *Manager) ValidateSession(token string) (Session, error) {
	if token == "" {
		return Session{}, ErrInvalidSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	s := m.sessions[token]
	if s == nil {
		return Session{}, ErrInvalidSession
	}
	if now.Sub(s.lastSeen) > sessionIdle || now.Sub(s.created) > sessionAbsolute {
		delete(m.sessions, token)
		return Session{}, ErrInvalidSession
	}
	s.lastSeen = now
	return Session{Token: token, CSRF: s.csrf}, nil
}

// Logout removes one session.
func (m *Manager) Logout(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}

// ChangePIN verifies the current PIN, persists a new one, and invalidates every
// session except the one making the change.
func (m *Manager) ChangePIN(currentToken, currentPIN, newPIN string) error {
	if err := ValidatePINFormat(newPIN); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.record == nil {
		return ErrSetupRequired
	}
	if !verifyPIN(*m.record, currentPIN) {
		return ErrInvalidPIN
	}
	record, err := HashPIN(newPIN)
	if err != nil {
		return err
	}
	if err := writeFileAtomicJSON(m.authPath, record); err != nil {
		return fmt.Errorf("save PIN: %w", err)
	}
	m.record = &record

	for token := range m.sessions {
		if token != currentToken {
			delete(m.sessions, token)
		}
	}
	return nil
}

func (m *Manager) pruneSessionsLocked(now time.Time) {
	for token, s := range m.sessions {
		if now.Sub(s.lastSeen) > sessionIdle || now.Sub(s.created) > sessionAbsolute {
			delete(m.sessions, token)
		}
	}
}

func (m *Manager) enforceSessionLimitLocked() {
	if len(m.sessions) <= maxSessions {
		return
	}
	type item struct {
		token   string
		created time.Time
	}
	items := make([]item, 0, len(m.sessions))
	for token, s := range m.sessions {
		items = append(items, item{token: token, created: s.created})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].created.Before(items[j].created) })
	for i := 0; i < len(items)-maxSessions; i++ {
		delete(m.sessions, items[i].token)
	}
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writeFileAtomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
