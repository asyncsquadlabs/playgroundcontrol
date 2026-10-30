package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/auth"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/enforce"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
	"github.com/asyncsquadlabs/playgroundcontrol/internal/store"
)

type fakeEnforcer struct {
	mu        sync.Mutex
	wakeCount int
	status    enforce.Status
}

func (f *fakeEnforcer) Wake() {
	f.mu.Lock()
	f.wakeCount++
	f.mu.Unlock()
}

func (f *fakeEnforcer) Status() enforce.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeEnforcer) wakes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakeCount
}

type fakeLaunchGuard struct {
	mu      sync.Mutex
	blocked bool
	calls   []bool
	err     error
}

func (f *fakeLaunchGuard) SetBlocked(blocked bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, blocked)
	if f.err != nil {
		return f.err
	}
	f.blocked = blocked
	return nil
}

func (f *fakeLaunchGuard) setError(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeLaunchGuard) Active() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.blocked, nil
}

func (f *fakeLaunchGuard) callsSnapshot() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

type fakeAI struct {
	mu        sync.Mutex
	testCalls int
	wakeCount int
	latency   time.Duration
	err       error
}

func (f *fakeAI) Test(_ context.Context) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testCalls++
	return f.latency, f.err
}

func (f *fakeAI) Wake() {
	f.mu.Lock()
	f.wakeCount++
	f.mu.Unlock()
}

func (f *fakeAI) counts() (tests, wakes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.testCalls, f.wakeCount
}

type testApp struct {
	server   *Server
	http     *httptest.Server
	client   *http.Client
	auth     *auth.Manager
	store    *store.Store
	enforcer *fakeEnforcer
	guard    *fakeLaunchGuard
	ai       *fakeAI
	dataDir  string
}

