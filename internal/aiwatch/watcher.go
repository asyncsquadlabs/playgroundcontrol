// Package aiwatch detects unlisted game processes with DeepSeek while play is
// blocked. High-confidence games are added to the blocked game list and closed
// by the enforcer; lower-confidence findings are recorded for parent review.
package aiwatch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/deepseek"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/protection"
)

const (
	DefaultPollInterval        = 15 * time.Second
	DefaultMinRequestInterval  = 30 * time.Second
	DefaultMaxRequestsPerHour  = 60
	DefaultCacheTTL            = 24 * time.Hour
	DefaultTouchInterval       = time.Minute
	DefaultMinProcessAge       = 5 * time.Second
	DefaultConfidence          = 0.70
	DefaultAutoBlockConfidence = 0.90
	DefaultBackoffMin          = time.Minute
	DefaultBackoffMax          = 15 * time.Minute
	DefaultMaxCacheEntries     = 2_000
	maxBatchSize               = 20
)

// Source lists processes and enriches selected candidates with version
// metadata.
type Source interface {
	List() ([]enforce.Process, error)
	Describe(enforce.Process) enforce.Process
}

// State supplies the current permission flag and game list.
type State interface {
	PlayAllowed() bool
	Games() []model.Game
}

// Settings supplies the current AI configuration and in-memory key.
type Settings interface {
	AICredentials() (model.AISettings, string, bool)
}

// Classifier is the DeepSeek classification surface.
type Classifier interface {
	Classify(context.Context, string, string, []deepseek.Observation) ([]deepseek.Verdict, error)
	Test(context.Context, string, string) (time.Duration, error)
}

// Recorder persists findings, auto-blocked games, ignore entries, and provider
// health.
type Recorder interface {
	RecordStrangeActivity(model.StrangeActivity) (model.StrangeActivity, error)
	AddAIGame(model.Game) (model.Game, bool, error)
	AIIgnoredKeys() (map[string]struct{}, error)
	SetAIStatus(time.Time, string) error
}

// Blocker requests an immediate enforcement scan after a game is auto-blocked.
type Blocker interface {
	Wake()
}

// Auditor stores AI error/recovery transitions.
type Auditor interface {
	AddEvent(model.AuditEvent) error
}

// Options configures a Watcher. Zero values use production defaults.
type Options struct {
	PollInterval        time.Duration
	MinRequestInterval  time.Duration
	MaxRequestsPerHour  int
	CacheTTL            time.Duration
	TouchInterval       time.Duration
	MinProcessAge       time.Duration
	Confidence          float64
	AutoBlockConfidence float64
	BackoffMin          time.Duration
	BackoffMax          time.Duration
	MaxCacheEntries     int
	Now                 func() time.Time
}

type candidate struct {
	process     enforce.Process
	observation deepseek.Observation
	key         string
	advisoryDue bool
}

type cacheEntry struct {
	verdict   deepseek.Verdict
	expires   time.Time
	lastTouch time.Time
}

// Watcher is safe for concurrent use.
type Watcher struct {
	source     Source
	state      State
	settings   Settings
	classifier Classifier
	recorder   Recorder
	auditor    Auditor
	blocker    Blocker

	interval            time.Duration
	minRequest          time.Duration
	maxRequests         int
	cacheTTL            time.Duration
	touchInterval       time.Duration
	minProcessAge       time.Duration
	confidence          float64
	autoBlockConfidence float64
	backoffMin          time.Duration
	backoffMax          time.Duration
	maxCache            int
	now                 func() time.Time
	baseCtx             context.Context
	wake                chan struct{}
	tickMu              sync.Mutex
	mu                  sync.Mutex
	cache               map[string]cacheEntry
	requestTimes        []time.Time
	lastRequest         time.Time
	lastStatusAt        time.Time
	lastError           string
	nextAttempt         time.Time
	backoff             time.Duration
}

