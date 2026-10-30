package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/protection"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

type bootstrapResponse struct {
	SetupRequired bool   `json:"setupRequired"`
	Version       string `json:"version"`
	LocalRequest  bool   `json:"localRequest"`
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, bootstrapResponse{
		SetupRequired: s.auth.SetupRequired(),
		Version:       s.version,
		LocalRequest:  isLoopbackRequest(r),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeAPIError(w, http.StatusForbidden, "local_only", "Health checks are available only on the local computer.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": s.version,
		"setup":   s.auth.SetupRequired(),
	})
}

type setupRequest struct {
	Token string `json:"token"`
	PIN   string `json:"pin"`
}

type sessionResponse struct {
	CSRFToken string `json:"csrfToken"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeAPIError(w, http.StatusForbidden, "local_only", "Initial setup must be completed on the gaming PC.")
		return
	}
	var req setupRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		writeAPIError(w, http.StatusBadRequest, "setup_token_required", "The one-time setup token is required.")
		return
	}
	if err := s.auth.Setup(req.Token, req.PIN); err != nil {
		switch {
		case errors.Is(err, auth.ErrPINFormat):
			writeAPIError(w, http.StatusBadRequest, "invalid_pin", "Choose a PIN with 6 to 12 digits.")
		case errors.Is(err, auth.ErrSetupToken):
			writeAPIError(w, http.StatusForbidden, "setup_token_invalid", "The setup token is invalid or expired. Run PlaygroundControl.exe as administrator and choose Set up PIN again.")
		case errors.Is(err, auth.ErrSetupComplete):
			writeAPIError(w, http.StatusConflict, "setup_complete", "The administrator PIN is already configured.")
		default:
			writeAPIError(w, http.StatusInternalServerError, "setup_failed", "Setup could not be saved.")
		}
		return
	}

	session, err := s.auth.NewSession()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "session_failed", "Setup succeeded, but a session could not be created.")
		return
	}
	s.setSessionCookie(w, session.Token)
	s.audit(model.AuditEvent{
		Type:    model.EventAuth,
		Message: "Initial administrator PIN configured.",
		ActorIP: clientIPString(r),
		Result:  "success",
	})
	writeJSON(w, http.StatusCreated, sessionResponse{CSRFToken: session.CSRF})
}

type loginRequest struct {
	PIN string `json:"pin"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ip := clientIPString(r)
	if remaining := s.auth.CheckLoginLock(ip); remaining > 0 {
		writeLoginLocked(w, remaining)
		return
	}
	session, err := s.auth.Login(ip, req.PIN)
	if err != nil {
		var locked *auth.LockedError
		switch {
		case errors.As(err, &locked):
			s.audit(model.AuditEvent{
				Type:    model.EventAuth,
				Message: "Administrator login blocked by rate limiting.",
				ActorIP: ip,
				Result:  "locked",
			})
			writeLoginLocked(w, locked.RetryAfter)
		case errors.Is(err, auth.ErrSetupRequired):
			writeAPIError(w, http.StatusConflict, "setup_required", "Complete initial setup on the gaming PC first.")
		default:
			s.audit(model.AuditEvent{
				Type:    model.EventAuth,
				Message: "Administrator login failed.",
				ActorIP: ip,
				Result:  "invalid",
			})
			writeAPIError(w, http.StatusUnauthorized, "invalid_pin", "Incorrect PIN.")
		}
		return
	}
	s.setSessionCookie(w, session.Token)
	s.audit(model.AuditEvent{
		Type:    model.EventAuth,
		Message: "Administrator signed in.",
		ActorIP: ip,
		Result:  "success",
	})
	writeJSON(w, http.StatusOK, sessionResponse{CSRFToken: session.CSRF})
}

