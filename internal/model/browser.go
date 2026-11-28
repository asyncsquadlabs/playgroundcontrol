package model

import (
	"errors"
	"strings"
	"time"
)

const (
	// BrowserChrome and the following IDs identify supported browser families.
	BrowserChrome   = "chrome"
	BrowserEdge     = "edge"
	BrowserFirefox  = "firefox"
	BrowserBrave    = "brave"
	BrowserVivaldi  = "vivaldi"
	BrowserOpera    = "opera"
	BrowserChromium = "chromium"

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

	// BrowserHistoryAlertBulkRemoval reports that recent visits or a large
	// portion of a profile's history disappeared.
	BrowserHistoryAlertBulkRemoval = "bulk_removal"
	// BrowserHistoryAlertDatabaseMissing reports that a previously readable
	// history database disappeared while its browser is still installed.
	BrowserHistoryAlertDatabaseMissing = "history_database_missing"
)

// ErrBrowserDomainLimit reports that the blocked-domain cap was reached.
var ErrBrowserDomainLimit = errors.New("the blocked browser domain limit has been reached")

// SupportedBrowsers returns the browser IDs accepted by the service.
func SupportedBrowsers() []string {
	return []string{
		BrowserChrome, BrowserEdge, BrowserFirefox, BrowserBrave,
		BrowserVivaldi, BrowserOpera, BrowserChromium,
	}
}

// NormalizeBrowser validates and lowercases a supported browser ID.
func NormalizeBrowser(raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case BrowserChrome, BrowserEdge, BrowserFirefox, BrowserBrave,
		BrowserVivaldi, BrowserOpera, BrowserChromium:
		return value, true
	default:
		return "", false
	}
}

// BrowserSettings is the non-secret view of browser monitoring configuration.
type BrowserSettings struct {
	Enabled          bool      `json:"enabled"`
	AutoBlock        bool      `json:"autoBlock"`
	AIClassification bool      `json:"aiClassification"`
	ActivityLogging  bool      `json:"activityLogging"`
	KeyConfigured    bool      `json:"keyConfigured"`
	LastScanAt       time.Time `json:"lastScanAt"`
	LastError        string    `json:"lastError,omitempty"`

	// ActivityBackfillAt is internal state. A zero value means the selected
	// accounts still need their initial or post-selection history backfill.
	ActivityBackfillAt time.Time `json:"-"`
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

// BrowserStatus is the enforcement and journal state reported to the
// dashboard.
type BrowserStatus struct {
	MonitoringEnabled       bool      `json:"monitoringEnabled"`
	Active                  bool      `json:"active"`
	URLBlocklistApplied     bool      `json:"urlBlocklistApplied"`
	HostsApplied            bool      `json:"hostsApplied"`
	DoHDisabled             bool      `json:"dohDisabled"`
	DriftDetected           bool      `json:"driftDetected"`
	Browsers                []string  `json:"browsers"`
	PolicyBrowsers          []string  `json:"policyBrowsers"`
	FallbackOnlyBrowsers    []string  `json:"fallbackOnlyBrowsers"`
	RestartRequiredBrowsers []string  `json:"restartRequiredBrowsers"`
	LastAppliedAt           time.Time `json:"lastAppliedAt,omitempty"`
	LastError               string    `json:"lastError,omitempty"`

	ActivityLoggingActive       bool `json:"activityLoggingActive"`
	ActivityBackfillPending     bool `json:"activityBackfillPending"`
	ActivityEvents24h           int  `json:"activityEvents24h"`
	UnacknowledgedHistoryAlerts int  `json:"unacknowledgedHistoryAlerts"`
	MonitoredUsers              int  `json:"monitoredUsers"`
}

// BrowserVisit is one locally observed navigation. Detailed fields are only
// filled when the activity journal is enabled; game detection consumes only
// Browser and Domain.
type BrowserVisit struct {
	Browser      string
	Domain       string
	VisitedAt    time.Time
	URL          string
	Title        string
	SearchEngine string
	SearchQuery  string
	UserSID      string
	UserName     string
	ProfileKey   string
	ProfileLabel string
	SourceKey    string
	Running      bool
}

// BrowserActivity is one retained detailed visit shown to the administrator.
type BrowserActivity struct {
	ID           string     `json:"id"`
	Time         time.Time  `json:"time"`
	UserSID      string     `json:"-"`
	UserName     string     `json:"userName"`
	Browser      string     `json:"browser"`
	ProfileLabel string     `json:"profileLabel,omitempty"`
	URL          string     `json:"url"`
	Title        string     `json:"title,omitempty"`
	Domain       string     `json:"domain"`
	SearchEngine string     `json:"searchEngine,omitempty"`
	SearchQuery  string     `json:"searchQuery,omitempty"`
	RemovedAt    *time.Time `json:"removedAt,omitempty"`
}

// BrowserActivityFilter selects retained journal rows.
type BrowserActivityFilter struct {
	UserName    string
	Browser     string
	Query       string
	From        time.Time
	To          time.Time
	RemovedOnly bool
	Limit       int
	Offset      int
}

// BrowserHistoryAlert is a profile-level history-clearing finding.
type BrowserHistoryAlert struct {
	ID             string     `json:"id"`
	Time           time.Time  `json:"time"`
	UserSID        string     `json:"-"`
	UserName       string     `json:"userName"`
	Browser        string     `json:"browser"`
	ProfileLabel   string     `json:"profileLabel,omitempty"`
	RemovedCount   int        `json:"removedCount"`
	Reason         string     `json:"reason"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
}

// BrowserUser is one local Windows profile that can be selected for journaling.
type BrowserUser struct {
	SID       string   `json:"sid"`
	UserName  string   `json:"userName"`
	Browsers  []string `json:"browsers"`
	Selected  bool     `json:"selected"`
	Available bool     `json:"available"`
}

// BrowserProfileInfo describes one discovered browser profile and the total
// history row count observed during a scan.
type BrowserProfileInfo struct {
	ProfileKey   string
	Browser      string
	UserSID      string
	UserName     string
	ProfileLabel string
	HistoryCount int64
	Readable     bool
	Error        string
}

// BrowserHistoryStat tracks the browser's own history row count for one
// profile so a bulk clear can be detected.
type BrowserHistoryStat struct {
	ProfileKey     string
	Browser        string
	UserSID        string
	UserName       string
	ProfileLabel   string
	VisitCount     int64
	LastReadableAt time.Time
	LastCheckedAt  time.Time
}
