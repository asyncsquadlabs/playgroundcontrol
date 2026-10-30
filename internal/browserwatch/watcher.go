// Package browserwatch detects game domains from Chrome and Edge history while
// play is blocked. It uses a deterministic seed list and optionally asks
// DeepSeek about unknown registrable domains.
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
	DefaultPollInterval       = 15 * time.Second
	DefaultLookback           = 15 * time.Minute
	DefaultScanOverlap        = time.Minute
	DefaultMinRequestInterval = 30 * time.Second
	DefaultMaxRequestsPerHour = 60
	DefaultCacheTTL           = 24 * time.Hour
	DefaultBackoffMin         = time.Minute
	DefaultBackoffMax         = 15 * time.Minute
	DefaultMaxCacheEntries    = 2_000
	maxBatchSize              = 20
	maxDomainsPerTick         = 200
	statusPersistInterval     = 5 * time.Minute
)

// Visit is one locally observed browser navigation. It contains no URL, path,
// query string, or page title.
type Visit struct {
	Browser   string
	Domain    string
	VisitedAt time.Time
}

// HistorySource returns unique normalized domains visited since the given time.
type HistorySource interface {
	RecentVisits(context.Context, time.Time) ([]Visit, error)
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

// Recorder persists domains, detections, and provider health.
type Recorder interface {
	BrowserDomainByDomain(string) (model.BrowserDomain, bool, error)
	RecordBrowserSeed(string, string, time.Time) (model.BrowserDomain, bool, error)
	RecordBrowserAIVerdict(string, float64, string, string, bool, time.Time) (model.BrowserDomain, string, bool, error)
	RecordBrowserDetection(model.BrowserDetection) error
	SetBrowserScanStatus(time.Time, string) error
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
	PollInterval       time.Duration
	Lookback           time.Duration
	ScanOverlap        time.Duration
	MinRequestInterval time.Duration
	MaxRequestsPerHour int
	CacheTTL           time.Duration
	BackoffMin         time.Duration
	BackoffMax         time.Duration
	MaxCacheEntries    int
	Now                func() time.Time
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
	now         func() time.Time

	baseCtx context.Context
	wake    chan struct{}

	tickMu sync.Mutex
	mu     sync.Mutex

	cache        map[string]cacheEntry
	seenVisits   map[string]time.Time
	requestTimes []time.Time
	lastRequest  time.Time
	nextAttempt  time.Time
	backoff      time.Duration

	lastScan     time.Time
	lastError    string
	lastStatusAt time.Time
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
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Watcher{
		source:      source,
		state:       state,
		settings:    settings,
		classifier:  classifier,
		recorder:    recorder,
		controller:  controller,
		auditor:     auditor,
		interval:    options.PollInterval,
		lookback:    options.Lookback,
		overlap:     options.ScanOverlap,
		minRequest:  options.MinRequestInterval,
		maxRequests: options.MaxRequestsPerHour,
		cacheTTL:    options.CacheTTL,
		backoffMin:  options.BackoffMin,
		backoffMax:  options.BackoffMax,
		maxCache:    options.MaxCacheEntries,
		now:         options.Now,
		wake:        make(chan struct{}, 1),
		cache:       make(map[string]cacheEntry),
		seenVisits:  make(map[string]time.Time),
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
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(w.now())
		case <-w.wake:
			w.Tick(w.now())
		}
	}
}

