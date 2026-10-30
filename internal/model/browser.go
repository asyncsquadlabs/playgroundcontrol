package model

import (
	"errors"
	"time"
)

const (
	// BrowserSourceSeed marks a domain matched by the built-in seed list.
	BrowserSourceSeed = "seed"
	// BrowserSourceAI marks a domain confirmed by DeepSeek.
	BrowserSourceAI = "ai"
	// BrowserSourceManual marks a domain added by the administrator.
	BrowserSourceManual = "manual"

	// BrowserStatusBlocked means the domain is enforced while play is blocked.
	BrowserStatusBlocked = "blocked"
	// BrowserStatusReview means an AI finding needs parent review.
	BrowserStatusReview = "review"
	// BrowserStatusAllowed means the parent allowlisted the domain.
	BrowserStatusAllowed = "allowed"

	// EventBrowser records browser monitoring and enforcement changes.
	EventBrowser = "browser"

	// BrowserMinConfidence is the lowest AI confidence worth recording for
	// review.
	BrowserMinConfidence = 0.70
	// BrowserAutoBlockConfidence is the AI confidence required to block without
	// parent review.
	BrowserAutoBlockConfidence = 0.90
)

// ErrBrowserDomainLimit reports that the blocked-domain cap was reached.
var ErrBrowserDomainLimit = errors.New("the blocked browser domain limit has been reached")

// BrowserSettings is the non-secret view of browser monitoring configuration.
type BrowserSettings struct {
	Enabled          bool      `json:"enabled"`
	AutoBlock        bool      `json:"autoBlock"`
	AIClassification bool      `json:"aiClassification"`
	KeyConfigured    bool      `json:"keyConfigured"`
	LastScanAt       time.Time `json:"lastScanAt"`
	LastError        string    `json:"lastError,omitempty"`
}

// DefaultBrowserSettings returns opt-in defaults. Monitoring is off until a
// parent enables it; automatic blocking is on once monitoring is enabled.
func DefaultBrowserSettings() BrowserSettings {
	return BrowserSettings{AutoBlock: true}
}

// BrowserDomain is one persisted browser domain and its current disposition.
type BrowserDomain struct {
	ID         string    `json:"id"`
	Domain     string    `json:"domain"`
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	Confidence float64   `json:"confidence,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	FirstSeen  time.Time `json:"firstSeen"`
	LastSeen   time.Time `json:"lastSeen"`
	SeenCount  int64     `json:"seenCount"`
	Model      string    `json:"model,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// BrowserDetection is one non-secret observation of a browser domain. It never
// contains a full URL, page title, profile path, or query string.
type BrowserDetection struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Browser    string    `json:"browser"`
	Domain     string    `json:"domain"`
	Source     string    `json:"source"`
	Action     string    `json:"action"`
	Confidence float64   `json:"confidence,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

// BrowserStatus is the enforcement state reported to the dashboard.
type BrowserStatus struct {
	MonitoringEnabled   bool      `json:"monitoringEnabled"`
	Active              bool      `json:"active"`
	URLBlocklistApplied bool      `json:"urlBlocklistApplied"`
	HostsApplied        bool      `json:"hostsApplied"`
	DoHDisabled         bool      `json:"dohDisabled"`
	DriftDetected       bool      `json:"driftDetected"`
	Browsers            []string  `json:"browsers"`
	LastAppliedAt       time.Time `json:"lastAppliedAt,omitempty"`
	LastError           string    `json:"lastError,omitempty"`
}
