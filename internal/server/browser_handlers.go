package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

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
	return browserResponse{
		Settings: settings, Status: status,
		BlockedCount: blocked, ReviewCount: review, AllowedCount: allowed,
	}
}

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.browserResponse())
}

type browserSettingsRequest struct {
	Enabled          bool `json:"enabled"`
	AutoBlock        bool `json:"autoBlock"`
	AIClassification bool `json:"aiClassification"`
}

func (s *Server) handleBrowserSettings(w http.ResponseWriter, r *http.Request) {
	var req browserSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	settings, err := s.store.SaveBrowserSettings(req.Enabled, req.AutoBlock, req.AIClassification)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAIKeyRequired):
			writeAPIError(w, http.StatusBadRequest, "api_key_required", "Add a DeepSeek API key before enabling browser AI classification.")
		default:
			writeAPIError(w, http.StatusInternalServerError, "save_failed", "The browser monitoring settings could not be saved.")
		}
		return
	}
	message := "Browser monitoring disabled."
	if settings.Enabled {
		message = "Browser monitoring enabled while play is blocked."
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