// Tick performs one history scan. Tests call it directly with a fixed clock.
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
	if w.state.PlayAllowed() || !settings.Enabled {
		w.resetBackoff()
		return
	}

	nowUTC := now.UTC()
	since := nowUTC.Add(-w.lookback)
	if !w.lastScan.IsZero() && nowUTC.Sub(w.lastScan) <= w.lookback {
		since = w.lastScan.Add(-w.overlap)
	}
	ctx := w.baseContext()
	historyCtx, cancelHistory := context.WithTimeout(ctx, 20*time.Second)
	visits, err := w.source.RecentVisits(historyCtx, since)
	cancelHistory()
	if err != nil {
		w.reportError(nowUTC, "Unable to read browser history: "+err.Error(), true)
		return
	}
	w.lastScan = nowUTC

	aiSettings := model.AISettings{}
	configured := false
	if w.settings != nil {
		aiSettings, _, configured = w.settings.AICredentials()
	}
	aiModel := strings.ToLower(strings.TrimSpace(aiSettings.Model))
	canAI := settings.AIClassification && configured && strings.TrimSpace(aiSettings.Model) != ""

	seen := make(map[string]int, len(visits))
	unique := make([]Visit, 0, len(visits))
	for _, visit := range visits {
		browser := strings.ToLower(strings.TrimSpace(visit.Browser))
		if browser != "chrome" && browser != "edge" {
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
		unique = append(unique, Visit{Browser: browser, Domain: domain, VisitedAt: visit.VisitedAt})
	}

	var unknown []Visit
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
			w.reportError(nowUTC, fmt.Sprintf("Unable to read browser domain %s: %v", visit.Domain, err), false)
			continue
		}
		if found && existing.Status == model.BrowserStatusAllowed {
			w.markVisit(visit)
			continue
		}
		if domains.IsSeed(visit.Domain) {
			_, changed, err := w.recorder.RecordBrowserSeed(visit.Domain, "Matched the built-in game seed list.", nowUTC)
			if err != nil {
				w.handleRecordError(nowUTC, visit.Domain, err)
				continue
			}
			w.recordDetection(model.BrowserDetection{
				Time: nowUTC, Browser: visit.Browser, Domain: visit.Domain,
				Source: model.BrowserSourceSeed, Action: "seed-blocked", Confidence: 1,
				Reason: "Matched the built-in game seed list.",
			})
			if changed {
				w.audit(model.AuditEvent{
					Time: nowUTC, Type: model.EventBrowser, Domain: visit.Domain,
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
				Time: nowUTC, Browser: visit.Browser, Domain: visit.Domain,
				Source: existing.Source, Action: "already-blocked",
				Confidence: existing.Confidence, Reason: existing.Reason,
			})
			w.markVisit(visit)
			continue
		}
		if found && existing.Status == model.BrowserStatusReview {
			w.recordDetection(model.BrowserDetection{
				Time: nowUTC, Browser: visit.Browser, Domain: visit.Domain,
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
		if cached && !nowUTC.Before(entry.expires) {
			delete(w.cache, cacheKey)
			cached = false
		}
		w.mu.Unlock()
		if cached {
			w.applyVerdict(visit, entry.verdict, aiSettings.Model, nowUTC)
			w.markVisit(visit)
			continue
		}
		unknown = append(unknown, visit)
	}

	if len(unknown) > 0 {
		w.classifyUnknown(ctx, unknown, aiSettings.Model, nowUTC)
	}
	if !w.hasError() {
		w.persistStatus(nowUTC, "")
	}
}

func (w *Watcher) classifyUnknown(ctx context.Context, visits []Visit, modelName string, now time.Time) {
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

func (w *Watcher) applyVerdict(visit Visit, verdict deepseek.DomainVerdict, modelName string, now time.Time) {
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

func (w *Watcher) shouldProcessVisit(visit Visit) bool {
	key := visit.Browser + "\x00" + visit.Domain
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.seenVisits[key]
	return !ok || visit.VisitedAt.After(last)
}

func (w *Watcher) markVisit(visit Visit) {
	key := visit.Browser + "\x00" + visit.Domain
	w.mu.Lock()
	defer w.mu.Unlock()
	if last, ok := w.seenVisits[key]; !ok || visit.VisitedAt.After(last) {
		w.seenVisits[key] = visit.VisitedAt
	}
	if len(w.seenVisits) > 20_000 {
		// Keep memory bounded; reprocessing old cached visits once is harmless.
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
