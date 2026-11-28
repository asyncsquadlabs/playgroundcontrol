package store

import (
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

func activityVisit(now time.Time, browser, user, profile, source, domain, rawURL, title, query string) model.BrowserVisit {
	name := `PC\kid`
	if user == "S-1-5-21-2" {
		name = `PC\kid2`
	}
	return model.BrowserVisit{
		Browser: browser, UserSID: user, UserName: name, ProfileKey: profile, ProfileLabel: "Default",
		SourceKey: source, Domain: domain, URL: rawURL, Title: title,
		SearchEngine: "Google", SearchQuery: query, VisitedAt: now,
	}
}

func TestBrowserActivityJournalUpsertFilterAndRemoval(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := openStore(t, t.TempDir(), fixedClock(now))
	if _, err := st.SaveBrowserSettings(true, true, false, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBrowserJournalUsers([]model.BrowserUser{{SID: "S-1-5-21-1", UserName: `PC\kid`, Selected: true}}, now); err != nil {
		t.Fatal(err)
	}
	visits := []model.BrowserVisit{
		activityVisit(now.Add(-time.Minute), "chrome", "S-1-5-21-1", "profile-1", "s1", "google.com", "https://google.com/search?q=algebra", "algebra - Google Search", "algebra"),
		activityVisit(now.Add(-2*time.Minute), "edge", "S-1-5-21-1", "profile-2", "s2", "poki.com", "https://poki.com/games", "Poki", ""),
	}
	if written, err := st.RecordBrowserActivity(now, visits); err != nil || written != 2 {
		t.Fatalf("written=%d err=%v", written, err)
	}
	// Upserting the same source key must not duplicate the row, and a non-empty
	// title update must win.
	updated := visits[0]
	updated.Title = "Algebra search"
	if written, err := st.RecordBrowserActivity(now.Add(time.Second), []model.BrowserVisit{updated}); err != nil || written != 1 {
		t.Fatalf("update written=%d err=%v", written, err)
	}
	filter := model.BrowserActivityFilter{From: now.Add(-24 * time.Hour), To: now.Add(time.Minute), Limit: 10}
	list, more, err := st.BrowserActivity(filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || more {
		t.Fatalf("list=%+v more=%t", list, more)
	}
	if list[0].Title != "Algebra search" || list[0].SearchQuery != "algebra" || list[0].URL == "" {
		t.Fatalf("item=%+v", list[0])
	}

	// Re-verify only the Google visit, then mark the older one as removed.
	if _, err := st.RecordBrowserActivity(now.Add(2*time.Second), []model.BrowserVisit{updated}); err != nil {
		t.Fatal(err)
	}
	removed, err := st.MarkMissingBrowserActivity("profile-2", now.Add(2*time.Second), now.Add(-7*24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	filter.RemovedOnly = true
	list, _, err = st.BrowserActivity(filter)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Domain != "poki.com" || list[0].RemovedAt == nil {
		t.Fatalf("removed list=%+v", list)
	}
}

func TestBrowserActivityFiltersAndPagination(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := openStore(t, t.TempDir(), fixedClock(now))
	visits := []model.BrowserVisit{
		activityVisit(now.Add(-3*time.Minute), "chrome", "S-1-5-21-1", "p1", "s1", "google.com", "https://google.com/search?q=one", "One", "one"),
		activityVisit(now.Add(-2*time.Minute), "edge", "S-1-5-21-1", "p2", "s2", "bing.com", "https://bing.com/search?q=two", "Two", "two"),
		activityVisit(now.Add(-time.Minute), "firefox", "S-1-5-21-2", "p3", "s3", "example.com", "https://example.com/page", "Example", ""),
	}
	if _, err := st.RecordBrowserActivity(now, visits); err != nil {
		t.Fatal(err)
	}
	base := model.BrowserActivityFilter{From: now.Add(-24 * time.Hour), To: now.Add(time.Minute)}
	userFilter := base
	userFilter.UserName = `PC\kid`
	list, _, err := st.BrowserActivity(userFilter)
	if err != nil || len(list) != 2 {
		t.Fatalf("user filter list=%+v err=%v", list, err)
	}
	browserFilter := base
	browserFilter.Browser = "firefox"
	list, _, err = st.BrowserActivity(browserFilter)
	if err != nil || len(list) != 1 || list[0].Browser != "firefox" {
		t.Fatalf("browser filter list=%+v err=%v", list, err)
	}
	queryFilter := base
	queryFilter.Query = "two"
	list, _, err = st.BrowserActivity(queryFilter)
	if err != nil || len(list) != 1 || list[0].Domain != "bing.com" {
		t.Fatalf("query filter list=%+v err=%v", list, err)
	}
	// LIKE wildcards in the search text are literal.
	queryFilter.Query = "%"
	list, _, err = st.BrowserActivity(queryFilter)
	if err != nil || len(list) != 0 {
		t.Fatalf("wildcard search list=%+v err=%v", list, err)
	}
	page := base
	page.Limit = 1
	list, more, err := st.BrowserActivity(page)
	if err != nil || len(list) != 1 || !more {
		t.Fatalf("page list=%+v more=%t err=%v", list, more, err)
	}
	page.Offset = 1
	list, more, err = st.BrowserActivity(page)
	if err != nil || len(list) != 1 || !more {
		t.Fatalf("page2 list=%+v more=%t err=%v", list, more, err)
	}
	page.Offset = 2
	list, more, err = st.BrowserActivity(page)
	if err != nil || len(list) != 1 || more {
		t.Fatalf("page3 list=%+v more=%t err=%v", list, more, err)
	}
}

func TestBrowserHistoryAlertsAcknowledgeAndCounts(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := openStore(t, t.TempDir(), fixedClock(now))
	alert, err := st.RecordBrowserHistoryAlert(model.BrowserHistoryAlert{
		Time: now, UserSID: "S-1-5-21-1", UserName: `PC\kid`, Browser: "chrome",
		ProfileLabel: "Default", RemovedCount: 7, Reason: model.BrowserHistoryAlertBulkRemoval,
	})
	if err != nil || alert.ID == "" {
		t.Fatalf("alert=%+v err=%v", alert, err)
	}
	count, err := st.UnacknowledgedBrowserHistoryAlertsCount()
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	alerts, err := st.BrowserHistoryAlerts(10)
	if err != nil || len(alerts) != 1 || alerts[0].RemovedCount != 7 {
		t.Fatalf("alerts=%+v err=%v", alerts, err)
	}
	ok, err := st.AcknowledgeBrowserHistoryAlert(alert.ID, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("ack=%t err=%v", ok, err)
	}
	count, _ = st.UnacknowledgedBrowserHistoryAlertsCount()
	if count != 0 {
		t.Fatalf("count after ack=%d", count)
	}
	if ok, _ := st.AcknowledgeBrowserHistoryAlert(alert.ID, now); ok {
		t.Fatal("second acknowledgement succeeded")
	}
}

func TestBrowserActivityRetentionAndUsersResetBackfill(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	st := openStore(t, t.TempDir(), fixedClock(now))
	if _, err := st.SaveBrowserSettings(true, true, false, true); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBrowserJournalUsers([]model.BrowserUser{{SID: "S-1-5-21-1", UserName: `PC\kid`, Selected: true}}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBrowserActivityBackfillDone(now); err != nil {
		t.Fatal(err)
	}
	if st.BrowserSettings().ActivityBackfillAt.IsZero() {
		t.Fatal("backfill marker was not set")
	}
	if err := st.SaveBrowserJournalUsers([]model.BrowserUser{{SID: "S-1-5-21-2", UserName: `PC\kid2`, Selected: true}}, now); err != nil {
		t.Fatal(err)
	}
	if !st.BrowserSettings().ActivityBackfillAt.IsZero() {
		t.Fatal("account change did not reset backfill")
	}
	visits := []model.BrowserVisit{
		activityVisit(now.Add(-91*24*time.Hour), "chrome", "S-1-5-21-1", "p1", "old", "google.com", "https://google.com/old", "Old", ""),
		activityVisit(now.Add(-time.Minute), "chrome", "S-1-5-21-1", "p1", "new", "google.com", "https://google.com/new", "New", ""),
	}
	if _, err := st.RecordBrowserActivity(now, visits); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PruneBrowserActivity(now); err != nil {
		t.Fatal(err)
	}
	list, _, err := st.BrowserActivity(model.BrowserActivityFilter{
		From: now.Add(-BrowserActivityRetention - time.Hour), To: now.Add(time.Minute), Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "New" {
		t.Fatalf("retention list=%+v", list)
	}
}
