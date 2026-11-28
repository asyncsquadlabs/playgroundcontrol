// Package browserwatch detects game domains from browser history and maintains
// the optional detailed activity journal. Detailed records are only persisted
// when the administrator enables activity logging.
package browserwatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/deepseek"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/domains"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

const (
	DefaultPollInterval           = 5 * time.Second
	DefaultLookback               = 15 * time.Minute
	DefaultScanOverlap            = time.Minute
	DefaultMinRequestInterval     = 30 * time.Second
	DefaultMaxRequestsPerHour     = 60
	DefaultCacheTTL               = 24 * time.Hour
	DefaultBackoffMin             = time.Minute
	DefaultBackoffMax             = 15 * time.Minute
	DefaultMaxCacheEntries        = 2_000
	DefaultActivityAuditInterval  = 5 * time.Minute
	DefaultActivityPruneInterval  = 6 * time.Hour
	DefaultActivityBackfillWindow = 90 * 24 * time.Hour

	historyRecentWindow          = 7 * 24 * time.Hour
	historyBulkAlertMinVisits    = 5
	historyAlertRatioMinVisits   = 2
	historyCountDropMinPrevious  = 50
	historyCountDropZeroPrevious = 20
	maxBatchSize                 = 20
	maxDomainsPerTick            = 200
	statusPersistInterval        = 5 * time.Minute
)

// Visit is one locally observed browser navigation.
type Visit = model.BrowserVisit

// HistorySource provides domain-level game visits and detailed journal scans.
type HistorySource interface {
	RecentGameVisits(context.Context, time.Time) ([]model.BrowserVisit, error)
	RecentJournalScan(context.Context, time.Time, map[string]struct{}) ([]model.BrowserVisit, []model.BrowserProfileInfo, error)
	BrowserUsers(context.Context) ([]model.BrowserUser, error)
}

// State supplies the current permission and browser-monitoring settings.
type State interface {
	PlayAllowed() bool
	BrowserSettings() model.BrowserSettings
}

// Settings supplies the shared DeepSeek configuration.
type Settings interface {
	AICredentials() (model.AISettings, string, bool)
}

// Classifier is the DeepSeek domain-classification surface.
type Classifier interface {
	ClassifyDomains(context.Context, string, string, []deepseek.DomainObservation) ([]deepseek.DomainVerdict, error)
}

// Recorder persists domains, detections, journal rows, history alerts, and
// provider health.
type Recorder interface {
	BrowserDomainByDomain(string) (model.BrowserDomain, bool, error)
	RecordBrowserSeed(string, string, time.Time) (model.BrowserDomain, bool, error)
	RecordBrowserAIVerdict(string, float64, string, string, bool, time.Time) (model.BrowserDomain, string, bool, error)
	RecordBrowserDetection(model.BrowserDetection) error
	SetBrowserScanStatus(time.Time, string) error

	SelectedBrowserUserSIDs() (map[string]struct{}, error)
	RecordBrowserActivity(time.Time, []model.BrowserVisit) (int, error)
	MarkMissingBrowserActivity(string, time.Time, time.Time) (int, error)
	CountBrowserActivityForProfile(string, time.Time) (int, error)
	BrowserHistoryStat(string) (model.BrowserHistoryStat, bool, error)
	SaveBrowserHistoryStat(model.BrowserHistoryStat) error
	RecordBrowserHistoryAlert(model.BrowserHistoryAlert) (model.BrowserHistoryAlert, error)
	SetBrowserActivityBackfillDone(time.Time) error
	PruneBrowserActivity(time.Time) (int64, error)
}

// Controller requests an enforcement rescan after a domain disposition
// changes.
type Controller interface {
	Wake()
}

// Auditor stores domain and provider transition events.
type Auditor interface {
	AddEvent(model.AuditEvent) error
}

// Options configures a Watcher. Zero values use production defaults.
type Options struct {
	PollInterval           time.Duration
	Lookback               time.Duration
	ScanOverlap            time.Duration
	MinRequestInterval     time.Duration
	MaxRequestsPerHour     int
	CacheTTL               time.Duration
	BackoffMin             time.Duration
	BackoffMax             time.Duration
	MaxCacheEntries        int
	ActivityAuditInterval  time.Duration
	ActivityPruneInterval  time.Duration
	ActivityBackfillWindow time.Duration
	Now                    func() time.Time
}

type cacheEntry struct {
	verdict deepseek.DomainVerdict
	expires time.Time
}

