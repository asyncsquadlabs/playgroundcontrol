package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/domains"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

type browserResponse struct {
	Settings     model.BrowserSettings `json:"settings"`
	Status       model.BrowserStatus   `json:"status"`
	BlockedCount int                   `json:"blockedCount"`
	ReviewCount  int                   `json:"reviewCount"`
	AllowedCount int                   `json:"allowedCount"`
}

func (s *Server) browserResponse() browserResponse {
	settings := s.store.BrowserSettings()
	status := model.BrowserStatus{Browsers: []string{}}
	if s.browser != nil {
		status = s.browser.Status()
	}
	if status.Browsers == nil {
		status.Browsers = []string{}
	}
	blocked, review, allowed, _ := s.store.BrowserDomainCounts()
	status = s.enrichBrowserStatus(status, settings)
	return browserResponse{
		Settings: settings, Status: status,
		BlockedCount: blocked, ReviewCount: review, AllowedCount: allowed,
	}
}

func (s *Server) enrichBrowserStatus(status model.BrowserStatus, settings model.BrowserSettings) model.BrowserStatus {
	if status.Browsers == nil {
		status.Browsers = []string{}
	}
	if status.PolicyBrowsers == nil {
		status.PolicyBrowsers = []string{}
	}
	if status.FallbackOnlyBrowsers == nil {
		status.FallbackOnlyBrowsers = []string{}
	}
	if status.RestartRequiredBrowsers == nil {
		status.RestartRequiredBrowsers = []string{}
	}
	users, err := s.store.BrowserJournalUserCount()
	if err == nil {
		status.MonitoredUsers = users
	}
	events, alerts, err := s.store.BrowserActivityCounts(s.now().UTC())
	if err == nil {
		status.ActivityEvents24h = events
		status.UnacknowledgedHistoryAlerts = alerts
	}
	status.ActivityLoggingActive = settings.Enabled && settings.ActivityLogging && users > 0
	status.ActivityBackfillPending = settings.Enabled && settings.ActivityLogging && users > 0 && settings.ActivityBackfillAt.IsZero()
	return status
}

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.browserResponse())
}

type browserSettingsRequest struct {
	Enabled          bool `json:"enabled"`
	AutoBlock        bool `json:"autoBlock"`
	AIClassification bool `json:"aiClassification"`
	ActivityLogging  bool `json:"activityLogging"`
}

func (s *Server) handleBrowserSettings(w http.ResponseWriter, r *http.Request) {
	var req browserSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	settings, err := s.store.SaveBrowserSettings(req.Enabled, req.AutoBlock, req.AIClassification, req.ActivityLogging)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAIKeyRequired):
			writeAPIError(w, http.StatusBadRequest, "api_key_required", "Add a DeepSeek API key before enabling browser AI classification.")
		case errors.Is(err, store.ErrBrowserActivityRequiresMonitoring):
			writeAPIError(w, http.StatusBadRequest, "monitoring_required", "Browser monitoring must be enabled before activity logging.")
		default:
			writeAPIError(w, http.StatusInternalServerError, "save_failed", "The browser monitoring settings could not be saved.")
		}
		return
	}
	message := "Browser monitoring disabled."
	if settings.Enabled {
		message = "Browser monitoring enabled."
		if settings.ActivityLogging {
			message += " Detailed browser activity logging is on."
		}
		if settings.AIClassification {
			message += " Unknown domains are classified by AI."
		}
	}
	s.audit(model.AuditEvent{
		Type:    model.EventBrowser,
		Message: message,
		ActorIP: clientIPString(r),
		Result:  "browser-settings",
	})
	if s.browser != nil {
		_ = s.browser.Sync()
		s.browser.Wake()
	}
	if s.browserWatch != nil {
		s.browserWatch.Wake()
	}
	writeJSON(w, http.StatusOK, s.browserResponse())
}

func (s *Server) handleBrowserDomains(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "", model.BrowserStatusBlocked, model.BrowserStatusReview, model.BrowserStatusAllowed:
	default:
		writeAPIError(w, http.StatusBadRequest, "invalid_status", "The browser domain status is invalid.")
		return
	}
	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_limit", "Limit must be a positive integer.")
			return
		}
		limit = parsed
	}
	list, err := s.store.BrowserDomains(status, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "domains_failed", "Browser domains could not be read.")
		return
	}
	if list == nil {
		list = []model.BrowserDomain{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": list})
}

type addBrowserDomainRequest struct {
	Domain string `json:"domain"`
}

func (s *Server) handleAddBrowserDomain(w http.ResponseWriter, r *http.Request) {
	var req addBrowserDomainRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	domain, err := s.store.AddBrowserDomain(req.Domain, s.now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, domains.ErrInvalidDomain):
			writeAPIError(w, http.StatusBadRequest, "invalid_domain", "Enter a valid public domain, such as example.com.")
		case errors.Is(err, store.ErrDuplicateBrowserDomain):
			writeAPIError(w, http.StatusConflict, "duplicate_domain", err.Error())
		case errors.Is(err, store.ErrBrowserDomainLimit):
			writeAPIError(w, http.StatusConflict, "domain_limit", "The blocked browser domain limit has been reached. Allow or remove another domain first.")
		default:
			writeAPIError(w, http.StatusInternalServerError, "save_failed", "The browser domain could not be saved.")
		}
		return
	}
	s.audit(model.AuditEvent{
		Type:    model.EventBrowser,
		Message: "Browser game domain added by the administrator: " + domain.Domain,
		ActorIP: clientIPString(r),
		Domain:  domain.Domain,
		Result:  "browser-domain-added",
	})
	if s.browser != nil {
		_ = s.browser.Sync()
		s.browser.Wake()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"domain": domain})
}

