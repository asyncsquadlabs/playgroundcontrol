package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

type fakeBrowser struct {
	mu     sync.Mutex
	wakes  int
	syncs  int
	status model.BrowserStatus
	err    error
}

func (f *fakeBrowser) Wake() {
	f.mu.Lock()
	f.wakes++
	f.mu.Unlock()
}

func (f *fakeBrowser) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs++
	return f.err
}

func (f *fakeBrowser) Status() model.BrowserStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.status
	status.Browsers = append([]string(nil), f.status.Browsers...)
	return status
}

func TestBrowserEndpointsRequireAuthAndCSRF(t *testing.T) {
	app := newTestApp(t, true)
	if result := app.request(t, http.MethodGet, "/api/v1/browser", nil, "", ""); result.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated browser status = %d", result.status)
	}
	csrf := app.login(t, "123456")
	if result := app.request(t, http.MethodPut, "/api/v1/browser/settings",
		map[string]any{"enabled": true, "autoBlock": true, "aiClassification": false}, "", ""); result.status != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d", result.status)
	}
	if result := app.request(t, http.MethodPut, "/api/v1/browser/settings",
		map[string]any{"enabled": true, "autoBlock": true, "aiClassification": false}, csrf, ""); result.status != http.StatusOK {
		t.Fatalf("settings status = %d body=%s", result.status, result.body)
	}
	if result := app.request(t, http.MethodPut, "/api/v1/browser/settings",
		map[string]any{"enabled": true, "autoBlock": true, "aiClassification": true}, csrf, ""); result.status != http.StatusBadRequest {
		t.Fatalf("AI classification without key status = %d body=%s", result.status, result.body)
	}
}

