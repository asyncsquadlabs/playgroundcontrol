// Package model contains the persisted configuration and audit types shared
// by the service, API, and enforcement engine.
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	SchemaVersion = 1

	EventPermission  = "permission"
	EventGame        = "game"
	EventAuth        = "auth"
	EventEnforcement = "enforcement"
	EventSystem      = "system"
	EventAI          = "ai"

	DefaultAIModel = "deepseek-flash"

	// GameSourceParent marks a game added by the administrator.
	GameSourceParent = "parent"
	// GameSourceAI marks a game automatically blocked from a DeepSeek verdict.
	GameSourceAI = "ai"
)

// Config is the validated play state shared by persistence, the enforcer, and
// the HTTP API. The zero value is intentionally safe: play is not allowed.
type Config struct {
	SchemaVersion int       `json:"schemaVersion"`
	PlayAllowed   bool      `json:"playAllowed"`
	Games         []Game    `json:"games"`
	LastChangedAt time.Time `json:"lastChangedAt,omitempty"`
	LastChangedBy string    `json:"lastChangedBy,omitempty"`
}

// Game describes one executable that is blocked while play is not allowed.
type Game struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Executable string    `json:"executable"`
	ImagePath  string    `json:"imagePath,omitempty"`
	Source     string    `json:"source"`
	AddedAt    time.Time `json:"addedAt"`
}