// Watcher is safe for concurrent use.
type Watcher struct {
	source     HistorySource
	state      State
	settings   Settings
	classifier Classifier
	recorder   Recorder
	controller Controller
	auditor    Auditor

	interval    time.Duration
	lookback    time.Duration
	overlap     time.Duration
	minRequest  time.Duration
	maxRequests int
	cacheTTL    time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration
	maxCache    int
	auditEvery  time.Duration
	pruneEvery  time.Duration
	backfillAge time.Duration
	now         func() time.Time

	baseCtx context.Context
	wake    chan struct{}

	tickMu  sync.Mutex
	pruneMu sync.Mutex
	mu      sync.Mutex

	cache        map[string]cacheEntry
	seenVisits   map[string]time.Time
	requestTimes []time.Time
	lastRequest  time.Time
	nextAttempt  time.Time
	backoff      time.Duration

	lastScan      time.Time
	lastAudit     time.Time
	lastPrune     time.Time
	missing       map[string]bool
	profileErrors map[string]string
	lastError     string
	lastStatusAt  time.Time

	backfillMu   sync.Mutex
	backfillBusy bool
	backfillNext time.Time
}

// New builds a browser watcher.
func New(source HistorySource, state State, settings Settings, classifier Classifier, recorder Recorder, controller Controller, auditor Auditor, options Options) *Watcher {
	if options.PollInterval <= 0 {
		options.PollInterval = DefaultPollInterval
	}
	if options.Lookback <= 0 {
		options.Lookback = DefaultLookback
	}
	if options.ScanOverlap <= 0 {
		options.ScanOverlap = DefaultScanOverlap
	}
	if options.MinRequestInterval <= 0 {
		options.MinRequestInterval = DefaultMinRequestInterval
	}
	if options.MaxRequestsPerHour <= 0 {
		options.MaxRequestsPerHour = DefaultMaxRequestsPerHour
	}
	if options.CacheTTL <= 0 {
		options.CacheTTL = DefaultCacheTTL
	}
	if options.BackoffMin <= 0 {
		options.BackoffMin = DefaultBackoffMin
	}
	if options.BackoffMax < options.BackoffMin {
		options.BackoffMax = DefaultBackoffMax
	}
	if options.MaxCacheEntries <= 0 {
		options.MaxCacheEntries = DefaultMaxCacheEntries
	}
	if options.ActivityAuditInterval <= 0 {
		options.ActivityAuditInterval = DefaultActivityAuditInterval
	}
	if options.ActivityPruneInterval <= 0 {
		options.ActivityPruneInterval = DefaultActivityPruneInterval
	}
	if options.ActivityBackfillWindow <= 0 {
		options.ActivityBackfillWindow = DefaultActivityBackfillWindow
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Watcher{
		source:        source,
		state:         state,
		settings:      settings,
		classifier:    classifier,
		recorder:      recorder,
		controller:    controller,
		auditor:       auditor,
		interval:      options.PollInterval,
		lookback:      options.Lookback,
		overlap:       options.ScanOverlap,
		minRequest:    options.MinRequestInterval,
		maxRequests:   options.MaxRequestsPerHour,
		cacheTTL:      options.CacheTTL,
		backoffMin:    options.BackoffMin,
		backoffMax:    options.BackoffMax,
		maxCache:      options.MaxCacheEntries,
		auditEvery:    options.ActivityAuditInterval,
		pruneEvery:    options.ActivityPruneInterval,
		backfillAge:   options.ActivityBackfillWindow,
		now:           options.Now,
		wake:          make(chan struct{}, 1),
		cache:         make(map[string]cacheEntry),
		seenVisits:    make(map[string]time.Time),
		missing:       make(map[string]bool),
		profileErrors: make(map[string]string),
	}
}

// Wake requests an immediate scan.
func (w *Watcher) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run scans until the context is canceled.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.baseCtx = ctx
	w.mu.Unlock()
	w.Tick(w.now())
	w.pruneIfDue(w.now())
	ticker := time.NewTicker(w.interval)
	pruneTicker := time.NewTicker(w.pruneEvery)
	defer ticker.Stop()
	defer pruneTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(w.now())
		case <-pruneTicker.C:
			w.pruneIfDue(w.now())
		case <-w.wake:
			w.Tick(w.now())
		}
	}
}

