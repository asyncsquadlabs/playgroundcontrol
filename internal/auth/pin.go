package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

var (
	ErrPINFormat = errors.New("PIN must be 6 to 12 digits")
	ErrBadAuth   = errors.New("authentication record is invalid")
)

// Record is the only authentication data persisted to disk.
type Record struct {
	Version   int       `json:"version"`
	Algorithm string    `json:"algorithm"`
	Salt      string    `json:"salt"`
	Hash      string    `json:"hash"`
	Time      uint32    `json:"time"`
	MemoryKiB uint32    `json:"memoryKiB"`
	Threads   uint8     `json:"threads"`
	KeyLen    uint32    `json:"keyLen"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ValidatePINFormat enforces the documented numeric PIN policy.
func ValidatePINFormat(pin string) error {
	if len(pin) < 6 || len(pin) > 12 {
		return ErrPINFormat
	}
	for i := 0; i < len(pin); i++ {
		if pin[i] < '0' || pin[i] > '9' {
			return ErrPINFormat
		}
	}
	return nil
}

// HashPIN creates an Argon2id record for a PIN.
func HashPIN(pin string) (Record, error) {
	if err := ValidatePINFormat(pin); err != nil {
		return Record{}, err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return Record{}, fmt.Errorf("generate PIN salt: %w", err)
	}
	key := argon2.IDKey([]byte(pin), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return Record{
		Version:   1,
		Algorithm: "argon2id",
		Salt:      base64.RawStdEncoding.EncodeToString(salt),
		Hash:      base64.RawStdEncoding.EncodeToString(key),
		Time:      argonTime,
		MemoryKiB: argonMemory,
		Threads:   argonThreads,
		KeyLen:    argonKeyLen,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

// DecodeRecord parses and validates a legacy authentication record.
func DecodeRecord(data []byte) (Record, error) {
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrBadAuth, err)
	}
	if !record.valid() {
		return Record{}, ErrBadAuth
	}
	return record, nil
}

func (r Record) valid() bool {
	if r.Version != 1 || r.Algorithm != "argon2id" {
		return false
	}
	if r.Time < 1 || r.Time > 10 || r.MemoryKiB < 8*1024 || r.MemoryKiB > 1024*1024 {
		return false
	}
	if r.Threads < 1 || r.Threads > 16 || r.KeyLen < 16 || r.KeyLen > 64 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(r.Salt)
	if err != nil || len(salt) < 12 || len(salt) > 64 {
		return false
	}
	hash, err := base64.RawStdEncoding.DecodeString(r.Hash)
	if err != nil || len(hash) != int(r.KeyLen) {
		return false
	}
	return true
}

func verifyPIN(r Record, pin string) bool {
	if !r.valid() || ValidatePINFormat(pin) != nil {
		return false
	}
	salt, _ := base64.RawStdEncoding.DecodeString(r.Salt)
	expected, _ := base64.RawStdEncoding.DecodeString(r.Hash)
	actual := argon2.IDKey([]byte(pin), salt, r.Time, r.MemoryKiB, r.Threads, r.KeyLen)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