type browserDomainStatusRequest struct {
	Status string `json:"status"`
}

func (s *Server) handleSetBrowserDomainStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_domain_id", "A browser domain ID is required.")
		return
	}
	var req browserDomainStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	domain, err := s.store.SetBrowserDomainStatus(id, req.Status, s.now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBrowserDomainNotFound):
			writeAPIError(w, http.StatusNotFound, "domain_not_found", "That browser domain is not configured.")
		case errors.Is(err, store.ErrBrowserDomainLimit):
			writeAPIError(w, http.StatusConflict, "domain_limit", "The blocked browser domain limit has been reached. Allow or remove another domain first.")
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_status", "The browser domain status must be blocked or allowed.")
		}
		return
	}
	message := "Browser game domain blocked: " + domain.Domain
	result := "browser-domain-blocked"
	if domain.Status == model.BrowserStatusAllowed {
		message = "Browser game domain allowed by the administrator: " + domain.Domain
		result = "browser-domain-allowed"
	}
	s.audit(model.AuditEvent{
		Type:    model.EventBrowser,
		Message: message,
		ActorIP: clientIPString(r),
		Domain:  domain.Domain,
		Result:  result,
	})
	if s.browser != nil {
		_ = s.browser.Sync()
		s.browser.Wake()
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": domain})
}

func (s *Server) handleBrowserDetections(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_limit", "Limit must be a positive integer.")
			return
		}
		limit = parsed
	}
	detections, err := s.store.RecentBrowserDetections(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "detections_failed", "Browser detections could not be read.")
		return
	}
	if detections == nil {
		detections = []model.BrowserDetection{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"detections": detections})
}

func (s *Server) listBrowserUsers(ctx context.Context) ([]model.BrowserUser, error) {
	discovered := []model.BrowserUser{}
	if s.browserUsers != nil {
		if users, err := s.browserUsers.BrowserUsers(ctx); err == nil {
			discovered = users
		}
	}
	stored, err := s.store.BrowserJournalUsers()
	if err != nil {
		return nil, err
	}
	selected := make(map[string]model.BrowserUser, len(stored))
	for _, user := range stored {
		selected[strings.ToUpper(strings.TrimSpace(user.SID))] = user
	}
	merged := make(map[string]model.BrowserUser, len(discovered)+len(selected))
	for _, user := range discovered {
		key := strings.ToUpper(strings.TrimSpace(user.SID))
		if key == "" {
			continue
		}
		if current, ok := selected[key]; ok {
			user.Selected = true
			if user.UserName == "" {
				user.UserName = current.UserName
			}
		}
		user.Available = true
		if user.Browsers == nil {
			user.Browsers = []string{}
		}
		merged[key] = user
	}
	for key, user := range selected {
		if _, ok := merged[key]; ok {
			continue
		}
		user.Selected = true
		user.Available = false
		if user.Browsers == nil {
			user.Browsers = []string{}
		}
		merged[key] = user
	}
	out := make([]model.BrowserUser, 0, len(merged))
	for _, user := range merged {
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserName != out[j].UserName {
			return out[i].UserName < out[j].UserName
		}
		return out[i].SID < out[j].SID
	})
	return out, nil
}