func newTestApp(t *testing.T, configure bool) *testApp {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authManager, err := auth.Open(dir, st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := authManager.Prepare(); err != nil {
		t.Fatal(err)
	}
	if configure {
		raw, err := os.ReadFile(authManager.SetupTokenPath())
		if err != nil {
			t.Fatal(err)
		}
		if err := authManager.Setup(strings.TrimSpace(string(raw)), "123456"); err != nil {
			t.Fatal(err)
		}
	}
	enforcer := &fakeEnforcer{status: enforce.Status{
		RunningBlockedGames: []enforce.Match{},
		PendingClosures:     []enforce.Pending{},
	}}
	guard := &fakeLaunchGuard{blocked: true}
	ai := &fakeAI{latency: 25 * time.Millisecond}
	httpAPI := New(Options{
		Address:     "127.0.0.1:8080",
		Version:     "test-version",
		LaunchGuard: guard,
		AI:          ai,
	}, st, authManager, enforcer)
	ts := httptest.NewServer(httpAPI.Handler())
	t.Cleanup(ts.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &testApp{
		server:   httpAPI,
		http:     ts,
		client:   &http.Client{Jar: jar, Timeout: 5 * time.Second},
		auth:     authManager,
		store:    st,
		enforcer: enforcer,
		guard:    guard,
		ai:       ai,
		dataDir:  dir,
	}
}

type apiResult struct {
	status  int
	body    []byte
	headers http.Header
	cookies []*http.Cookie
}

func (a *testApp) request(t *testing.T, method, path string, body any, csrf, origin string) apiResult {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.http.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if csrf != "" {
		req.Header.Set(CSRFHeader, csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return apiResult{
		status:  resp.StatusCode,
		body:    data,
		headers: resp.Header.Clone(),
		cookies: resp.Cookies(),
	}
}

func (a *testApp) login(t *testing.T, pin string) string {
	t.Helper()
	result := a.request(t, http.MethodPost, "/api/v1/session", map[string]string{"pin": pin}, "", "")
	if result.status != http.StatusOK {
		t.Fatalf("login status = %d body=%s", result.status, result.body)
	}
	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(result.body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CSRFToken == "" {
		t.Fatal("login returned an empty CSRF token")
	}
	return payload.CSRFToken
}

func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return value
}

func TestBootstrapAndLoginFlow(t *testing.T) {
	app := newTestApp(t, true)

	result := app.request(t, http.MethodGet, "/api/v1/bootstrap", nil, "", "")
	if result.status != http.StatusOK {
		t.Fatalf("bootstrap status = %d", result.status)
	}
	bootstrap := decodeMap(t, result.body)
	if bootstrap["setupRequired"] != false || bootstrap["version"] != "test-version" || bootstrap["localRequest"] != true {
		t.Fatalf("unexpected bootstrap: %+v", bootstrap)
	}

	if result := app.request(t, http.MethodGet, "/api/v1/health", nil, "", ""); result.status != http.StatusOK {
		t.Fatalf("health status = %d", result.status)
	}
	if result := app.request(t, http.MethodGet, "/api/v1/state", nil, "", ""); result.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated state status = %d", result.status)
	}

	if result := app.request(t, http.MethodPost, "/api/v1/session", map[string]string{"pin": "000000"}, "", ""); result.status != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d", result.status)
	}
	csrf := app.login(t, "123456")
	if csrf == "" {
		t.Fatal("empty CSRF")
	}
	result = app.request(t, http.MethodGet, "/api/v1/state", nil, "", "")
	if result.status != http.StatusOK {
		t.Fatalf("state status = %d", result.status)
	}
	state := decodeMap(t, result.body)
	if state["playAllowed"] != false {
		t.Fatalf("unexpected initial state: %+v", state)
	}
	if _, ok := state["games"].([]any); !ok {
		t.Fatalf("games is not an array: %+v", state["games"])
	}
	if state["csrfToken"] != csrf {
		t.Fatal("state did not return the session CSRF token")
	}
}

func TestSetupEndpointAndCookie(t *testing.T) {
	app := newTestApp(t, false)
	raw, err := os.ReadFile(app.auth.SetupTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))

	if result := app.request(t, http.MethodPost, "/api/v1/setup", map[string]string{"token": "wrong", "pin": "123456"}, "", ""); result.status != http.StatusForbidden {
		t.Fatalf("wrong setup token status = %d", result.status)
	}
	result := app.request(t, http.MethodPost, "/api/v1/setup", map[string]string{"token": token, "pin": "123456"}, "", "")
	if result.status != http.StatusCreated {
		t.Fatalf("setup status = %d body=%s", result.status, result.body)
	}
	if len(result.cookies) != 1 {
		t.Fatalf("setup cookies = %+v", result.cookies)
	}
	cookie := result.cookies[0]
	if cookie.Name != SessionCookie || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %+v", cookie)
	}
	var setupResponse struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(result.body, &setupResponse); err != nil {
		t.Fatal(err)
	}
	if setupResponse.CSRFToken == "" {
		t.Fatal("setup did not return a CSRF token")
	}
	if result := app.request(t, http.MethodGet, "/api/v1/state", nil, "", ""); result.status != http.StatusOK {
		t.Fatalf("state after setup status = %d", result.status)
	}
	if _, err := os.Stat(app.auth.SetupTokenPath()); !os.IsNotExist(err) {
		t.Fatalf("setup token file still exists: %v", err)
	}

	result = app.request(t, http.MethodPost, "/api/v1/setup", map[string]string{"token": token, "pin": "654321"}, "", "")
	if result.status != http.StatusConflict {
		t.Fatalf("repeated setup status = %d body=%s", result.status, result.body)
	}
}