// Tick performs one scan. Tests call it directly with a fixed clock.
func (w *Watcher) Tick(now time.Time) {
	if w == nil {
		return
	}
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	if w.state == nil || w.source == nil || w.recorder == nil {
		return
	}
	settings := w.state.BrowserSettings()
	if !settings.Enabled {
		w.resetBackoff()
		return
	}
	playAllowed := w.state.PlayAllowed()
	journalEnabled := settings.ActivityLogging
	gameEnabled := !playAllowed
	if !journalEnabled && !gameEnabled {
		w.resetBackoff()
		return
	}

	nowUTC := now.UTC()
	selected := map[string]struct{}{}
	if journalEnabled {
		var err error
		selected, err = w.recorder.SelectedBrowserUserSIDs()
		if err != nil {
			w.reportError(nowUTC, "Unable to read monitored browser accounts: "+err.Error(), false)
			selected = map[string]struct{}{}
		}
	}
	if playAllowed && (!journalEnabled || len(selected) == 0) {
		w.resetBackoff()
		return
	}
	if journalEnabled && len(selected) > 0 && settings.ActivityBackfillAt.IsZero() {
		w.maybeStartBackfill(nowUTC, selected)
	}

	since := w.scanSince(nowUTC, journalEnabled, settings)
	ctx := w.baseContext()
	historyCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	var (
		visits []model.BrowserVisit
		infos  []model.BrowserProfileInfo
		err    error
	)
	if journalEnabled && len(selected) > 0 {
		visits, infos, err = w.source.RecentJournalScan(historyCtx, since, selected)
	} else if gameEnabled {
		visits, err = w.source.RecentGameVisits(historyCtx, since)
	}
	cancel()
	if err != nil {
		w.reportError(nowUTC, "Unable to read browser history: "+err.Error(), true)
		return
	}
	w.lastScan = nowUTC

	if journalEnabled && len(selected) > 0 {
		journalVisits := filterSelectedVisits(visits, selected)
		if len(journalVisits) > 0 {
			if _, err := w.recorder.RecordBrowserActivity(nowUTC, journalVisits); err != nil {
				w.reportError(nowUTC, "Unable to save browser activity: "+err.Error(), false)
			}
		}
		w.reportProfileErrors(infos, selected, nowUTC)
	}

	if gameEnabled {
		gameCutoff := nowUTC.Add(-w.lookback)
		gameVisits := make([]model.BrowserVisit, 0, len(visits))
		for _, visit := range visits {
			if visit.Running && !visit.VisitedAt.Before(gameCutoff) {
				gameVisits = append(gameVisits, visit)
			}
		}
		w.detectDomains(ctx, gameVisits, nowUTC)
	}

	w.pruneIfDue(nowUTC)
	w.auditIfDue(ctx, selected, nowUTC)
	if !w.hasError() {
		w.persistStatus(nowUTC, "")
	}
}

func (w *Watcher) scanSince(now time.Time, journalEnabled bool, settings model.BrowserSettings) time.Time {
	last := settings.LastScanAt.UTC()
	var since time.Time
	if !last.IsZero() {
		since = last.Add(-w.overlap)
	}
	if journalEnabled {
		if settings.ActivityBackfillAt.IsZero() {
			return now.Add(-w.lookback)
		}
		if since.IsZero() || since.Before(now.Add(-w.backfillAge)) {
			since = now.Add(-w.backfillAge)
		}
		return since
	}
	if since.IsZero() || now.Sub(last) > w.lookback {
		return now.Add(-w.lookback)
	}
	return since
}

func (w *Watcher) maybeStartBackfill(now time.Time, selected map[string]struct{}) {
	w.backfillMu.Lock()
	if w.backfillBusy || (!w.backfillNext.IsZero() && now.Before(w.backfillNext)) {
		w.backfillMu.Unlock()
		return
	}
	w.backfillBusy = true
	w.backfillMu.Unlock()
	go w.runBackfill(now, selected)
}