func (s *Server) handleBrowserUsers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	users, err := s.listBrowserUsers(ctx)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "users_failed", "The Windows accounts could not be read.")
		return
	}
	if users == nil {
		users = []model.BrowserUser{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

type browserUsersRequest struct {
	SIDs []string `json:"sids"`
}

func (s *Server) handleSetBrowserUsers(w http.ResponseWriter, r *http.Request) {
	var req browserUsersRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(req.SIDs) > 200 {
		writeAPIError(w, http.StatusBadRequest, "too_many_users", "Select at most 200 Windows accounts.")
		return
	}
	seen := make(map[string]struct{}, len(req.SIDs))
	selected := make([]model.BrowserUser, 0, len(req.SIDs))
	for _, raw := range req.SIDs {
		sid := strings.ToUpper(strings.TrimSpace(raw))
		if !strings.HasPrefix(sid, "S-1-") || len(sid) > 128 {
			writeAPIError(w, http.StatusBadRequest, "invalid_sid", "One of the selected Windows accounts is invalid.")
			return
		}
		if _, duplicate := seen[sid]; duplicate {
			continue
		}
		seen[sid] = struct{}{}
		selected = append(selected, model.BrowserUser{SID: sid, Selected: true})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	users, _ := s.listBrowserUsers(ctx)
	cancel()
	names := make(map[string]string, len(users))
	for _, user := range users {
		names[strings.ToUpper(strings.TrimSpace(user.SID))] = user.UserName
	}
	for i := range selected {
		if name := names[selected[i].SID]; name != "" {
			selected[i].UserName = name
		}
	}
	if err := s.store.SaveBrowserJournalUsers(selected, s.now().UTC()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save_failed", "The monitored Windows accounts could not be saved.")
		return
	}
	message := fmt.Sprintf("Browser journal account selection updated (%d selected).", len(selected))
	if len(selected) == 0 {
		message = "Browser journal account selection cleared."
	}
	s.audit(model.AuditEvent{
		Type: model.EventBrowser, Message: message, ActorIP: clientIPString(r),
		Result: "browser-journal-users",
	})
	if s.browserWatch != nil {
		s.browserWatch.Wake()
	}
	refreshCtx, refreshCancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer refreshCancel()
	updated, err := s.listBrowserUsers(refreshCtx)
	if err != nil {
		updated = selected
	}
	if updated == nil {
		updated = []model.BrowserUser{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": updated})
}

func (s *Server) parseActivityFilter(r *http.Request) (model.BrowserActivityFilter, string) {
	now := s.now().UTC()
	to := now
	var filter model.BrowserActivityFilter
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, "The end time is invalid."
		}
		to = parsed.UTC()
	}
	from := to.Add(-24 * time.Hour)
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return filter, "The start time is invalid."
		}
		from = parsed.UTC()
	}
	if !to.After(from) {
		return filter, "The end time must be after the start time."
	}
	if to.Sub(from) > store.BrowserActivityRetention {
		return filter, "The activity range cannot exceed 90 days."
	}
	filter.From = from
	filter.To = to
	filter.UserName = strings.TrimSpace(r.URL.Query().Get("user"))
	if len(filter.UserName) > 128 {
		return filter, "The user filter is too long."
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("browser")); raw != "" {
		browser, ok := model.NormalizeBrowser(raw)
		if !ok {
			return filter, "The browser filter is invalid."
		}
		filter.Browser = browser
	}
	filter.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	if len(filter.Query) > 200 {
		return filter, "The search filter is too long."
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("removed")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return filter, "The removed filter must be true or false."
		}
		filter.RemovedOnly = parsed
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			return filter, "Limit must be between 1 and 200."
		}
		filter.Limit = parsed
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			return filter, "Offset must be zero or greater."
		}
		filter.Offset = parsed
	}
	return filter, ""
}

func (s *Server) handleBrowserActivity(w http.ResponseWriter, r *http.Request) {
	filter, message := s.parseActivityFilter(r)
	if message != "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_filter", message)
		return
	}
	activity, hasMore, err := s.store.BrowserActivity(filter)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "activity_failed", "Browser activity could not be read.")
		return
	}
	alerts, err := s.store.BrowserHistoryAlerts(20)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "alerts_failed", "Browser history alerts could not be read.")
		return
	}
	if activity == nil {
		activity = []model.BrowserActivity{}
	}
	if alerts == nil {
		alerts = []model.BrowserHistoryAlert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activity": activity,
		"hasMore":  hasMore,
		"alerts":   alerts,
	})
}

func (s *Server) handleBrowserActivityFilters(w http.ResponseWriter, r *http.Request) {
	users, browsers, err := s.store.BrowserActivityFilterValues()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "filters_failed", "Browser activity filters could not be read.")
		return
	}
	if users == nil {
		users = []string{}
	}
	if browsers == nil {
		browsers = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "browsers": browsers})
}

func (s *Server) handleAcknowledgeBrowserHistoryAlert(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_alert_id", "An alert ID is required.")
		return
	}
	ok, err := s.store.AcknowledgeBrowserHistoryAlert(id, s.now().UTC())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "acknowledge_failed", "The history alert could not be acknowledged.")
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "alert_not_found", "That history alert is not available.")
		return
	}
	s.audit(model.AuditEvent{
		Type: model.EventBrowser, Message: "Browser history-clear alert acknowledged by the administrator.",
		ActorIP: clientIPString(r), Result: "browser-history-alert-acknowledged",
	})
	w.WriteHeader(http.StatusNoContent)
}
