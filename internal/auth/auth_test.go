package auth

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	current time.Time
}

func (c *fakeClock) Now() time.Time      { return c.current }
func (c *fakeClock) Add(d time.Duration) { c.current = c.current.Add(d) }

type memoryRecordStore struct {
	mu      sync.Mutex
	record  *Record
	loadErr error
	saveErr error
}

func (m *memoryRecordStore) LoadAuthRecord() (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return Record{}, false, m.loadErr
	}
	if m.record == nil {
		return Record{}, false, nil
	}
	return *m.record, true, nil
}

func (m *memoryRecordStore) SaveAuthRecord(record Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	copied := record
	m.record = &copied
	return nil
}

func configuredManager(t *testing.T, clock *fakeClock, pin string) *Manager {
	t.Helper()
	records := &memoryRecordStore{}
	m, err := Open(t.TempDir(), records, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(m.SetupTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Setup(strings.TrimSpace(string(token)), pin); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPINFormatAndHashing(t *testing.T) {
	for _, pin := range []string{"123456", "123456789012"} {
		if err := ValidatePINFormat(pin); err != nil {
			t.Fatalf("%q rejected: %v", pin, err)
		}
	}
	for _, pin := range []string{"12345", "1234567890123", "12345a", " 123456", ""} {
		if err := ValidatePINFormat(pin); err == nil {
			t.Fatalf("%q should have been rejected", pin)
		}
	}

	record, err := HashPIN("654321")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPIN(record, "654321") {
		t.Fatal("correct PIN did not verify")
	}
	if verifyPIN(record, "654322") {
		t.Fatal("incorrect PIN verified")
	}
	tampered := record
	tampered.Hash = record.Hash[:len(record.Hash)-2] + "AA"
	if verifyPIN(tampered, "654321") {
		t.Fatal("tampered hash verified")
	}
}

func TestSetupConsumesTokenAndPersists(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{current: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	records := &memoryRecordStore{}
	m, err := Open(dir, records, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(); err != nil {
		t.Fatal(err)
	}
	if !m.SetupRequired() {
		t.Fatal("fresh manager should require setup")
	}
	raw, err := os.ReadFile(m.SetupTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	if !m.SetupTokenValid(token) {
		t.Fatal("fresh token was not valid")
	}
	if err := m.Setup("wrong", "123456"); !errors.Is(err, ErrSetupToken) {
		t.Fatalf("wrong token error = %v", err)
	}
	if err := m.Setup(token, "123"); !errors.Is(err, ErrPINFormat) {
		t.Fatalf("short PIN error = %v", err)
	}
	if err := m.Setup(token, "123456"); err != nil {
		t.Fatal(err)
	}
	if m.SetupRequired() {
		t.Fatal("setup should be complete")
	}
	if _, err := os.Stat(m.SetupTokenPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup token was not removed: %v", err)
	}
	if _, err := m.Login("192.168.1.20", "123456"); err != nil {
		t.Fatalf("login after setup: %v", err)
	}

	reloaded, err := Open(dir, records, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.SetupRequired() {
		t.Fatal("PIN did not persist")
	}
	if _, err := reloaded.Login("192.168.1.20", "123456"); err != nil {
		t.Fatalf("login after reload: %v", err)
	}
}

func TestSetupSaveFailureDoesNotMarkConfigured(t *testing.T) {
	dir := t.TempDir()
	records := &memoryRecordStore{}
	m, err := Open(dir, records, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.SetupTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	records.saveErr = errors.New("disk unavailable")
	if err := m.Setup(token, "123456"); err == nil {
		t.Fatal("setup unexpectedly succeeded")
	}
	if !m.SetupRequired() {
		t.Fatal("failed save must leave setup required")
	}
	if !m.SetupTokenValid(token) {
		t.Fatal("failed save must leave the setup token usable")
	}
}

func TestOpenRejectsInvalidRecord(t *testing.T) {
	records := &memoryRecordStore{record: &Record{Version: 99}}
	if _, err := Open(t.TempDir(), records, time.Now); !errors.Is(err, ErrBadAuth) {
		t.Fatalf("invalid record error = %v, want ErrBadAuth", err)
	}
}

func TestOpenPropagatesRecordStoreFailure(t *testing.T) {
	records := &memoryRecordStore{loadErr: errors.New("storage unavailable")}
	if _, err := Open(t.TempDir(), records, time.Now); err == nil {
		t.Fatal("record store failure was ignored")
	}
}

func TestSetupTokenExpires(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m, err := Open(t.TempDir(), &memoryRecordStore{}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(m.SetupTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	clock.Add(setupTokenLifetime + time.Second)
	if m.SetupTokenValid(token) {
		t.Fatal("expired token was valid")
	}
	if err := m.Setup(token, "123456"); !errors.Is(err, ErrSetupToken) {
		t.Fatalf("expired token error = %v", err)
	}
}

func TestLoginLockoutIsPerIPAndRecovers(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := configuredManager(t, clock, "123456")

	for i := 0; i < maxLoginFailures-1; i++ {
		_, err := m.Login("192.168.1.20", "000000")
		if !errors.Is(err, ErrInvalidPIN) {
			t.Fatalf("attempt %d error = %v", i+1, err)
		}
	}
	_, err := m.Login("192.168.1.20", "000000")
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("fifth failure error = %v, want LockedError", err)
	}
	if locked.RetryAfter < 14*time.Minute {
		t.Fatalf("unexpected lock duration %s", locked.RetryAfter)
	}
	if _, err := m.Login("192.168.1.20", "123456"); !errors.As(err, &locked) {
		t.Fatalf("correct PIN should be locked, got %v", err)
	}
	if _, err := m.Login("192.168.1.21", "123456"); err != nil {
		t.Fatalf("other IP should not be locked: %v", err)
	}
	clock.Add(loginLockDuration + time.Second)
	if _, err := m.Login("192.168.1.20", "123456"); err != nil {
		t.Fatalf("login after lock expired: %v", err)
	}
}

func TestSessionExpiryAndLogout(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := configuredManager(t, clock, "123456")

	session, err := m.Login("127.0.0.1", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if session.CSRF == "" || session.Token == "" {
		t.Fatal("session credentials were empty")
	}
	if _, err := m.ValidateSession(session.Token); err != nil {
		t.Fatal(err)
	}
	clock.Add(sessionIdle + time.Second)
	if _, err := m.ValidateSession(session.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("idle-expired session error = %v", err)
	}

	absolute, err := m.Login("127.0.0.1", "123456")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the session active for almost its full absolute lifetime.
	for i := 0; i < 35; i++ {
		clock.Add(20 * time.Minute)
		if _, err := m.ValidateSession(absolute.Token); err != nil {
			t.Fatalf("active session expired early at %s: %v", 20*time.Duration(i+1)*time.Minute, err)
		}
	}
	clock.Add(19 * time.Minute)
	if _, err := m.ValidateSession(absolute.Token); err != nil {
		t.Fatalf("session within absolute lifetime: %v", err)
	}
	clock.Add(2 * time.Minute)
	if _, err := m.ValidateSession(absolute.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("absolute-expired session error = %v", err)
	}

	active, err := m.Login("127.0.0.1", "123456")
	if err != nil {
		t.Fatal(err)
	}
	m.Logout(active.Token)
	if _, err := m.ValidateSession(active.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("logged-out session was accepted")
	}
}

func TestChangePINInvalidatesOtherSessions(t *testing.T) {
	clock := &fakeClock{current: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := configuredManager(t, clock, "123456")
	current, err := m.Login("192.168.1.10", "123456")
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Login("192.168.1.11", "123456")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ChangePIN(current.Token, "000000", "654321"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatalf("wrong current PIN error = %v", err)
	}
	if err := m.ChangePIN(current.Token, "123456", "654321"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ValidateSession(current.Token); err != nil {
		t.Fatalf("current session was invalidated: %v", err)
	}
	if _, err := m.ValidateSession(other.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal("other session was not invalidated")
	}
	if _, err := m.Login("192.168.1.12", "123456"); !errors.Is(err, ErrInvalidPIN) {
		t.Fatal("old PIN still worked")
	}
	if _, err := m.Login("192.168.1.12", "654321"); err != nil {
		t.Fatalf("new PIN did not work: %v", err)
	}
}