// New builds a watcher.
func New(source Source, state State, settings Settings, classifier Classifier, recorder Recorder, auditor Auditor, blocker Blocker, options Options) *Watcher {
	if options.PollInterval <= 0 {
		options.PollInterval = DefaultPollInterval
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
	if options.TouchInterval <= 0 {
		options.TouchInterval = DefaultTouchInterval
	}
	if options.MinProcessAge <= 0 {
		options.MinProcessAge = DefaultMinProcessAge
	}
	if options.Confidence <= 0 || options.Confidence > 1 {
		options.Confidence = DefaultConfidence
	}
	if options.AutoBlockConfidence <= 0 || options.AutoBlockConfidence > 1 {
		options.AutoBlockConfidence = DefaultAutoBlockConfidence
	}
	if options.AutoBlockConfidence < options.Confidence {
		options.AutoBlockConfidence = options.Confidence
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
		source:              source,
		state:               state,
		settings:            settings,
		classifier:          classifier,
		recorder:            recorder,
		auditor:             auditor,
		blocker:             blocker,
		interval:            options.PollInterval,
		minRequest:          options.MinRequestInterval,
		maxRequests:         options.MaxRequestsPerHour,
		cacheTTL:            options.CacheTTL,
		touchInterval:       options.TouchInterval,
		minProcessAge:       options.MinProcessAge,
		confidence:          options.Confidence,
		autoBlockConfidence: options.AutoBlockConfidence,
		backoffMin:          options.BackoffMin,
		backoffMax:          options.BackoffMax,
		maxCache:            options.MaxCacheEntries,
		now:                 options.Now,
		wake:                make(chan struct{}, 1),
		cache:               make(map[string]cacheEntry),
	}
}

// Wake requests an immediate scan.
func (w *Watcher) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run scans until the context is canceled.
func (w *Watcher) Run(ctx context.Context) {
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

// Tick performs one scan. Tests call it directly with a fixed clock.
func (w *Watcher) Tick(now time.Time) {
	w.tickMu.Lock()
	defer w.tickMu.Unlock()
	if w.state == nil || w.source == nil || w.settings == nil || w.classifier == nil || w.recorder == nil {
		return
	}
	if w.state.PlayAllowed() {
		w.resetBackoff()
		return
	}
	settings, apiKey, configured := w.settings.AICredentials()
	if !settings.Enabled || !configured || strings.TrimSpace(apiKey) == "" {
		w.resetBackoff()
		return
	}
	ignored, err := w.recorder.AIIgnoredKeys()
	if err != nil {
		w.reportError(now, fmt.Sprintf("Unable to read the AI ignore list: %v", err), false, false)
		return
	}

	processes, err := w.source.List()
	if err != nil {
		w.reportError(now, fmt.Sprintf("Unable to enumerate processes for AI monitoring: %v", err), false, false)
		return
	}
	games := append([]model.Game(nil), w.state.Games()...)
	games = append(games, protection.RobloxGames()...)

	nowUTC := now.UTC()
	processes = append([]enforce.Process(nil), processes...)
	sort.Slice(processes, func(i, j int) bool {
		if !strings.EqualFold(processes[i].Name, processes[j].Name) {
			return strings.ToLower(processes[i].Name) < strings.ToLower(processes[j].Name)
		}
		return processes[i].PID < processes[j].PID
	})

	observations := make([]candidate, 0, maxBatchSize)
	positiveCached := make([]candidate, 0)
	seen := make(map[string]struct{})
	observationSeen := make(map[string]struct{})

	w.mu.Lock()
	w.pruneRequestWindow(nowUTC)
	w.pruneCache(nowUTC)
	for _, process := range processes {
		if !eligibleProcess(process, nowUTC, w.minProcessAge) || coveredByGame(process, games) {
			continue
		}
		if isIgnoredProcess(process, ignored) {
			continue
		}
		key := processKey(process)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		cacheKey := key + "\x00" + strings.ToLower(settings.Model)
		if entry, ok := w.cache[cacheKey]; ok {
			if nowUTC.Before(entry.expires) {
				if entry.verdict.IsGame && entry.verdict.Confidence >= w.confidence {
					autoBlockDue := settings.AutoBlock && entry.verdict.Confidence >= w.autoBlockConfidence
					touchDue := entry.lastTouch.IsZero() || nowUTC.Sub(entry.lastTouch) >= w.touchInterval
					if autoBlockDue || touchDue {
						if touchDue {
							entry.lastTouch = nowUTC
							w.cache[cacheKey] = entry
						}
						positiveCached = append(positiveCached, candidate{
							process:     process,
							key:         cacheKey,
							advisoryDue: touchDue,
						})
					}
				}
				continue
			}
			delete(w.cache, cacheKey)
		}
		if len(observations) >= maxBatchSize {
			continue
		}
		described := w.source.Describe(process)
		observation := observationFor(described)
		observationKeyValue := observationKey(observation.Executable, observation.ImagePath)
		if _, duplicate := observationSeen[observationKeyValue]; duplicate {
			continue
		}
		observationSeen[observationKeyValue] = struct{}{}
		observations = append(observations, candidate{
			process:     described,
			observation: observation,
			key:         cacheKey,
		})
	}
	w.mu.Unlock()

	w.touchCached(nowUTC, settings.Model, positiveCached)
	if len(observations) == 0 {
		return
	}

	w.mu.Lock()
	blocked := w.nextAttempt.After(nowUTC) ||
		(!w.lastRequest.IsZero() && nowUTC.Sub(w.lastRequest) < w.minRequest) ||
		len(w.requestTimes) >= w.maxRequests
	if !blocked {
		w.lastRequest = nowUTC
		w.requestTimes = append(w.requestTimes, nowUTC)
	}
	w.mu.Unlock()
	if blocked {
		return
	}

	observationsForModel := make([]deepseek.Observation, len(observations))
	processByObservation := make(map[string]enforce.Process, len(observations))
	for i, item := range observations {
		observationsForModel[i] = item.observation
		processByObservation[observationKey(item.observation.Executable, item.observation.ImagePath)] = item.process
	}

	w.mu.Lock()
	baseCtx := w.baseCtx
	w.mu.Unlock()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 25*time.Second)
	defer cancel()
	verdicts, err := w.classifier.Classify(ctx, apiKey, settings.Model, observationsForModel)
	if err != nil {
		if baseCtx.Err() != nil {
			return
		}
		w.reportError(nowUTC, fmt.Sprintf("DeepSeek classification failed: %v", err), true, false)
		return
	}
	w.reportSuccess(nowUTC, false)

	for _, verdict := range verdicts {
		process, ok := processByObservation[observationKey(verdict.Executable, verdict.ImagePath)]
		if !ok {
			continue
		}
		cacheKey := processKey(process) + "\x00" + strings.ToLower(settings.Model)
		w.mu.Lock()
		w.cache[cacheKey] = cacheEntry{
			verdict:   verdict,
			expires:   nowUTC.Add(w.cacheTTL),
			lastTouch: nowUTC,
		}
		w.pruneCache(nowUTC)
		w.mu.Unlock()

		w.handlePositiveVerdict(process, verdict, settings.Model, nowUTC, true)
	}
}

// Test performs one minimal DeepSeek classification and updates health status.
func (w *Watcher) Test(ctx context.Context) (time.Duration, error) {
	if w == nil || w.settings == nil || w.classifier == nil {
		return 0, errors.New("AI monitoring is unavailable")
	}
	settings, apiKey, configured := w.settings.AICredentials()
	if !configured || strings.TrimSpace(apiKey) == "" {
		return 0, errors.New("a DeepSeek API key is not configured")
	}
	latency, err := w.classifier.Test(ctx, apiKey, settings.Model)
	now := w.now().UTC()
	if err != nil {
		w.reportError(now, fmt.Sprintf("DeepSeek connection test failed: %v", err), false, true)
		return latency, err
	}
	w.reportSuccess(now, true)
	return latency, nil
}

func (w *Watcher) touchCached(now time.Time, modelName string, candidates []candidate) {
	if w.state == nil || w.state.PlayAllowed() {
		return
	}
	for _, item := range candidates {
		w.mu.Lock()
		entry, ok := w.cache[item.key]
		w.mu.Unlock()
		if !ok || !entry.verdict.IsGame || entry.verdict.Confidence < w.confidence {
			continue
		}
		described := w.source.Describe(item.process)
		w.handlePositiveVerdict(described, entry.verdict, modelName, now, item.advisoryDue)
	}
}

// handlePositiveVerdict applies auto-block or advisory handling to one
// validated game verdict. It re-reads the authoritative settings before acting.
func (w *Watcher) handlePositiveVerdict(process enforce.Process, verdict deepseek.Verdict, modelName string, now time.Time, advisoryDue bool) {
	if w.state == nil || w.settings == nil || w.recorder == nil {
		return
	}
	if !verdict.IsGame || verdict.Confidence < w.confidence || w.state.PlayAllowed() {
		return
	}
	settings, apiKey, configured := w.settings.AICredentials()
	if !configured || !settings.Enabled || strings.TrimSpace(apiKey) == "" || settings.Model != modelName {
		return
	}
	currentGames := append([]model.Game(nil), w.state.Games()...)
	currentGames = append(currentGames, protection.RobloxGames()...)
	if coveredByGame(process, currentGames) {
		return
	}
	if settings.AutoBlock && verdict.Confidence >= w.autoBlockConfidence &&
		strings.TrimSpace(process.ImagePath) != "" {
		if w.attemptAutoBlock(process, verdict, settings.Model, now) {
			return
		}
	}
	if !advisoryDue {
		return
	}
	if _, err := w.recorder.RecordStrangeActivity(
		strangeActivityFromVerdict(process, verdict, settings.Model, now),
	); err != nil {
		w.reportError(now, fmt.Sprintf("Could not update strange activity: %v", err), false, false)
	}
}

// attemptAutoBlock adds one high-confidence game to the blocked list and wakes
// the enforcer. It returns true when the verdict was handled or intentionally
// skipped, and false when auto-block failed so the caller can record a finding.
func (w *Watcher) attemptAutoBlock(process enforce.Process, verdict deepseek.Verdict, modelName string, now time.Time) bool {
	settings, apiKey, configured := w.settings.AICredentials()
	if !configured || !settings.Enabled || !settings.AutoBlock ||
		strings.TrimSpace(apiKey) == "" || settings.Model != modelName {
		return true
	}
	if w.state.PlayAllowed() {
		return true
	}
	ignored, err := w.recorder.AIIgnoredKeys()
	if err != nil {
		w.reportError(now, fmt.Sprintf("Unable to read the AI ignore list: %v", err), false, false)
		return true
	}
	if isIgnoredProcess(process, ignored) {
		return true
	}
	currentGames := append([]model.Game(nil), w.state.Games()...)
	currentGames = append(currentGames, protection.RobloxGames()...)
	if coveredByGame(process, currentGames) {
		return true
	}

	name := safeName(verdict.Name, process)
	game, skipped, err := w.recorder.AddAIGame(model.Game{
		Name:       name,
		Executable: strings.TrimSpace(process.Name),
		ImagePath:  strings.TrimSpace(process.ImagePath),
		Source:     model.GameSourceAI,
		AddedAt:    now,
	})
	if err != nil {
		w.audit(model.AuditEvent{
			Time:     now,
			Type:     model.EventAI,
			Message:  fmt.Sprintf("Could not auto-block %s: %v", name, err),
			GameName: name,
			PID:      process.PID,
			Result:   "ai-auto-block-failed",
		})
		w.reportError(now, fmt.Sprintf("Could not auto-block %s: %v", name, err), false, false)
		return false
	}
	if skipped {
		return true
	}
	if game.Name == "" {
		game.Name = name
	}
	w.audit(model.AuditEvent{
		Time:     now,
		Type:     model.EventAI,
		Message:  fmt.Sprintf("AI auto-blocked game: %s (%s, confidence %.2f) - %s", game.Name, game.Executable, verdict.Confidence, safeText(verdict.Reason, 200)),
		GameID:   game.ID,
		GameName: game.Name,
		PID:      process.PID,
		Result:   "ai-auto-blocked",
	})
	if w.blocker != nil {
		w.blocker.Wake()
	}
	return true
}

func isIgnoredProcess(process enforce.Process, ignored map[string]struct{}) bool {
	if len(ignored) == 0 {
		return false
	}
	_, ok := ignored[model.StrangeActivityKey(process.Name, process.ImagePath)]
	return ok
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

func (w *Watcher) resetBackoff() {
	w.mu.Lock()
	w.backoff = 0
	w.nextAttempt = time.Time{}
	w.mu.Unlock()
}

func (w *Watcher) reportSuccess(now time.Time, force bool) {
	w.mu.Lock()
	hadError := w.lastError != ""
	w.lastError = ""
	w.backoff = 0
	w.nextAttempt = time.Time{}
	shouldPersist := force || hadError || w.lastStatusAt.IsZero() || now.Sub(w.lastStatusAt) >= 5*time.Minute
	if shouldPersist {
		w.lastStatusAt = now
	}
	w.mu.Unlock()
	if hadError {
		w.audit(model.AuditEvent{
			Time:    now,
			Type:    model.EventAI,
			Message: "DeepSeek monitoring recovered.",
			Result:  "ai-recovered",
		})
	}
	if shouldPersist && w.recorder != nil {
		_ = w.recorder.SetAIStatus(now, "")
	}
}

func (w *Watcher) reportError(now time.Time, message string, backoff, force bool) {
	message = truncate(message, 300)
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
	shouldPersist := force || changed || w.lastStatusAt.IsZero() || now.Sub(w.lastStatusAt) >= 5*time.Minute
	if shouldPersist {
		w.lastStatusAt = now
	}
	w.mu.Unlock()
	if changed {
		w.audit(model.AuditEvent{
			Time:    now,
			Type:    model.EventAI,
			Message: message,
			Result:  "ai-error",
		})
	}
	if shouldPersist && w.recorder != nil {
		_ = w.recorder.SetAIStatus(time.Time{}, message)
	}
}

func (w *Watcher) audit(event model.AuditEvent) {
	if w.auditor == nil {
		return
	}
	_ = w.auditor.AddEvent(event)
}

func eligibleProcess(process enforce.Process, now time.Time, minAge time.Duration) bool {
	if process.PID == 0 || process.SessionID == 0 || strings.TrimSpace(process.Name) == "" {
		return false
	}
	if !process.CreatedAt.IsZero() {
		age := now.Sub(process.CreatedAt.UTC())
		if age < minAge {
			return false
		}
	}
	name := strings.ToLower(strings.TrimSpace(process.Name))
	if knownShellProcesses[name] || name == "playgroundcontrol.exe" {
		return false
	}
	path := normalizePath(process.ImagePath)
	if path == "" {
		return true
	}
	for _, prefix := range []string{
		`c:\windows\`,
		`c:\program files\playgroundcontrol\`,
		`c:\program files (x86)\playgroundcontrol\`,
	} {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	return true
}

func coveredByGame(process enforce.Process, games []model.Game) bool {
	for _, game := range games {
		if game.Matches(process.Name, process.ImagePath) {
			return true
		}
	}
	_, roblox := protection.MatchRobloxProcess(process.Name, process.ImagePath)
	return roblox
}

func observationFor(process enforce.Process) deepseek.Observation {
	return deepseek.Observation{
		Executable:      strings.TrimSpace(process.Name),
		ImagePath:       redactImagePath(process.ImagePath),
		ProductName:     strings.TrimSpace(process.ProductName),
		CompanyName:     strings.TrimSpace(process.CompanyName),
		FileDescription: strings.TrimSpace(process.FileDescription),
	}
}

func strangeActivityFromVerdict(process enforce.Process, verdict deepseek.Verdict, modelName string, now time.Time) model.StrangeActivity {
	return model.StrangeActivity{
		Name:            safeName(verdict.Name, process),
		Executable:      strings.TrimSpace(process.Name),
		ImagePath:       process.ImagePath,
		ProductName:     strings.TrimSpace(process.ProductName),
		CompanyName:     strings.TrimSpace(process.CompanyName),
		FileDescription: strings.TrimSpace(process.FileDescription),
		Confidence:      verdict.Confidence,
		Reason:          safeText(verdict.Reason, 300),
		FirstSeen:       now,
		LastSeen:        now,
		SeenCount:       1,
		LastPID:         process.PID,
		Model:           modelName,
	}
}

func safeName(aiName string, process enforce.Process) string {
	for _, candidate := range []string{
		aiName,
		process.ProductName,
		process.FileDescription,
		strings.TrimSuffix(process.Name, ".exe"),
	} {
		candidate = safeText(candidate, 80)
		if candidate != "" {
			return candidate
		}
	}
	return "Unrecognized game"
}

func safeText(value string, max int) string {
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

func processKey(process enforce.Process) string {
	return strings.ToLower(strings.TrimSpace(process.Name)) + "\x00" +
		strings.ToLower(normalizePath(process.ImagePath))
}

func observationKey(executable, imagePath string) string {
	return strings.ToLower(strings.TrimSpace(executable)) + "\x00" +
		strings.ToLower(strings.TrimSpace(imagePath))
}

func normalizePath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "/", `\`))
	path = strings.TrimPrefix(path, `\\?\`)
	path = strings.TrimPrefix(path, `\??\`)
	return strings.ToLower(path)
}

var userProfilePattern = regexp.MustCompile(`(?i)^([a-z]:\\(?:users|documents and settings)\\)[^\\]+`)

func redactImagePath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "/", `\`))
	path = strings.TrimPrefix(path, `\\?\`)
	path = strings.TrimPrefix(path, `\??\`)
	return userProfilePattern.ReplaceAllString(path, `${1}<user>`)
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max]
}

var knownShellProcesses = map[string]bool{
	"explorer.exe":                true,
	"dwm.exe":                     true,
	"sihost.exe":                  true,
	"ctfmon.exe":                  true,
	"taskhostw.exe":               true,
	"runtimebroker.exe":           true,
	"searchhost.exe":              true,
	"startmenuexperiencehost.exe": true,
	"textinputhost.exe":           true,
	"shellexperiencehost.exe":     true,
	"lockapp.exe":                 true,
	"logonui.exe":                 true,
	"userinit.exe":                true,
	"fontdrvhost.exe":             true,
	"backgroundtaskhost.exe":      true,
	"applicationframehost.exe":    true,
	"systemsettings.exe":          true,
	"widgetservice.exe":           true,
	"widgetboard.exe":             true,
	"securityhealthsystray.exe":   true,
}
