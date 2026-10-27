// Package server exposes the LAN dashboard and authenticated control API.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

const (
	Port              = 8080
	SessionCookie     = "pc_session"
	CSRFHeader        = "X-CSRF-Token"
	maxRequestBody    = 1 << 20
	defaultEventLimit = 50
	maxEventLimit     = 500
)

//go:embed ui/*
var uiFiles embed.FS

// Enforcer is the control surface the API needs from the enforcement loop.
type Enforcer interface {
	Wake()
	Status() enforce.Status
}

// LaunchGuard is the Windows pre-launch block used for the mandatory Roblox
// protection. A nil guard is treated as unavailable.
type LaunchGuard interface {
	SetBlocked(blocked bool) error
	Active() (bool, error)
}

// Options configures a Server.
type Options struct {
	Address      string
	Version      string
	AllowedHosts []string
	Now          func() time.Time
	LaunchGuard  LaunchGuard
}

// Server holds process-wide dependencies. It is safe to construct once and
// serve concurrently.
type Server struct {
	store       *store.Store
	auth        *auth.Manager
	enforcer    Enforcer
	launchGuard LaunchGuard
	version     string
	address     string
	now         func() time.Time

	handler      http.Handler
	allowedHosts map[string]struct{}
	lanURLs      []string
}

// New creates the HTTP server and its route table.
func New(options Options, st *store.Store, authManager *auth.Manager, enforcer Enforcer) *Server {
	if options.Address == "" {
		options.Address = fmt.Sprintf("0.0.0.0:%d", Port)
	}
	if options.Version == "" {
		options.Version = "dev"
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	hosts, urls := discoverLocalAccess(options.Address)
	for _, host := range options.AllowedHosts {
		hosts[normalizeHost(host)] = struct{}{}
	}
	s := &Server{
		store:        st,
		auth:         authManager,
		enforcer:     enforcer,
		launchGuard:  options.LaunchGuard,
		version:      options.Version,
		address:      options.Address,
		now:          options.Now,
		allowedHosts: hosts,
		lanURLs:      urls,
	}
	s.handler = s.buildHandler()
	return s
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// LANURLs returns the dashboard URLs shown by the UI.
func (s *Server) LANURLs() []string {
	out := make([]string, len(s.lanURLs))
	copy(out, s.lanURLs)
	return out
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /setup", s.handleIndex)
	mux.HandleFunc("GET /assets/app.css", s.handleCSS)
	mux.HandleFunc("GET /assets/app.js", s.handleJS)

	mux.HandleFunc("GET /api/v1/bootstrap", s.handleBootstrap)
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
	mux.HandleFunc("POST /api/v1/session", s.handleLogin)
	mux.Handle("DELETE /api/v1/session", s.withMutation(s.handleLogout))
	mux.Handle("GET /api/v1/state", s.withAuth(s.handleState))
	mux.Handle("PUT /api/v1/play-state", s.withMutation(s.handlePlayState))
	mux.Handle("POST /api/v1/games", s.withMutation(s.handleAddGame))
	mux.Handle("DELETE /api/v1/games/{id}", s.withMutation(s.handleDeleteGame))
	mux.Handle("GET /api/v1/events", s.withAuth(s.handleEvents))
	mux.Handle("PUT /api/v1/pin", s.withMutation(s.handleChangePIN))

	return s.securityHeaders(s.networkGuard(mux))
}

type ctxKey int

const sessionContextKey ctxKey = 1

func sessionFromContext(ctx context.Context) (auth.Session, bool) {
	session, ok := ctx.Value(sessionContextKey).(auth.Session)
	return session, ok
}

func (s *Server) withAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := s.session(r)
		if err != nil {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized", "Administrator login required.")
			return
		}
		ctx := context.WithValue(r.Context(), sessionContextKey, session)
		next(w, r.WithContext(ctx))
	})
}