// AuditEvent is a non-secret, bounded record of an administrative or
// enforcement action. PINs, tokens, API keys, and cookies must never be placed
// here.
type AuditEvent struct {
	Time     time.Time `json:"time"`
	Type     string    `json:"type"`
	Message  string    `json:"message"`
	ActorIP  string    `json:"actorIp,omitempty"`
	GameID   string    `json:"gameId,omitempty"`
	GameName string    `json:"gameName,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	PID      uint32    `json:"pid,omitempty"`
	Result   string    `json:"result,omitempty"`
}

// AISettings is the non-secret view of DeepSeek monitoring configuration. The
// API key is deliberately not part of this type so it cannot be serialized
// accidentally.
type AISettings struct {
	Enabled        bool      `json:"enabled"`
	AutoBlock      bool      `json:"autoBlock"`
	Model          string    `json:"model"`
	KeyConfigured  bool      `json:"keyConfigured"`
	KeyHint        string    `json:"keyHint,omitempty"`
	LastVerifiedAt time.Time `json:"lastVerifiedAt"`
	LastError      string    `json:"lastError,omitempty"`
}

// DefaultAISettings returns AI monitoring defaults. Monitoring itself stays
// off, while high-confidence auto-block is enabled once a parent turns AI on.
func DefaultAISettings() AISettings {
	return AISettings{Model: DefaultAIModel, AutoBlock: true}
}

// StrangeActivity is one AI-identified game that was running while play was
// blocked and was not already covered by the configured or built-in game list.
// It is evidence for a parent, not an enforcement instruction.
type StrangeActivity struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Executable      string    `json:"executable"`
	ImagePath       string    `json:"imagePath,omitempty"`
	ProductName     string    `json:"productName,omitempty"`
	CompanyName     string    `json:"companyName,omitempty"`
	FileDescription string    `json:"fileDescription,omitempty"`
	Confidence      float64   `json:"confidence"`
	Reason          string    `json:"reason"`
	FirstSeen       time.Time `json:"firstSeen"`
	LastSeen        time.Time `json:"lastSeen"`
	SeenCount       int64     `json:"seenCount"`
	LastPID         uint32    `json:"lastPid,omitempty"`
	Model           string    `json:"model"`
}

// NormalizeStrangeActivity trims and bounds an AI finding. It returns a copy.
func NormalizeStrangeActivity(a StrangeActivity) (StrangeActivity, error) {
	a.Name = strings.TrimSpace(a.Name)
	a.Executable = strings.TrimSpace(a.Executable)
	a.ImagePath = normalizeProcessPath(a.ImagePath)
	a.ProductName = truncateText(strings.TrimSpace(a.ProductName), 200)
	a.CompanyName = truncateText(strings.TrimSpace(a.CompanyName), 200)
	a.FileDescription = truncateText(strings.TrimSpace(a.FileDescription), 200)
	a.Reason = truncateText(strings.TrimSpace(a.Reason), 300)
	a.Model = truncateText(strings.TrimSpace(a.Model), 64)

	if a.Name == "" || containsControl(a.Name) {
		return StrangeActivity{}, errors.New("strange activity display name is required")
	}
	a.Name = truncateText(a.Name, 80)
	if a.Executable == "" || containsControl(a.Executable) || len(a.Executable) > 255 {
		return StrangeActivity{}, errors.New("strange activity executable is invalid")
	}
	if strings.ContainsAny(a.Executable, `/\`) {
		return StrangeActivity{}, errors.New("strange activity executable must be a file name")
	}
	if containsControl(a.ImagePath) || len(a.ImagePath) > 32767 {
		return StrangeActivity{}, errors.New("strange activity image path is invalid")
	}
	if math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) {
		return StrangeActivity{}, errors.New("strange activity confidence is invalid")
	}
	if a.Confidence < 0 {
		a.Confidence = 0
	}
	if a.Confidence > 1 {
		a.Confidence = 1
	}
	if a.Reason == "" {
		a.Reason = "DeepSeek identified this process as a game."
	}
	now := time.Now().UTC()
	if a.FirstSeen.IsZero() {
		a.FirstSeen = now
	} else {
		a.FirstSeen = a.FirstSeen.UTC()
	}
	if a.LastSeen.IsZero() {
		a.LastSeen = a.FirstSeen
	} else {
		a.LastSeen = a.LastSeen.UTC()
	}
	if a.SeenCount < 1 {
		a.SeenCount = 1
	}
	if a.ID == "" {
		id, err := NewID()
		if err != nil {
			return StrangeActivity{}, err
		}
		a.ID = id
	} else if len(a.ID) > 64 || containsControl(a.ID) || strings.ContainsAny(a.ID, `/\`) {
		return StrangeActivity{}, errors.New("invalid strange activity ID")
	}
	if a.Model == "" {
		a.Model = DefaultAIModel
	}
	return a, nil
}

// StrangeActivityKey returns the stable deduplication key for a process.
func StrangeActivityKey(executable, imagePath string) string {
	key := strings.ToLower(strings.TrimSpace(executable)) + "\x00" +
		strings.ToLower(normalizeProcessPath(imagePath))
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// DefaultConfig returns a fresh, fail-closed configuration.
func DefaultConfig() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		PlayAllowed:   false,
		Games:         []Game{},
	}
}

// Normalize fills default schema data and canonicalizes a configuration loaded
// from disk. It does not silently accept invalid games.
func (c *Config) Normalize() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = SchemaVersion
	}
	if c.Games == nil {
		c.Games = []Game{}
	}
	for i := range c.Games {
		g, err := NormalizeGame(c.Games[i])
		if err == nil {
			c.Games[i] = g
		}
	}
}

// Validate checks the whole persisted configuration.
func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported config schema %d", c.SchemaVersion)
	}
	seen := make(map[string]struct{}, len(c.Games))
	for _, g := range c.Games {
		ng, err := NormalizeGame(g)
		if err != nil {
			return fmt.Errorf("game %q: %w", g.Name, err)
		}
		key := gameKey(ng)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate game %q", ng.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// NormalizeGame validates and trims a game. It returns a copy.
func NormalizeGame(g Game) (Game, error) {
	g.Name = strings.TrimSpace(g.Name)
	g.Executable = strings.TrimSpace(g.Executable)
	g.ImagePath = strings.TrimSpace(g.ImagePath)

	if g.Name == "" {
		return Game{}, errors.New("display name is required")
	}
	if len(g.Name) > 80 {
		return Game{}, errors.New("display name is too long (maximum 80 characters)")
	}
	if containsControl(g.Name) {
		return Game{}, errors.New("display name contains control characters")
	}

	exe := strings.TrimSpace(g.Executable)
	if exe == "" {
		return Game{}, errors.New("executable name is required")
	}
	if strings.ContainsAny(exe, `/\`) {
		return Game{}, errors.New("executable must be a file name, not a path")
	}
	if strings.ContainsAny(exe, `<>:"|?*`) {
		return Game{}, errors.New("executable contains invalid Windows filename characters")
	}
	if strings.HasSuffix(exe, ".") || strings.HasSuffix(exe, " ") {
		return Game{}, errors.New("executable must not end with a dot or space")
	}
	if len(exe) > 255 || strings.EqualFold(exe, ".") || strings.EqualFold(exe, "..") {
		return Game{}, errors.New("invalid executable name")
	}
	if !strings.HasSuffix(strings.ToLower(exe), ".exe") {
		return Game{}, errors.New("executable must end with .exe")
	}
	if containsControl(exe) {
		return Game{}, errors.New("executable contains control characters")
	}
	if isCriticalExecutable(exe) {
		return Game{}, fmt.Errorf("%q is a protected Windows or application process", exe)
	}
	g.Executable = exe

	path, err := normalizeImagePath(g.ImagePath)
	if err != nil {
		return Game{}, err
	}
	if path != "" {
		base := path
		if idx := strings.LastIndexByte(base, '\\'); idx >= 0 {
			base = base[idx+1:]
		}
		if !strings.EqualFold(base, exe) {
			return Game{}, errors.New("image path must end with the configured executable name")
		}
	}
	g.ImagePath = path

	if g.ID == "" {
		id, err := NewID()
		if err != nil {
			return Game{}, err
		}
		g.ID = id
	} else if len(g.ID) > 64 || containsControl(g.ID) || strings.ContainsAny(g.ID, `/\`) {
		return Game{}, errors.New("invalid game ID")
	}
	switch strings.ToLower(strings.TrimSpace(g.Source)) {
	case "", GameSourceParent:
		g.Source = GameSourceParent
	case GameSourceAI:
		g.Source = GameSourceAI
	default:
		return Game{}, errors.New("game source must be parent or ai")
	}
	if g.AddedAt.IsZero() {
		g.AddedAt = time.Now().UTC()
	} else {
		g.AddedAt = g.AddedAt.UTC()
	}
	return g, nil
}

func normalizeImagePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	p = strings.ReplaceAll(p, "/", `\`)
	p = strings.TrimPrefix(p, `\\?\`)
	p = strings.TrimPrefix(p, `\??\`)
	if containsControl(p) {
		return "", errors.New("image path contains control characters")
	}
	if len(p) > 32767 {
		return "", errors.New("image path is too long")
	}
	if strings.ContainsAny(p, `?"<>|*`) {
		return "", errors.New("image path contains invalid characters")
	}

	isDrivePath := len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && p[2] == '\\'
	isUNC := strings.HasPrefix(p, `\\`)
	if !isDrivePath && !isUNC {
		return "", errors.New("image path must be an absolute Windows path")
	}
	colonStart := 0
	if isDrivePath {
		colonStart = 2
	}
	for i := colonStart; i < len(p); i++ {
		if p[i] == ':' {
			return "", errors.New("image path contains an invalid colon")
		}
	}
	if !strings.HasSuffix(strings.ToLower(p), ".exe") {
		return "", errors.New("image path must point to an .exe file")
	}
	return p, nil
}