func (w *Watcher) runBackfill(now time.Time, selected map[string]struct{}) {
	defer func() {
		w.backfillMu.Lock()
		w.backfillBusy = false
		w.backfillMu.Unlock()
	}()
	ctx := w.baseContext()
	since := now.Add(-w.backfillAge)
	backfillCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	visits, infos, err := w.source.RecentJournalScan(backfillCtx, since, selected)
	cancel()
	if err != nil {
		w.backfillMu.Lock()
		w.backfillNext = now.Add(5 * time.Minute)
		w.backfillMu.Unlock()
		if ctx.Err() == nil {
			w.reportError(now, "Unable to import browser activity history: "+err.Error(), false)
		}
		return
	}
	journalVisits := filterSelectedVisits(visits, selected)
	if len(journalVisits) > 0 {
		if _, err := w.recorder.RecordBrowserActivity(now, journalVisits); err != nil {
			w.backfillMu.Lock()
			w.backfillNext = now.Add(5 * time.Minute)
			w.backfillMu.Unlock()
			w.reportError(now, "Unable to save imported browser activity: "+err.Error(), false)
			return
		}
	}
	w.seedHistoryStats(infos, selected, now)
	selectedInfos := 0
	readable := 0
	for _, info := range infos {
		if _, ok := selected[strings.ToUpper(info.UserSID)]; !ok {
			continue
		}
		selectedInfos++
		if info.Readable {
			readable++
		} else if info.Error != "" {
			w.audit(model.AuditEvent{
				Time: now, Type: model.EventBrowser,
				Message: fmt.Sprintf("Browser history for %s (%s) could not be imported: %s", info.UserName, info.Browser, info.Error),
				Result:  "browser-activity-error",
			})
		}
	}
	if selectedInfos > 0 && readable == 0 {
		w.backfillMu.Lock()
		w.backfillNext = now.Add(5 * time.Minute)
		w.backfillMu.Unlock()
		w.reportError(now, "Browser activity history could not be imported for any selected account.", false)
		return
	}
	doneAt := w.now().UTC()
	if err := w.recorder.SetBrowserActivityBackfillDone(doneAt); err != nil {
		w.reportError(now, "Unable to finish browser activity import: "+err.Error(), false)
		return
	}
	_ = w.recorder.SetBrowserScanStatus(doneAt, "")
	w.audit(model.AuditEvent{
		Time: now, Type: model.EventBrowser,
		Message: "Browser activity history import completed.",
		Result:  "browser-activity-imported",
	})
}

func (w *Watcher) seedHistoryStats(infos []model.BrowserProfileInfo, selected map[string]struct{}, now time.Time) {
	for _, info := range infos {
		if _, ok := selected[strings.ToUpper(info.UserSID)]; !ok || !info.Readable {
			continue
		}
		stat := model.BrowserHistoryStat{
			ProfileKey: info.ProfileKey, Browser: info.Browser, UserSID: info.UserSID,
			UserName: info.UserName, ProfileLabel: info.ProfileLabel,
			VisitCount: info.HistoryCount, LastReadableAt: now, LastCheckedAt: now,
		}
		_ = w.recorder.SaveBrowserHistoryStat(stat)
	}
}

func (w *Watcher) auditIfDue(ctx context.Context, selected map[string]struct{}, now time.Time) {
	settings := w.state.BrowserSettings()
	if len(selected) == 0 || !settings.ActivityLogging || settings.ActivityBackfillAt.IsZero() {
		return
	}
	if !w.lastAudit.IsZero() && now.Sub(w.lastAudit) < w.auditEvery {
		return
	}
	w.lastAudit = now
	auditCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	visits, infos, err := w.source.RecentJournalScan(auditCtx, now.Add(-historyRecentWindow), selected)
	if err != nil {
		w.reportError(now, "Unable to audit browser history removals: "+err.Error(), false)
		return
	}
	journalVisits := filterSelectedVisits(visits, selected)
	if len(journalVisits) > 0 {
		if _, err := w.recorder.RecordBrowserActivity(now, journalVisits); err != nil {
			w.reportError(now, "Unable to verify browser activity: "+err.Error(), false)
			return
		}
	}
	w.auditProfiles(infos, selected, now)
}