func TestBrowserDomainLifecycleAndState(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	if result := app.request(t, http.MethodPost, "/api/v1/browser/domains",
		map[string]string{"domain": "chrome://settings"}, csrf, ""); result.status != http.StatusBadRequest {
		t.Fatalf("invalid domain status = %d body=%s", result.status, result.body)
	}
	result := app.request(t, http.MethodPost, "/api/v1/browser/domains",
		map[string]string{"domain": "https://www.poki.com/games"}, csrf, "")
	if result.status != http.StatusCreated {
		t.Fatalf("add domain status = %d body=%s", result.status, result.body)
	}
	added := decodeMap(t, result.body)["domain"].(map[string]any)
	if added["domain"] != "poki.com" || added["status"] != model.BrowserStatusBlocked {
		t.Fatalf("added domain = %+v", added)
	}
	if result := app.request(t, http.MethodPost, "/api/v1/browser/domains",
		map[string]string{"domain": "poki.com"}, csrf, ""); result.status != http.StatusConflict {
		t.Fatalf("duplicate domain status = %d body=%s", result.status, result.body)
	}

	blocked := app.request(t, http.MethodGet, "/api/v1/browser/domains?status=blocked", nil, "", "")
	if blocked.status != http.StatusOK {
		t.Fatalf("blocked list status = %d", blocked.status)
	}
	list := decodeMap(t, blocked.body)["domains"].([]any)
	if len(list) != 1 {
		t.Fatalf("blocked domains = %+v", list)
	}
	id := list[0].(map[string]any)["id"].(string)
	allowed := app.request(t, http.MethodPut, "/api/v1/browser/domains/"+id,
		map[string]string{"status": "allowed"}, csrf, "")
	if allowed.status != http.StatusOK {
		t.Fatalf("allow status = %d body=%s", allowed.status, allowed.body)
	}
	allowedList := app.request(t, http.MethodGet, "/api/v1/browser/domains?status=allowed", nil, "", "")
	if len(decodeMap(t, allowedList.body)["domains"].([]any)) != 1 {
		t.Fatalf("allowed list = %s", allowedList.body)
	}

	now := time.Now().UTC()
	if _, _, err := app.store.RecordBrowserSeed("crazygames.com", "seed", now); err != nil {
		t.Fatal(err)
	}
	if err := app.store.RecordBrowserDetection(model.BrowserDetection{
		Time: now, Browser: "chrome", Domain: "crazygames.com",
		Source: model.BrowserSourceSeed, Action: "seed-blocked", Confidence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	stateResult := app.request(t, http.MethodGet, "/api/v1/state", nil, "", "")
	state := decodeMap(t, stateResult.body)
	browser := state["browser"].(map[string]any)
	if browser["blockedCount"].(float64) != 1 || browser["allowedCount"].(float64) != 1 {
		t.Fatalf("browser summary = %+v", browser)
	}
	detections := app.request(t, http.MethodGet, "/api/v1/browser/detections?limit=10", nil, "", "")
	if len(decodeMap(t, detections.body)["detections"].([]any)) != 1 {
		t.Fatalf("detections = %s", detections.body)
	}

	events := app.request(t, http.MethodGet, "/api/v1/events?limit=20", nil, "", "")
	if events.status != http.StatusOK {
		t.Fatalf("events status = %d", events.status)
	}
	if !strings.Contains(string(events.body), "poki.com") {
		t.Fatalf("browser audit event missing: %s", events.body)
	}
}

func TestDashboardAssetsIncludeBrowserPanel(t *testing.T) {
	app := newTestApp(t, true)
	index := app.request(t, http.MethodGet, "/", nil, "", "")
	if index.status != http.StatusOK || !strings.Contains(string(index.body), "Browser monitoring") {
		t.Fatalf("dashboard missing browser panel: status=%d", index.status)
	}
	js := app.request(t, http.MethodGet, "/assets/app.js", nil, "", "")
	if js.status != http.StatusOK || !strings.Contains(string(js.body), "/api/v1/browser/settings") {
		t.Fatalf("dashboard JS missing browser API: status=%d", js.status)
	}
}

func TestBrowserJournalUsersActivityAndAlerts(t *testing.T) {
	app := newTestApp(t, true)
	csrf := app.login(t, "123456")

	users := app.request(t, http.MethodGet, "/api/v1/browser/users", nil, "", "")
	if users.status != http.StatusOK || !strings.Contains(string(users.body), `PC\\kid`) {
		t.Fatalf("users status=%d body=%s", users.status, users.body)
	}
	updated := app.request(t, http.MethodPut, "/api/v1/browser/users",
		map[string]any{"sids": []string{"S-1-5-21-1"}}, csrf, "")
	if updated.status != http.StatusOK || !strings.Contains(string(updated.body), `"selected":true`) {
		t.Fatalf("update users status=%d body=%s", updated.status, updated.body)
	}

	settings := app.request(t, http.MethodPut, "/api/v1/browser/settings",
		map[string]any{"enabled": true, "autoBlock": true, "aiClassification": false, "activityLogging": true}, csrf, "")
	if settings.status != http.StatusOK {
		t.Fatalf("journal settings status=%d body=%s", settings.status, settings.body)
	}
	now := time.Now().UTC()
	if _, err := app.store.RecordBrowserActivity(now, []model.BrowserVisit{{
		Browser: "chrome", Domain: "google.com", VisitedAt: now,
		UserSID: "S-1-5-21-1", UserName: `PC\kid`, ProfileKey: "profile-1", ProfileLabel: "Default",
		SourceKey: "source-1", URL: "https://www.google.com/search?q=algebra",
		Title: "algebra - Google Search", SearchEngine: "Google", SearchQuery: "algebra",
	}}); err != nil {
		t.Fatal(err)
	}
	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)
	activity := app.request(t, http.MethodGet, "/api/v1/browser/activity?from="+from+"&to="+to, nil, "", "")
	if activity.status != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", activity.status, activity.body)
	}
	payload := decodeMap(t, activity.body)
	items := payload["activity"].([]any)
	if len(items) != 1 || !strings.Contains(string(activity.body), "algebra") {
		t.Fatalf("activity payload=%s", activity.body)
	}
	if strings.Contains(string(activity.body), "0001-01-01") {
		t.Fatalf("activity payload leaked a zero timestamp: %s", activity.body)
	}
	filters := app.request(t, http.MethodGet, "/api/v1/browser/activity/filters", nil, "", "")
	if filters.status != http.StatusOK || !strings.Contains(string(filters.body), `PC\\kid`) {
		t.Fatalf("filters status=%d body=%s", filters.status, filters.body)
	}

	alert, err := app.store.RecordBrowserHistoryAlert(model.BrowserHistoryAlert{
		Time: now, UserSID: "S-1-5-21-1", UserName: `PC\kid`, Browser: "chrome",
		ProfileLabel: "Default", RemovedCount: 8, Reason: model.BrowserHistoryAlertBulkRemoval,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity = app.request(t, http.MethodGet, "/api/v1/browser/activity", nil, "", "")
	if !strings.Contains(string(activity.body), `"alerts"`) || !strings.Contains(string(activity.body), alert.ID) {
		t.Fatalf("activity alerts missing: %s", activity.body)
	}
	ack := app.request(t, http.MethodPost, "/api/v1/browser/activity/alerts/"+alert.ID+"/acknowledge", nil, csrf, "")
	if ack.status != http.StatusNoContent {
		t.Fatalf("ack status=%d body=%s", ack.status, ack.body)
	}
	if count, err := app.store.UnacknowledgedBrowserHistoryAlertsCount(); err != nil || count != 0 {
		t.Fatalf("unacknowledged=%d err=%v", count, err)
	}
}

func TestBrowserActivityValidationAndLoggingRequiresMonitoring(t *testing.T) {
	app := newTestApp(t, true)
	if result := app.request(t, http.MethodGet, "/api/v1/browser/activity", nil, "", ""); result.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated activity status=%d", result.status)
	}
	csrf := app.login(t, "123456")
	if result := app.request(t, http.MethodPut, "/api/v1/browser/users",
		map[string]any{"sids": []string{"S-1-5-21-1"}}, "", ""); result.status != http.StatusForbidden {
		t.Fatalf("users without CSRF status=%d", result.status)
	}
	if result := app.request(t, http.MethodGet, "/api/v1/browser/activity?browser=unknown", nil, "", ""); result.status != http.StatusBadRequest {
		t.Fatalf("invalid browser status=%d body=%s", result.status, result.body)
	}
	if result := app.request(t, http.MethodGet, "/api/v1/browser/activity?limit=999", nil, "", ""); result.status != http.StatusBadRequest {
		t.Fatalf("invalid limit status=%d body=%s", result.status, result.body)
	}
	if result := app.request(t, http.MethodPut, "/api/v1/browser/settings",
		map[string]any{"enabled": false, "activityLogging": true}, csrf, ""); result.status != http.StatusBadRequest {
		t.Fatalf("journal without monitoring status=%d body=%s", result.status, result.body)
	}
}

func TestDashboardAssetsIncludeActivityPanel(t *testing.T) {
	app := newTestApp(t, true)
	index := app.request(t, http.MethodGet, "/", nil, "", "")
	if index.status != http.StatusOK || !strings.Contains(string(index.body), "Browser activity") ||
		!strings.Contains(string(index.body), "Monitored Windows accounts") {
		t.Fatalf("dashboard missing activity panel: status=%d", index.status)
	}
	js := app.request(t, http.MethodGet, "/assets/app.js", nil, "", "")
	if js.status != http.StatusOK || !strings.Contains(string(js.body), "/api/v1/browser/activity") ||
		!strings.Contains(string(js.body), "/api/v1/browser/users") {
		t.Fatalf("dashboard JS missing activity API: status=%d", js.status)
	}
}