// GameKey returns the uniqueness key for a game.
func GameKey(g Game) string {
	g, _ = NormalizeGame(g)
	return gameKey(g)
}

func gameKey(g Game) string {
	path := strings.ToLower(g.ImagePath)
	return strings.ToLower(g.Executable) + "\x00" + path
}

// Matches reports whether a running process is covered by this game. A game
// without an image path matches the basename in any directory.
func (g Game) Matches(processName, imagePath string) bool {
	if !strings.EqualFold(strings.TrimSpace(processName), g.Executable) &&
		!strings.EqualFold(baseName(imagePath), g.Executable) {
		return false
	}
	if g.ImagePath == "" {
		return true
	}
	return strings.EqualFold(normalizeProcessPath(imagePath), g.ImagePath)
}

func normalizeProcessPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "/", `\`))
	p = strings.TrimPrefix(p, `\\?\`)
	p = strings.TrimPrefix(p, `\??\`)
	return p
}

func baseName(p string) string {
	if idx := strings.LastIndexByte(p, '\\'); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

func containsControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isCriticalExecutable(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "system", "smss.exe", "csrss.exe", "wininit.exe", "services.exe",
		"lsass.exe", "svchost.exe", "winlogon.exe", "explorer.exe", "dwm.exe",
		"fontdrvhost.exe", "sihost.exe", "ctfmon.exe", "taskhostw.exe",
		"runtimebroker.exe", "playgroundcontrol.exe":
		return true
	default:
		return false
	}
}

// NewID creates a random 128-bit identifier for a game or session-adjacent
// object.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