func (w *Watcher) auditProfiles(infos []model.BrowserProfileInfo, selected map[string]struct{}, now time.Time) {
	for _, info := range infos {
		if _, ok := selected[strings.ToUpper(info.UserSID)]; !ok {
			continue
		}
		old, found, err := w.recorder.BrowserHistoryStat(info.ProfileKey)
		if err != nil {
			w.reportError(now, "Unable to read browser history baseline: "+err.Error(), false)
			continue
		}
		if !info.Readable {
			if found && !old.LastReadableAt.IsZero() && !w.isMissing(info.ProfileKey) {
				w.setMissing(info.ProfileKey, true)
				w.recordHistoryAlert(info, 0, model.BrowserHistoryAlertDatabaseMissing, now)
			}
			stat := old
			stat.ProfileKey = info.ProfileKey
			stat.Browser = info.Browser
			stat.UserSID = info.UserSID
			stat.UserName = info.UserName
			stat.ProfileLabel = info.ProfileLabel
			stat.LastCheckedAt = now
			_ = w.recorder.SaveBrowserHistoryStat(stat)
			continue
		}
		w.setMissing(info.ProfileKey, false)
		removedRecent, err := w.recorder.MarkMissingBrowserActivity(info.ProfileKey, now, now.Add(-historyRecentWindow))
		if err != nil {
			w.reportError(now, "Unable to mark removed browser activity: "+err.Error(), false)
			continue
		}
		recentCount, err := w.recorder.CountBrowserActivityForProfile(info.ProfileKey, now.Add(-historyRecentWindow))
		if err != nil {
			recentCount = 0
		}
		removedTotal := int64(0)
		if found && old.VisitCount > info.HistoryCount {
			removedTotal = old.VisitCount - info.HistoryCount
		}
		alert := removedRecent >= historyBulkAlertMinVisits ||
			(removedRecent >= historyAlertRatioMinVisits && recentCount > 0 && removedRecent*2 >= recentCount)
		if found {
			if old.VisitCount >= historyCountDropZeroPrevious && info.HistoryCount == 0 {
				alert = true
			} else if old.VisitCount >= historyCountDropMinPrevious && removedTotal*2 >= old.VisitCount {
				alert = true
			}
		}
		stat := model.BrowserHistoryStat{
			ProfileKey: info.ProfileKey, Browser: info.Browser, UserSID: info.UserSID,
			UserName: info.UserName, ProfileLabel: info.ProfileLabel,
			VisitCount: info.HistoryCount, LastReadableAt: now, LastCheckedAt: now,
		}
		_ = w.recorder.SaveBrowserHistoryStat(stat)
		if alert {
			count := removedRecent
			if int64(count) < removedTotal {
				count = int(removedTotal)
			}
			w.recordHistoryAlert(info, count, model.BrowserHistoryAlertBulkRemoval, now)
		}
	}
}

func (w *Watcher) recordHistoryAlert(info model.BrowserProfileInfo, count int, reason string, now time.Time) {
	alert, err := w.recorder.RecordBrowserHistoryAlert(model.BrowserHistoryAlert{
		Time: now, UserSID: info.UserSID, UserName: info.UserName, Browser: info.Browser,
		ProfileLabel: info.ProfileLabel, RemovedCount: count, Reason: reason,
	})
	if err != nil {
		w.reportError(now, "Unable to save browser history alert: "+err.Error(), false)
		return
	}
	message := fmt.Sprintf("Browser history was cleared or entries were deleted for %s (%s): %d visits. The activity journal still contains them.",
		info.UserName, info.Browser, count)
	result := "browser-history-cleared"
	if reason == model.BrowserHistoryAlertDatabaseMissing {
		message = fmt.Sprintf("The %s history database for %s was no longer readable. Previously recorded activity remains in the journal.",
			info.Browser, info.UserName)
		result = "browser-history-missing"
	}
	w.audit(model.AuditEvent{Time: now, Type: model.EventBrowser, Message: message, Result: result, Domain: ""})
	_ = alert
}

func (w *Watcher) isMissing(profileKey string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.missing[profileKey]
}

func (w *Watcher) setMissing(profileKey string, value bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if value {
		w.missing[profileKey] = true
	} else {
		delete(w.missing, profileKey)
	}
}

func (w *Watcher) reportProfileErrors(infos []model.BrowserProfileInfo, selected map[string]struct{}, now time.Time) {
	for _, info := range infos {
		if _, ok := selected[strings.ToUpper(info.UserSID)]; !ok {
			continue
		}
		key := info.ProfileKey
		w.mu.Lock()
		previous := w.profileErrors[key]
		if info.Error == "" {
			delete(w.profileErrors, key)
		} else if info.Error != previous {
			w.profileErrors[key] = info.Error
		}
		w.mu.Unlock()
		if info.Error == "" || info.Error == previous {
			continue
		}
		w.audit(model.AuditEvent{
			Time: now, Type: model.EventBrowser,
			Message: fmt.Sprintf("Browser activity could not be read for %s (%s): %s", info.UserName, info.Browser, info.Error),
			Result:  "browser-activity-error",
		})
	}
}

func (w *Watcher) pruneIfDue(now time.Time) {
	if w.recorder == nil {
		return
	}
	w.pruneMu.Lock()
	defer w.pruneMu.Unlock()
	if !w.lastPrune.IsZero() && now.Sub(w.lastPrune) < w.pruneEvery {
		return
	}
	deleted, err := w.recorder.PruneBrowserActivity(now)
	w.lastPrune = now
	if err != nil {
		w.reportError(now, "Unable to prune browser activity: "+err.Error(), false)
		return
	}
	if deleted > 0 {
		w.audit(model.AuditEvent{
			Time: now, Type: model.EventBrowser,
			Message: fmt.Sprintf("Pruned %d old browser activity records.", deleted),
			Result:  "browser-activity-pruned",
		})
	}
}