func TestPlayStateGamesAndAuthorization(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	result := app.request(t, http.MethodPut, "/api/v1/play-state", map[string]bool{"allowed": true}, "", "")
	if result.status != http.StatusForbidden {
		t.Fatalf("play toggle without CSRF status = %d", result.status)
	}
	result = app.request(t, http.MethodPut, "/api/v1/play-state", map[string]bool{"allowed": true}, csrf, "http://evil.example")
	if result.status != http.StatusForbidden {
		t.Fatalf("play toggle with bad origin status = %d", result.status)
	}
	result = app.request(t, http.MethodPut, "/api/v1/play-state", map[string]bool{"allowed": true}, csrf, "")
	if result.status != http.StatusOK {
		t.Fatalf("play toggle status = %d body=%s", result.status, result.body)
	}
	if !app.store.PlayAllowed() {
		t.Fatal("play permission was not persisted")
	}
	if app.enforcer.wakes() == 0 {
		t.Fatal("enforcement engine was not woken")
	}
	calls := app.guard.callsSnapshot()
	if len(calls) != 1 || calls[0] != false {
		t.Fatalf("launch guard calls = %+v, want [false]", calls)
	}
	state := decodeMap(t, result.body)
	if state["launchGuardActive"] != false {
		t.Fatalf("launch guard should be disarmed while play is allowed: %+v", state)
	}
	builtins, ok := state["builtinGames"].([]any)
	if !ok || len(builtins) == 0 {
		t.Fatalf("built-in Roblox games missing from state: %+v", state)
	}

	result = app.request(t, http.MethodPut, "/api/v1/play-state", map[string]bool{"allowed": false}, csrf, "")
	if result.status != http.StatusOK {
		t.Fatalf("block toggle status = %d body=%s", result.status, result.body)
	}
	calls = app.guard.callsSnapshot()
	if len(calls) != 2 || calls[1] != true {
		t.Fatalf("launch guard calls after block = %+v, want [false true]", calls)
	}
	if app.store.PlayAllowed() {
		t.Fatal("block toggle was not persisted")
	}

	result = app.request(t, http.MethodPost, "/api/v1/games", map[string]string{
		"name": "Explorer", "executable": "explorer.exe",
	}, csrf, "")
	if result.status != http.StatusBadRequest {
		t.Fatalf("protected game status = %d body=%s", result.status, result.body)
	}

	result = app.request(t, http.MethodPost, "/api/v1/games", map[string]string{
		"name": "Example Game", "executable": "game.exe",
	}, csrf, "")
	if result.status != http.StatusCreated {
		t.Fatalf("add game status = %d body=%s", result.status, result.body)
	}
	added := decodeMap(t, result.body)
	game := added["game"].(map[string]any)
	gameID := game["id"].(string)
	if gameID == "" {
		t.Fatal("game ID was empty")
	}
	if result := app.request(t, http.MethodPost, "/api/v1/games", map[string]string{
		"name": "Duplicate", "executable": "GAME.EXE",
	}, csrf, ""); result.status != http.StatusConflict {
		t.Fatalf("duplicate game status = %d body=%s", result.status, result.body)
	}
	result = app.request(t, http.MethodDelete, "/api/v1/games/"+gameID, nil, csrf, "")
	if result.status != http.StatusNoContent {
		t.Fatalf("delete game status = %d body=%s", result.status, result.body)
	}
	if len(app.store.Games()) != 0 {
		t.Fatal("game was not deleted from persistence")
	}

	result = app.request(t, http.MethodGet, "/api/v1/events?limit=20", nil, "", "")
	if result.status != http.StatusOK {
		t.Fatalf("events status = %d", result.status)
	}
	eventsPayload := decodeMap(t, result.body)
	if _, ok := eventsPayload["events"].([]any); !ok {
		t.Fatalf("events response malformed: %+v", eventsPayload)
	}
	if result := app.request(t, http.MethodGet, "/api/v1/events?limit=bad", nil, "", ""); result.status != http.StatusBadRequest {
		t.Fatalf("bad event limit status = %d", result.status)
	}
}

func TestPlayStateFailsClosedWhenLaunchGuardFails(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	result := app.request(t, http.MethodPut, "/api/v1/play-state", map[string]bool{"allowed": true}, csrf, "")
	if result.status != http.StatusOK {
		t.Fatalf("allow play status = %d body=%s", result.status, result.body)
	}
	app.guard.setError(errors.New("registry access denied"))
	result = app.request(t, http.MethodPut, "/api/v1/play-state", map[string]bool{"allowed": false}, csrf, "")
	if result.status != http.StatusInternalServerError {
		t.Fatalf("guard failure status = %d body=%s", result.status, result.body)
	}
	if !app.store.PlayAllowed() {
		t.Fatal("play state changed even though the launch guard failed")
	}
	events := app.request(t, http.MethodGet, "/api/v1/events?limit=20", nil, "", "")
	if events.status != http.StatusOK || !bytes.Contains(events.body, []byte("guard-failed")) {
		t.Fatalf("guard failure was not audited: status=%d body=%s", events.status, events.body)
	}
}

