package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

const (
	defaultStrangeLimit = 50
	maxStrangeLimit     = 200
)

type aiSettingsRequest struct {
	Enabled     bool   `json:"enabled"`
	AutoBlock   bool   `json:"autoBlock"`
	Model       string `json:"model"`
	APIKey      string `json:"apiKey"`
	ClearAPIKey bool   `json:"clearApiKey"`
}

func (s *Server) handleAISettings(w http.ResponseWriter, r *http.Request) {
	var req aiSettingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	settings, err := s.store.SaveAISettings(req.Enabled, req.AutoBlock, req.Model, req.APIKey, req.ClearAPIKey)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAIKeyRequired):
			writeAPIError(w, http.StatusBadRequest, "api_key_required", "Add a DeepSeek API key before enabling AI monitoring.")
		case errors.Is(err, store.ErrAIConfiguration):
			writeAPIError(w, http.StatusBadRequest, "invalid_ai_settings", err.Error())
		default:
			writeAPIError(w, http.StatusInternalServerError, "save_failed", "The AI settings could not be saved.")
		}
		return
	}

	message := "AI monitoring disabled."
	if settings.Enabled {
		message = "AI monitoring enabled."
		if settings.AutoBlock {
			message += " High-confidence auto-block is on."
		} else {
			message += " Auto-block is off; findings require review."
		}
	}
	if strings.TrimSpace(req.APIKey) != "" {
		message += " DeepSeek API key updated."
	}
	if req.ClearAPIKey {
		message += " DeepSeek API key removed."
	}
	s.audit(model.AuditEvent{
		Type:    model.EventAI,
		Message: message,
		ActorIP: clientIPString(r),
		Result:  "success",
	})
	if s.ai != nil {
		s.ai.Wake()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ai": settings})
}

func (s *Server) handleTestAI(w http.ResponseWriter, r *http.Request) {
	if s.ai == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "ai_unavailable", "AI monitoring is not available in this service instance.")
		return
	}
	if !s.store.AISettings().KeyConfigured {
		writeAPIError(w, http.StatusBadRequest, "api_key_required", "Configure a DeepSeek API key before testing.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	latency, err := s.ai.Test(ctx)
	if err != nil {
		s.audit(model.AuditEvent{
			Type:    model.EventAI,
			Message: "DeepSeek connection test failed: " + err.Error(),
			ActorIP: clientIPString(r),
			Result:  "ai-test-failed",
		})
		writeAPIError(w, http.StatusBadGateway, "ai_test_failed", "DeepSeek connection test failed: "+err.Error())
		return
	}
	s.audit(model.AuditEvent{
		Type:    model.EventAI,
		Message: "DeepSeek connection test succeeded.",
		ActorIP: clientIPString(r),
		Result:  "ai-test-ok",
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"model":     s.store.AISettings().Model,
		"latencyMs": latency.Milliseconds(),
	})
}

func (s *Server) handleStrangeActivity(w http.ResponseWriter, r *http.Request) {
	limit := defaultStrangeLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_limit", "Limit must be a positive integer.")
			return
		}
		limit = parsed
		if limit > maxStrangeLimit {
			limit = maxStrangeLimit
		}
	}
	activity, err := s.store.RecentStrangeActivity(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "activity_failed", "Strange activity could not be read.")
		return
	}
	if activity == nil {
		activity = []model.StrangeActivity{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": activity})
}

func (s *Server) handleIgnoreStrangeActivity(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_activity_id", "A strange activity ID is required.")
		return
	}
	removed, err := s.store.IgnoreStrangeActivity(id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save_failed", "The strange activity could not be ignored.")
		return
	}
	if !removed {
		writeAPIError(w, http.StatusNotFound, "strange_activity_not_found", "That strange activity was not found.")
		return
	}
	s.audit(model.AuditEvent{
		Type:    model.EventAI,
		Message: "Strange activity marked as not a game; AI will ignore this executable and path.",
		ActorIP: clientIPString(r),
		Result:  "ignored",
	})
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleBlockStrangeActivity(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_activity_id", "A strange activity ID is required.")
		return
	}
	game, err := s.store.PromoteStrangeActivity(id)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStrangeActivityNotFound):
			writeAPIError(w, http.StatusNotFound, "strange_activity_not_found", "That strange activity was not found.")
		case errors.Is(err, store.ErrDuplicateGame):
			writeAPIError(w, http.StatusConflict, "duplicate_game", "That game is already in the blocked list. Mark the finding as not a game instead.")
		default:
			writeAPIError(w, http.StatusInternalServerError, "save_failed", "The finding could not be added to the blocked game list.")
		}
		return
	}
	s.audit(model.AuditEvent{
		Type:     model.EventGame,
		Message:  "Game added from strange activity: " + game.Name,
		ActorIP:  clientIPString(r),
		GameID:   game.ID,
		GameName: game.Name,
		Result:   "added-from-strange",
	})
	if s.enforcer != nil {
		s.enforcer.Wake()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"game": game})
}