func (w *Watcher) detectDomains(ctx context.Context, visits []model.BrowserVisit, now time.Time) {
	settings := w.state.BrowserSettings()
	aiSettings := model.AISettings{}
	configured := false
	if w.settings != nil {
		aiSettings, _, configured = w.settings.AICredentials()
	}
	aiModel := strings.ToLower(strings.TrimSpace(aiSettings.Model))
	canAI := settings.AIClassification && configured && strings.TrimSpace(aiSettings.Model) != ""

	seen := make(map[string]int, len(visits))
	unique := make([]model.BrowserVisit, 0, len(visits))
	for _, visit := range visits {
		browser, ok := model.NormalizeBrowser(visit.Browser)
		if !ok {
			continue
		}
		domain, err := domains.Normalize(visit.Domain)
		if err != nil {
			continue
		}
		key := browser + "\x00" + domain
		if index, duplicate := seen[key]; duplicate {
			if visit.VisitedAt.After(unique[index].VisitedAt) {
				unique[index].VisitedAt = visit.VisitedAt
			}
			continue
		}
		seen[key] = len(unique)
		unique = append(unique, model.BrowserVisit{Browser: browser, Domain: domain, VisitedAt: visit.VisitedAt})
	}

	var unknown []model.BrowserVisit
	unknownDomains := make(map[string]struct{})
	handled := 0
	for _, visit := range unique {
		if !w.shouldProcessVisit(visit) {
			continue
		}
		if handled >= maxDomainsPerTick {
			break
		}
		handled++
		existing, found, err := w.recorder.BrowserDomainByDomain(visit.Domain)
		if err != nil {
			w.reportError(now, fmt.Sprintf("Unable to read browser domain %s: %v", visit.Domain, err), false)
			continue
		}
		if found && existing.Status == model.BrowserStatusAllowed {
			w.markVisit(visit)
			continue
		}
		if domains.IsSeed(visit.Domain) {
			_, changed, err := w.recorder.RecordBrowserSeed(visit.Domain, "Matched the built-in game seed list.", now)
			if err != nil {
				w.handleRecordError(now, visit.Domain, err)
				continue
			}
			w.recordDetection(model.BrowserDetection{
				Time: now, Browser: visit.Browser, Domain: visit.Domain,
				Source: model.BrowserSourceSeed, Action: "seed-blocked", Confidence: 1,
				Reason: "Matched the built-in game seed list.",
			})
			if changed {
				w.audit(model.AuditEvent{
					Time: now, Type: model.EventBrowser, Domain: visit.Domain,
					Message: "Blocked browser game domain from the seed list: " + visit.Domain,
					Result:  "browser-seed-blocked",
				})
				w.wakeController()
			}
			w.markVisit(visit)
			continue
		}
		if found && existing.Status == model.BrowserStatusBlocked {
			w.recordDetection(model.BrowserDetection{
				Time: now, Browser: visit.Browser, Domain: visit.Domain,
				Source: existing.Source, Action: "already-blocked",
				Confidence: existing.Confidence, Reason: existing.Reason,
			})
			w.markVisit(visit)
			continue
		}
		if found && existing.Status == model.BrowserStatusReview {
			w.recordDetection(model.BrowserDetection{
				Time: now, Browser: visit.Browser, Domain: visit.Domain,
				Source: existing.Source, Action: "review",
				Confidence: existing.Confidence, Reason: existing.Reason,
			})
			w.markVisit(visit)
			continue
		}
		if !canAI {
			w.markVisit(visit)
			continue
		}
		if _, duplicate := unknownDomains[visit.Domain]; duplicate {
			continue
		}
		unknownDomains[visit.Domain] = struct{}{}
		cacheKey := visit.Domain + "\x00" + aiModel
		w.mu.Lock()
		entry, cached := w.cache[cacheKey]
		if cached && !now.Before(entry.expires) {
			delete(w.cache, cacheKey)
			cached = false
		}
		w.mu.Unlock()
		if cached {
			w.applyVerdict(visit, entry.verdict, aiSettings.Model, now)
			w.markVisit(visit)
			continue
		}
		unknown = append(unknown, visit)
	}

	if len(unknown) > 0 {
		w.classifyUnknown(ctx, unknown, aiSettings.Model, now)
	}
}