func (s *Server) withMutation(next http.HandlerFunc) http.Handler {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		session, _ := sessionFromContext(r.Context())
		provided := r.Header.Get(CSRFHeader)
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(session.CSRF)) != 1 {
			writeAPIError(w, http.StatusForbidden, "csrf_failed", "The request could not be verified. Reload the dashboard and try again.")
			return
		}
		if !s.validOrigin(r) {
			writeAPIError(w, http.StatusForbidden, "origin_rejected", "The request origin is not allowed.")
			return
		}
		next(w, r)
	})
}

func (s *Server) session(r *http.Request) (auth.Session, error) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" {
		return auth.Session{}, auth.ErrInvalidSession
	}
	session, err := s.auth.ValidateSession(cookie.Value)
	if err != nil {
		return auth.Session{}, err
	}
	return session, nil
}

func (s *Server) validOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; frame-src 'none'; worker-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Permitted-Cross-Domain-Policies", "none")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), serial=(), hid=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) networkGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := remoteIP(r.RemoteAddr)
		if !isPrivateOrLoopback(ip) {
			http.Error(w, "Access is restricted to the local network.", http.StatusForbidden)
			return
		}
		if !s.allowedHost(normalizeHost(r.Host)) {
			http.Error(w, "The requested host is not allowed.", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowedHost(host string) bool {
	_, ok := s.allowedHosts[host]
	return ok
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	for i := 0; i < len(host); i++ {
		if host[i] == '%' {
			host = host[:i]
			break
		}
	}
	return host
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

func isPrivateOrLoopback(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func isLoopbackRequest(r *http.Request) bool {
	ip := remoteIP(r.RemoteAddr)
	return ip != nil && ip.IsLoopback()
}

func discoverLocalAccess(address string) (map[string]struct{}, []string) {
	hosts := map[string]struct{}{
		"localhost": {},
		"127.0.0.1": {},
		"::1":       {},
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		hostname = strings.TrimSuffix(strings.ToLower(hostname), ".")
		hosts[hostname] = struct{}{}
		if !strings.Contains(hostname, ".") {
			hosts[hostname+".local"] = struct{}{}
		}
	}

	port := strconv.Itoa(Port)
	if _, p, err := net.SplitHostPort(address); err == nil && p != "" && p != "0" {
		port = p
	}

	var lan []string
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch value := addr.(type) {
				case *net.IPNet:
					ip = value.IP
				case *net.IPAddr:
					ip = value.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}
				ipStr := ip.String()
				hosts[normalizeHost(ipStr)] = struct{}{}
				if ip.To4() != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
					lan = append(lan, "http://"+ipStr+":"+port)
				}
			}
		}
	}
	sort.Strings(lan)
	lan = dedupeStrings(lan)
	if len(lan) == 0 {
		lan = []string{"http://127.0.0.1:" + port}
	}
	return hosts, lan
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.serveEmbedded(w, "ui/index.html", "text/html; charset=utf-8")
}

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	s.serveEmbedded(w, "ui/app.css", "text/css; charset=utf-8")
}

func (s *Server) handleJS(w http.ResponseWriter, r *http.Request) {
	s.serveEmbedded(w, "ui/app.js", "text/javascript; charset=utf-8")
}

func (s *Server) serveEmbedded(w http.ResponseWriter, name, contentType string) {
	data, err := uiFiles.ReadFile(name)
	if err != nil {
		http.Error(w, "Dashboard asset is unavailable.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiError{Code: code, Message: message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain exactly one JSON object")
	}
	return nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	// #nosec G124 -- HTTP is intentional on the trusted private LAN; Secure would prevent the cookie from being sent. See SECURITY.md.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((12 * time.Hour).Seconds()),
		Expires:  s.now().Add(12 * time.Hour).UTC(),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	// #nosec G124 -- Matching HTTP-only cookie policy; it has already expired, so Secure would not improve confidentiality.
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0).UTC(),
	})
}