func TestPINChangeInvalidatesOtherSessions(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	secondJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	secondClient := &http.Client{Jar: secondJar, Timeout: 5 * time.Second}
	secondRequest, _ := http.NewRequest(http.MethodPost, app.http.URL+"/api/v1/session", strings.NewReader(`{"pin":"123456"}`))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse, err := secondClient.Do(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondResponse.Body.Close()
	if secondResponse.StatusCode != http.StatusOK {
		t.Fatalf("second login status = %d", secondResponse.StatusCode)
	}

	if result := app.request(t, http.MethodPut, "/api/v1/pin", map[string]string{
		"currentPin": "000000", "newPin": "654321",
	}, csrf, ""); result.status != http.StatusForbidden {
		t.Fatalf("wrong current PIN status = %d body=%s", result.status, result.body)
	}
	result := app.request(t, http.MethodPut, "/api/v1/pin", map[string]string{
		"currentPin": "123456", "newPin": "654321",
	}, csrf, "")
	if result.status != http.StatusNoContent {
		t.Fatalf("PIN change status = %d body=%s", result.status, result.body)
	}
	if result := app.request(t, http.MethodGet, "/api/v1/state", nil, "", ""); result.status != http.StatusOK {
		t.Fatalf("current session was invalidated: %d", result.status)
	}
	stateRequest, _ := http.NewRequest(http.MethodGet, app.http.URL+"/api/v1/state", nil)
	stateResponse, err := secondClient.Do(stateRequest)
	if err != nil {
		t.Fatal(err)
	}
	stateResponse.Body.Close()
	if stateResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session status = %d", stateResponse.StatusCode)
	}

	result = app.request(t, http.MethodDelete, "/api/v1/session", nil, csrf, "")
	if result.status != http.StatusNoContent {
		t.Fatalf("logout status = %d", result.status)
	}
	if result := app.request(t, http.MethodGet, "/api/v1/state", nil, "", ""); result.status != http.StatusUnauthorized {
		t.Fatalf("state after logout status = %d", result.status)
	}
}

func TestNetworkAndHostGuards(t *testing.T) {
	app := newTestApp(t, true)
	handler := app.server.Handler()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v1/bootstrap", nil)
	req.RemoteAddr = "8.8.8.8:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("public source status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://evil.example/api/v1/bootstrap", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "evil.example"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("bad host status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/v1/health", nil)
	req.RemoteAddr = "192.168.1.20:12345"
	req.Host = "127.0.0.1:8080"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("LAN health status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/v1/setup", strings.NewReader(`{"token":"x","pin":"123456"}`))
	req.RemoteAddr = "192.168.1.20:12345"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("LAN setup status = %d", recorder.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	app := newTestApp(t, true)
	result := app.request(t, http.MethodGet, "/api/v1/bootstrap", nil, "", "")
	if !strings.Contains(result.headers.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("missing CSP: %q", result.headers.Get("Content-Security-Policy"))
	}
	if result.headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}
	if result.headers.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing frame protection")
	}
	csp := result.headers.Get("Content-Security-Policy")
	for _, directive := range []string{"object-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("CSP missing %q: %q", directive, csp)
		}
	}
	if result.headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing referrer policy")
	}
	if result.headers.Get("Permissions-Policy") == "" {
		t.Fatal("missing permissions policy")
	}
	if result.headers.Get("Cross-Origin-Opener-Policy") != "same-origin" {
		t.Fatal("missing cross-origin opener policy")
	}
}

func TestStaticDashboardIsServed(t *testing.T) {
	app := newTestApp(t, true)
	result := app.request(t, http.MethodGet, "/", nil, "", "")
	if result.status != http.StatusOK || !bytes.Contains(result.body, []byte("Playground Control")) {
		t.Fatalf("dashboard root status=%d", result.status)
	}
	result = app.request(t, http.MethodGet, "/assets/app.js", nil, "", "")
	if result.status != http.StatusOK || len(result.body) == 0 {
		t.Fatalf("app.js status=%d", result.status)
	}
}

func TestPINPersistsAcrossApplicationRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	firstAuth, err := auth.Open(dir, st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstAuth.Prepare(); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(firstAuth.SetupTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := firstAuth.Setup(strings.TrimSpace(string(token)), "123456"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reloadedStore, err := store.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloadedStore.Close() })
	reloadedAuth, err := auth.Open(dir, reloadedStore, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedAuth.SetupRequired() {
		t.Fatal("PIN did not survive application restart")
	}
	if _, err := reloadedAuth.Login("127.0.0.1", "123456"); err != nil {
		t.Fatalf("login with persisted PIN: %v", err)
	}

	api := New(Options{
		Address:     "127.0.0.1:8080",
		Version:     "test-version",
		LaunchGuard: &fakeLaunchGuard{blocked: true},
	}, reloadedStore, reloadedAuth, &fakeEnforcer{status: enforce.Status{
		RunningBlockedGames: []enforce.Match{},
		PendingClosures:     []enforce.Pending{},
	}})
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	response, err := http.Get(ts.URL + "/api/v1/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var bootstrap map[string]any
	if err := json.NewDecoder(response.Body).Decode(&bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap["setupRequired"] != false {
		t.Fatalf("bootstrap after restart = %+v", bootstrap)
	}
}

func TestAISettingsEndpointsNeverExposeKey(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	result := app.request(t, http.MethodPut, "/api/v1/ai-settings", map[string]any{
		"enabled": true, "autoBlock": true, "model": "deepseek-flash", "apiKey": "sk-test",
	}, "", "")
	if result.status != http.StatusForbidden {
		t.Fatalf("AI settings without CSRF status = %d", result.status)
	}

	result = app.request(t, http.MethodPut, "/api/v1/ai-settings", map[string]any{
		"enabled": true, "autoBlock": true, "model": "deepseek-flash",
	}, csrf, "")
	if result.status != http.StatusBadRequest || !bytes.Contains(result.body, []byte("api_key_required")) {
		t.Fatalf("enable without key status = %d body=%s", result.status, result.body)
	}

	const testValue = "distinctive-test-key-1234"
	result = app.request(t, http.MethodPut, "/api/v1/ai-settings", map[string]any{
		"enabled": true, "autoBlock": true, "model": "deepseek-flash", "apiKey": testValue,
	}, csrf, "")
	if result.status != http.StatusOK {
		t.Fatalf("AI settings status = %d body=%s", result.status, result.body)
	}
	if bytes.Contains(result.body, []byte(testValue)) {
		t.Fatal("AI settings response contained the API key")
	}

	result = app.request(t, http.MethodGet, "/api/v1/state", nil, "", "")
	if result.status != http.StatusOK {
		t.Fatalf("state status = %d body=%s", result.status, result.body)
	}
	if bytes.Contains(result.body, []byte(testValue)) {
		t.Fatal("state response contained the API key")
	}
	state := decodeMap(t, result.body)
	ai, ok := state["ai"].(map[string]any)
	if !ok || ai["enabled"] != true || ai["autoBlock"] != true || ai["keyConfigured"] != true || ai["keyHint"] != "1234" {
		t.Fatalf("unexpected AI view: %+v", state["ai"])
	}
	if _, exists := ai["apiKey"]; exists {
		t.Fatal("AI view exposed an apiKey field")
	}

	result = app.request(t, http.MethodPost, "/api/v1/ai-settings/test", nil, csrf, "")
	if result.status != http.StatusOK || !bytes.Contains(result.body, []byte(`"latencyMs":25`)) {
		t.Fatalf("AI test status = %d body=%s", result.status, result.body)
	}
	if tests, _ := app.ai.counts(); tests != 1 {
		t.Fatalf("AI test calls = %d, want 1", tests)
	}

	app.ai.mu.Lock()
	app.ai.err = errors.New("provider unavailable")
	app.ai.mu.Unlock()
	result = app.request(t, http.MethodPost, "/api/v1/ai-settings/test", nil, csrf, "")
	if result.status != http.StatusBadGateway {
		t.Fatalf("failed AI test status = %d body=%s", result.status, result.body)
	}
	if bytes.Contains(result.body, []byte(testValue)) {
		t.Fatal("AI test failure response contained the API key")
	}

	result = app.request(t, http.MethodPut, "/api/v1/ai-settings", map[string]any{
		"enabled": true, "autoBlock": true, "model": "deepseek-flash", "clearApiKey": true,
	}, csrf, "")
	if result.status != http.StatusBadRequest {
		t.Fatalf("clear while enabled status = %d body=%s", result.status, result.body)
	}
	result = app.request(t, http.MethodPut, "/api/v1/ai-settings", map[string]any{
		"enabled": false, "autoBlock": true, "model": "deepseek-flash", "clearApiKey": true,
	}, csrf, "")
	if result.status != http.StatusOK {
		t.Fatalf("clear key status = %d body=%s", result.status, result.body)
	}
	state = decodeMap(t, app.request(t, http.MethodGet, "/api/v1/state", nil, "", "").body)
	ai = state["ai"].(map[string]any)
	if ai["keyConfigured"] != false {
		t.Fatalf("key remained configured: %+v", ai)
	}
}

func TestStrangeActivityEndpointsPromoteAndIgnore(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	first, err := app.store.RecordStrangeActivity(model.StrangeActivity{
		Name:       "Finding One",
		Executable: "one.exe",
		ImagePath:  `C:\Games\one.exe`,
		Confidence: 0.93,
		Reason:     "Interactive game client.",
		Model:      model.DefaultAIModel,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.store.RecordStrangeActivity(model.StrangeActivity{
		Name:       "Finding Two",
		Executable: "two.exe",
		ImagePath:  `C:\Games\two.exe`,
		Confidence: 0.88,
		Reason:     "Game emulator.",
		Model:      model.DefaultAIModel,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := app.request(t, http.MethodGet, "/api/v1/strange-activity?limit=50", nil, "", "")
	if result.status != http.StatusOK {
		t.Fatalf("activity status = %d body=%s", result.status, result.body)
	}
	payload := decodeMap(t, result.body)
	activity, ok := payload["activity"].([]any)
	if !ok || len(activity) != 2 {
		t.Fatalf("unexpected activity: %+v", payload)
	}

	result = app.request(t, http.MethodPost, "/api/v1/strange-activity/"+first.ID+"/block", nil, csrf, "")
	if result.status != http.StatusCreated {
		t.Fatalf("promote status = %d body=%s", result.status, result.body)
	}
	if len(app.store.Games()) != 1 {
		t.Fatalf("game was not promoted: %+v", app.store.Games())
	}
	if tests, wakes := app.ai.counts(); tests != 0 || wakes != 0 {
		t.Fatalf("promotion unexpectedly touched AI: tests=%d wakes=%d", tests, wakes)
	}

	result = app.request(t, http.MethodDelete, "/api/v1/strange-activity/"+second.ID, nil, csrf, "")
	if result.status != http.StatusNoContent {
		t.Fatalf("ignore status = %d body=%s", result.status, result.body)
	}
	ignored, err := app.store.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignored[model.StrangeActivityKey(second.Executable, second.ImagePath)]; !ok {
		t.Fatal("ignored finding did not create an AI ignore entry")
	}
	result = app.request(t, http.MethodDelete, "/api/v1/strange-activity/"+second.ID, nil, csrf, "")
	if result.status != http.StatusNotFound {
		t.Fatalf("repeat ignore status = %d body=%s", result.status, result.body)
	}
	result = app.request(t, http.MethodGet, "/api/v1/strange-activity?limit=50", nil, "", "")
	payload = decodeMap(t, result.body)
	activity, ok = payload["activity"].([]any)
	if !ok || len(activity) != 0 {
		t.Fatalf("activity was not cleared: %+v", payload)
	}
}

func TestDeleteAIGameCreatesIgnoreEntry(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")
	if _, err := app.store.SaveAISettings(true, true, model.DefaultAIModel, "sk-test-key-1234", false); err != nil {
		t.Fatal(err)
	}
	game, skipped, err := app.store.AddAIGame(model.Game{
		Name:       "AI Game",
		Executable: "ai.exe",
		ImagePath:  `C:\Games\ai.exe`,
		Source:     model.GameSourceAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if skipped {
		t.Fatal("AI add was unexpectedly skipped")
	}

	state := decodeMap(t, app.request(t, http.MethodGet, "/api/v1/state", nil, "", "").body)
	games, ok := state["games"].([]any)
	if !ok || len(games) != 1 {
		t.Fatalf("unexpected games: %+v", state["games"])
	}
	view, ok := games[0].(map[string]any)
	if !ok || view["source"] != model.GameSourceAI {
		t.Fatalf("game source was not exposed: %+v", games[0])
	}

	result := app.request(t, http.MethodDelete, "/api/v1/games/"+game.ID, nil, csrf, "")
	if result.status != http.StatusNoContent {
		t.Fatalf("delete AI game status = %d body=%s", result.status, result.body)
	}
	ignored, err := app.store.AIIgnoredKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ignored[model.StrangeActivityKey(game.Executable, game.ImagePath)]; !ok {
		t.Fatal("deleting an AI game did not create an ignore entry")
	}
	result = app.request(t, http.MethodGet, "/api/v1/events?limit=50", nil, "", "")
	if !bytes.Contains(result.body, []byte("removed-and-ignored")) {
		t.Fatalf("missing removal audit event: %s", result.body)
	}
}