func writeLoginLocked(w http.ResponseWriter, remaining time.Duration) {
	seconds := int(remaining.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeAPIError(w, http.StatusTooManyRequests, "login_locked", "Too many failed attempts. Try again later.")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if session, ok := sessionFromContext(r.Context()); ok {
		s.auth.Logout(session.Token)
		s.audit(model.AuditEvent{
			Type:    model.EventAuth,
			Message: "Administrator signed out.",
			ActorIP: clientIPString(r),
			Result:  "success",
		})
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusNoContent, nil)
}

type browserSummary struct {
	Settings     model.BrowserSettings `json:"settings"`
	Status       model.BrowserStatus   `json:"status"`
	BlockedCount int                   `json:"blockedCount"`
	ReviewCount  int                   `json:"reviewCount"`
	AllowedCount int                   `json:"allowedCount"`
}

type stateResponse struct {
	PlayAllowed         bool              `json:"playAllowed"`
	Games               []model.Game      `json:"games"`
	BuiltinGames        []model.Game      `json:"builtinGames"`
	LaunchGuardActive   bool              `json:"launchGuardActive"`
	RunningBlockedGames []enforce.Match   `json:"runningBlockedGames"`
	PendingClosures     []enforce.Pending `json:"pendingClosures"`
	LANURLs             []string          `json:"lanUrls"`
	LastChangedAt       time.Time         `json:"lastChangedAt"`
	Version             string            `json:"version"`
	CSRFToken           string            `json:"csrfToken"`
	Warnings            []string          `json:"warnings"`
	AI                  model.AISettings  `json:"ai"`
	Browser             browserSummary    `json:"browser"`
}

func (s *Server) state(session auth.Session) stateResponse {
	cfg := s.store.Config()
	aiSettings := s.store.AISettings()
	status := enforce.Status{
		RunningBlockedGames: []enforce.Match{},
		PendingClosures:     []enforce.Pending{},
	}
	if s.enforcer != nil {
		status = s.enforcer.Status()
		if status.RunningBlockedGames == nil {
			status.RunningBlockedGames = []enforce.Match{}
		}
		if status.PendingClosures == nil {
			status.PendingClosures = []enforce.Pending{}
		}
	}
	warnings := []string{}
	if notice := s.store.LoadNotice(); notice != "" {
		warnings = append(warnings, notice)
	}
	guardActive := false
	if s.launchGuard != nil {
		var err error
		guardActive, err = s.launchGuard.Active()
		if err != nil {
			warnings = append(warnings, "The Roblox launch guard status could not be checked: "+err.Error())
		}
	}
	if !cfg.PlayAllowed && !guardActive {
		warnings = append(warnings, "The Roblox pre-launch guard is not active. Restart the Playground Control service, or turn play on and block it again.")
	}
	if cfg.PlayAllowed && guardActive {
		warnings = append(warnings, "The Roblox pre-launch guard is unexpectedly active. Roblox may remain blocked until the service is restarted.")
	}
	if aiSettings.Enabled && !aiSettings.KeyConfigured {
		warnings = append(warnings, "AI monitoring is enabled but no usable DeepSeek API key is configured.")
	}
	if aiSettings.Enabled && aiSettings.LastError != "" {
		warnings = append(warnings, "AI monitoring is reporting an error: "+aiSettings.LastError)
	}
	browserSettings := s.store.BrowserSettings()
	browserStatus := model.BrowserStatus{Browsers: []string{}}
	if s.browser != nil {
		browserStatus = s.browser.Status()
	}
	blockedCount, reviewCount, allowedCount, countsErr := s.store.BrowserDomainCounts()
	if countsErr != nil {
		warnings = append(warnings, "Browser domain counts could not be read: "+countsErr.Error())
	}
	if browserSettings.Enabled && browserSettings.LastError != "" {
		warnings = append(warnings, "Browser monitoring is reporting an error: "+browserSettings.LastError)
	}
	if browserSettings.Enabled && browserStatus.LastError != "" && browserStatus.LastError != browserSettings.LastError {
		warnings = append(warnings, "Browser controls are reporting an error: "+browserStatus.LastError)
	}
	if browserSettings.Enabled && len(browserStatus.Browsers) == 0 {
		warnings = append(warnings, "Browser monitoring is enabled, but Chrome or Edge was not detected on this PC.")
	}
	if browserSettings.Enabled && !cfg.PlayAllowed && blockedCount > 0 &&
		(!browserStatus.URLBlocklistApplied || !browserStatus.HostsApplied) {
		warnings = append(warnings, "Browser game blocking is not fully active. The service will retry; check the browser panel for details.")
	}
	if (!browserSettings.Enabled || cfg.PlayAllowed) &&
		(browserStatus.URLBlocklistApplied || browserStatus.HostsApplied || browserStatus.DoHDisabled) {
		warnings = append(warnings, "Browser controls are still active although browser blocking should be off. The service will retry; check the browser panel for details.")
	}
	if browserSettings.Enabled && browserSettings.AIClassification && !browserSettings.KeyConfigured {
		warnings = append(warnings, "Browser AI classification is enabled but no usable DeepSeek API key is configured.")
	}
	if blockedCount >= store.MaxBrowserDomains {
		warnings = append(warnings, "The blocked browser domain limit has been reached; additional detections are not enforced until another domain is allowed.")
	}
	if browserStatus.DriftDetected {
		warnings = append(warnings, "Browser policies or hosts entries were modified outside Playground Control and were repaired.")
	}
	games := cfg.Games
	if games == nil {
		games = []model.Game{}
	}
	return stateResponse{
		PlayAllowed:         cfg.PlayAllowed,
		Games:               games,
		BuiltinGames:        protection.RobloxGames(),
		LaunchGuardActive:   guardActive,
		RunningBlockedGames: status.RunningBlockedGames,
		PendingClosures:     status.PendingClosures,
		LANURLs:             s.LANURLs(),
		LastChangedAt:       cfg.LastChangedAt,
		Version:             s.version,
		CSRFToken:           session.CSRF,
		Warnings:            warnings,
		AI:                  aiSettings,
		Browser: browserSummary{
			Settings:     browserSettings,
			Status:       browserStatus,
			BlockedCount: blockedCount,
			ReviewCount:  reviewCount,
			AllowedCount: allowedCount,
		},
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	writeJSON(w, http.StatusOK, s.state(session))
}

type playStateRequest struct {
	Allowed *bool `json:"allowed"`
}

func (s *Server) handlePlayState(w http.ResponseWriter, r *http.Request) {
	var req playStateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Allowed == nil {
		writeAPIError(w, http.StatusBadRequest, "allowed_required", "The allowed field is required.")
		return
	}
	session, _ := sessionFromContext(r.Context())
	ip := clientIPString(r)
	previous := s.store.PlayAllowed()
	targetBlocked := !*req.Allowed

	// Roblox must be stopped at process creation, not merely killed after it
	// opens. Install/remove the pre-launch guard before committing the flag so
	// a failure cannot silently report protection that is not active.
	if s.launchGuard == nil {
		writeAPIError(w, http.StatusInternalServerError, "launch_guard_unavailable", "The Roblox launch guard is not available in this service instance.")
		return
	}
	if err := s.launchGuard.SetBlocked(targetBlocked); err != nil {
		if revertErr := s.launchGuard.SetBlocked(!previous); revertErr != nil {
			s.audit(model.AuditEvent{
				Type:    model.EventSystem,
				Message: "Guard update failed and the previous guard state could not be restored: " + revertErr.Error(),
				ActorIP: ip,
				Result:  "guard-revert-failed",
			})
		}
		s.audit(model.AuditEvent{
			Type:    model.EventSystem,
			Message: "Failed to update the Roblox pre-launch guard: " + err.Error(),
			ActorIP: ip,
			Result:  "guard-failed",
		})
		writeAPIError(w, http.StatusInternalServerError, "launch_guard_failed", "The Roblox pre-launch guard could not be updated. The play state was not changed.")
		return
	}
	if err := s.store.SetPlayAllowed(*req.Allowed, ip); err != nil {
		if revertErr := s.launchGuard.SetBlocked(!previous); revertErr != nil {
			s.audit(model.AuditEvent{
				Type:    model.EventSystem,
				Message: "Permission save failed and the Roblox guard could not be reverted: " + revertErr.Error(),
				ActorIP: ip,
				Result:  "guard-revert-failed",
			})
		}
		writeAPIError(w, http.StatusInternalServerError, "save_failed", "The permission change could not be saved.")
		return
	}
	message := "Play permission revoked; Roblox is launch-blocked and configured games will be closed."
	if *req.Allowed {
		message = "Play permission granted; the Roblox launch guard is disarmed."
	}
	s.audit(model.AuditEvent{
		Type:    model.EventPermission,
		Message: message,
		ActorIP: ip,
		Result:  "success",
	})
	if s.enforcer != nil {
		s.enforcer.Wake()
	}
	if s.ai != nil {
		s.ai.Wake()
	}
	if s.browser != nil {
		_ = s.browser.Sync()
		s.browser.Wake()
	}
	writeJSON(w, http.StatusOK, s.state(session))
}

type addGameRequest struct {
	Name       string `json:"name"`
	Executable string `json:"executable"`
	ImagePath  string `json:"imagePath"`
}

func (s *Server) handleAddGame(w http.ResponseWriter, r *http.Request) {
	var req addGameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	candidate, err := model.NormalizeGame(model.Game{
		Name:       req.Name,
		Executable: req.Executable,
		ImagePath:  req.ImagePath,
	})
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_game", err.Error())
		return
	}
	game, err := s.store.AddGame(candidate)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateGame) {
			writeAPIError(w, http.StatusConflict, "duplicate_game", err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "save_failed", "The game could not be saved.")
		return
	}
	s.audit(model.AuditEvent{
		Type:     model.EventGame,
		Message:  "Game added: " + game.Name,
		ActorIP:  clientIPString(r),
		GameID:   game.ID,
		GameName: game.Name,
		Result:   "added",
	})
	if s.enforcer != nil {
		s.enforcer.Wake()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"game": game})
}