func (w *Watcher) classifyUnknown(ctx context.Context, visits []model.BrowserVisit, modelName string, now time.Time) {
	if w.classifier == nil || w.settings == nil {
		return
	}
	_, apiKey, configured := w.settings.AICredentials()
	if !configured || strings.TrimSpace(apiKey) == "" {
		return
	}

	w.mu.Lock()
	w.pruneRequestWindow(now)
	w.pruneCache(now)
	blocked := w.nextAttempt.After(now) ||
		(!w.lastRequest.IsZero() && now.Sub(w.lastRequest) < w.minRequest) ||
		len(w.requestTimes) >= w.maxRequests
	if !blocked {
		w.lastRequest = now
		w.requestTimes = append(w.requestTimes, now)
	}
	w.mu.Unlock()
	if blocked {
		return
	}

	batch := visits
	if len(batch) > maxBatchSize {
		batch = batch[:maxBatchSize]
	}
	observations := make([]deepseek.DomainObservation, len(batch))
	for i, visit := range batch {
		observations[i] = deepseek.DomainObservation{Domain: visit.Domain}
	}
	callCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	verdicts, err := w.classifier.ClassifyDomains(callCtx, apiKey, modelName, observations)
	if err != nil {
		if ctx.Err() == nil {
			w.reportError(now, "DeepSeek domain classification failed: "+err.Error(), true)
		}
		return
	}
	w.reportSuccess(now)
	for _, visit := range batch {
		w.markVisit(visit)
	}

	byDomain := make(map[string]deepseek.DomainVerdict, len(verdicts))
	for _, verdict := range verdicts {
		domain := strings.ToLower(strings.TrimSpace(verdict.Domain))
		if domain == "" {
			continue
		}
		byDomain[domain] = verdict
		cacheKey := domain + "\x00" + strings.ToLower(strings.TrimSpace(modelName))
		w.mu.Lock()
		w.cache[cacheKey] = cacheEntry{verdict: verdict, expires: now.Add(w.cacheTTL)}
		w.pruneCache(now)
		w.mu.Unlock()
	}
	for _, visit := range batch {
		verdict, ok := byDomain[visit.Domain]
		if !ok {
			continue
		}
		w.applyVerdict(visit, verdict, modelName, now)
	}
}

func (w *Watcher) applyVerdict(visit model.BrowserVisit, verdict deepseek.DomainVerdict, modelName string, now time.Time) {
	if !verdict.IsGame || verdict.Confidence < model.BrowserMinConfidence {
		return
	}
	if w.state == nil || w.state.PlayAllowed() {
		return
	}
	current := w.state.BrowserSettings()
	if !current.Enabled || !current.AIClassification {
		return
	}
	stored, action, changed, err := w.recorder.RecordBrowserAIVerdict(
		visit.Domain, verdict.Confidence, verdict.Reason, modelName, current.AutoBlock, now,
	)
	if err != nil {
		w.handleRecordError(now, visit.Domain, err)
		return
	}
	if stored.Domain == "" {
		return
	}
	w.recordDetection(model.BrowserDetection{
		Time: now, Browser: visit.Browser, Domain: visit.Domain,
		Source: model.BrowserSourceAI, Action: action,
		Confidence: verdict.Confidence, Reason: verdict.Reason,
	})
	switch action {
	case "ai-auto-blocked":
		if changed {
			w.audit(model.AuditEvent{
				Time: now, Type: model.EventBrowser, Domain: visit.Domain,
				Message: fmt.Sprintf("AI auto-blocked browser game domain: %s (confidence %.2f) - %s",
					visit.Domain, verdict.Confidence, boundText(verdict.Reason, 200)),
				Result: "browser-ai-auto-blocked",
			})
			w.wakeController()
		}
	case "review":
		w.audit(model.AuditEvent{
			Time: now, Type: model.EventBrowser, Domain: visit.Domain,
			Message: fmt.Sprintf("Browser game domain needs review: %s (confidence %.2f)", visit.Domain, verdict.Confidence),
			Result:  "browser-review",
		})
	}
}

func (w *Watcher) handleRecordError(now time.Time, domain string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, model.ErrBrowserDomainLimit) {
		w.audit(model.AuditEvent{
			Time: now, Type: model.EventBrowser, Domain: domain,
			Message: "Blocked browser domain limit reached; detection was not enforced: " + domain,
			Result:  "browser-domain-limit",
		})
		return
	}
	w.reportError(now, fmt.Sprintf("Unable to save browser domain %s: %v", domain, err), false)
}

func (w *Watcher) recordDetection(detection model.BrowserDetection) {
	if w.recorder == nil {
		return
	}
	_ = w.recorder.RecordBrowserDetection(detection)
}

func (w *Watcher) shouldProcessVisit(visit model.BrowserVisit) bool {
	key := visit.Browser + "\x00" + visit.Domain
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.seenVisits[key]
	return !ok || visit.VisitedAt.After(last)
}

func (w *Watcher) markVisit(visit model.BrowserVisit) {
	key := visit.Browser + "\x00" + visit.Domain
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.seenVisits[key]; !ok || visit.VisitedAt.After(last) {
		w.seenVisits[key] = visit.VisitedAt
	}
	if len(w.seenVisits) > 20_000 {
		w.seenVisits = make(map[string]time.Time)
	}
}

func (w *Watcher) hasError() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastError != ""
}

func (w *Watcher) persistStatus(now time.Time, message string) {
	w.mu.Lock()
	shouldPersist := w.lastStatusAt.IsZero() || now.Sub(w.lastStatusAt) >= statusPersistInterval
	if shouldPersist {
		w.lastStatusAt = now
	}
	w.mu.Unlock()
	if shouldPersist && w.recorder != nil {
		_ = w.recorder.SetBrowserScanStatus(now, message)
	}
}

func (w *Watcher) reportSuccess(now time.Time) {
	w.mu.Lock()
	hadError := w.lastError != ""
	w.lastError = ""
	w.backoff = 0
	w.nextAttempt = time.Time{}
	shouldPersist := hadError || w.lastStatusAt.IsZero() || now.Sub(w.lastStatusAt) >= statusPersistInterval
	if shouldPersist {
		w.lastStatusAt = now
	}
	w.mu.Unlock()
	if hadError {
		w.audit(model.AuditEvent{
			Time: now, Type: model.EventBrowser,
			Message: "Browser domain monitoring recovered.",
			Result:  "browser-recovered",
		})
	}
	if shouldPersist && w.recorder != nil {
		_ = w.recorder.SetBrowserScanStatus(now, "")
	}
}

func (w *Watcher) reportError(now time.Time, message string, backoff bool) {
	message = boundText(message, 300)
	w.mu.Lock()
	changed := message != w.lastError
	w.lastError = message
	if backoff {
		if w.backoff == 0 {
			w.backoff = w.backoffMin
		} else {
			w.backoff *= 2
			if w.backoff > w.backoffMax {
				w.backoff = w.backoffMax
			}
		}
		w.nextAttempt = now.Add(w.backoff)
	}
	shouldPersist := changed || w.lastStatusAt.IsZero() || now.Sub(w.lastStatusAt) >= statusPersistInterval
	if shouldPersist {
		w.lastStatusAt = now
	}
	w.mu.Unlock()
	if changed {
		w.audit(model.AuditEvent{
			Time: now, Type: model.EventBrowser,
			Message: message,
			Result:  "browser-error",
		})
	}
	if shouldPersist && w.recorder != nil {
		_ = w.recorder.SetBrowserScanStatus(time.Time{}, message)
	}
}

func (w *Watcher) resetBackoff() {
	w.mu.Lock()
	w.backoff = 0
	w.nextAttempt = time.Time{}
	w.mu.Unlock()
}

func (w *Watcher) pruneRequestWindow(now time.Time) {
	cutoff := now.Add(-time.Hour)
	kept := w.requestTimes[:0]
	for _, request := range w.requestTimes {
		if request.After(cutoff) {
			kept = append(kept, request)
		}
	}
	w.requestTimes = kept
}

func (w *Watcher) pruneCache(now time.Time) {
	for key, entry := range w.cache {
		if now.After(entry.expires) {
			delete(w.cache, key)
		}
	}
	if len(w.cache) <= w.maxCache {
		return
	}
	keys := make([]string, 0, len(w.cache))
	for key := range w.cache {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return w.cache[keys[i]].expires.After(w.cache[keys[j]].expires)
	})
	for _, key := range keys[w.maxCache:] {
		delete(w.cache, key)
	}
}

func (w *Watcher) wakeController() {
	if w.controller != nil {
		w.controller.Wake()
	}
}

func (w *Watcher) audit(event model.AuditEvent) {
	if w.auditor == nil {
		return
	}
	_ = w.auditor.AddEvent(event)
}

func (w *Watcher) baseContext() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.baseCtx == nil {
		return context.Background()
	}
	return w.baseCtx
}

func filterSelectedVisits(visits []model.BrowserVisit, selected map[string]struct{}) []model.BrowserVisit {
	if len(selected) == 0 {
		return nil
	}
	out := make([]model.BrowserVisit, 0, len(visits))
	for _, visit := range visits {
		if _, ok := selected[strings.ToUpper(strings.TrimSpace(visit.UserSID))]; ok {
			out = append(out, visit)
		}
	}
	return out
}

func boundText(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) > max {
		value = value[:max]
	}
	return value
}