func (s *Server) handleDeleteGame(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_game_id", "A game ID is required.")
		return
	}
	cfg := s.store.Config()
	var found *model.Game
	for i := range cfg.Games {
		if cfg.Games[i].ID == id {
			found = &cfg.Games[i]
			break
		}
	}
	if found == nil {
		writeAPIError(w, http.StatusNotFound, "game_not_found", "That game is not configured.")
		return
	}
	removed, err := s.store.DeleteGame(id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save_failed", "The game could not be removed.")
		return
	}
	if !removed {
		writeAPIError(w, http.StatusNotFound, "game_not_found", "That game is not configured.")
		return
	}
	message := "Game removed: " + found.Name
	result := "removed"
	if found.Source == model.GameSourceAI {
		message = "AI-added game removed; AI will ignore it: " + found.Name
		result = "removed-and-ignored"
	}
	s.audit(model.AuditEvent{
		Type:     model.EventGame,
		Message:  message,
		ActorIP:  clientIPString(r),
		GameID:   found.ID,
		GameName: found.Name,
		Result:   result,
	})
	if s.enforcer != nil {
		s.enforcer.Wake()
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := defaultEventLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_limit", "Limit must be a positive integer.")
			return
		}
		limit = parsed
		if limit > maxEventLimit {
			limit = maxEventLimit
		}
	}
	events, err := s.store.RecentEvents(limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "events_failed", "Activity could not be read.")
		return
	}
	if events == nil {
		events = []model.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

type changePINRequest struct {
	CurrentPIN string `json:"currentPin"`
	NewPIN     string `json:"newPin"`
}

func (s *Server) handleChangePIN(w http.ResponseWriter, r *http.Request) {
	var req changePINRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	session, _ := sessionFromContext(r.Context())
	err := s.auth.ChangePIN(session.Token, req.CurrentPIN, req.NewPIN)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrPINFormat):
			writeAPIError(w, http.StatusBadRequest, "invalid_pin", "Choose a PIN with 6 to 12 digits.")
		case errors.Is(err, auth.ErrInvalidPIN):
			s.audit(model.AuditEvent{
				Type:    model.EventAuth,
				Message: "PIN change failed: current PIN was incorrect.",
				ActorIP: clientIPString(r),
				Result:  "invalid",
			})
			writeAPIError(w, http.StatusForbidden, "invalid_current_pin", "The current PIN is incorrect.")
		default:
			writeAPIError(w, http.StatusInternalServerError, "pin_change_failed", "The PIN could not be changed.")
		}
		return
	}
	s.audit(model.AuditEvent{
		Type:    model.EventAuth,
		Message: "Administrator PIN changed.",
		ActorIP: clientIPString(r),
		Result:  "success",
	})
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Server) audit(event model.AuditEvent) {
	_ = s.store.AddEvent(event)
}

func clientIPString(r *http.Request) string {
	ip := remoteIP(r.RemoteAddr)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}
