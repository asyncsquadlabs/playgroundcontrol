package store

import (
	"errors"
	"testing"
	"time"

	"github.com/asyncsquadlabs/playgroundcontrol/internal/model"
)

func openBrowserTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestBrowserSettingsDefaultsAndValidation(t *testing.T) {
	st := openBrowserTestStore(t)
	settings := st.BrowserSettings()
	if settings.Enabled || !settings.AutoBlock || settings.AIClassification {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if _, err := st.SaveBrowserSettings(true, true, true); !errors.Is(err, ErrAIKeyRequired) {
		t.Fatalf("SaveBrowserSettings with AI and no key = %v, want ErrAIKeyRequired", err)
	}
	updated, err := st.SaveBrowserSettings(true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.AutoBlock || updated.AIClassification {
		t.Fatalf("unexpected saved settings: %+v", updated)
	}
}

func TestBrowserSeedAndAllowlistPersistence(t *testing.T) {
	st := openBrowserTestStore(t)
	now := time.Now().UTC()
	item, changed, err := st.RecordBrowserSeed("https://www.poki.com/games", "seed match", now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || item.Domain != "poki.com" || item.Status != model.BrowserStatusBlocked || item.Source != model.BrowserSourceSeed {
		t.Fatalf("unexpected seed result: %+v changed=%t", item, changed)
	}
	again, changed, err := st.RecordBrowserSeed("poki.com", "seed match", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if changed || again.SeenCount != 2 {
		t.Fatalf("repeat seed result: %+v changed=%t", again, changed)
	}

	allowed, err := st.SetBrowserDomainStatus(item.ID, model.BrowserStatusAllowed, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Status != model.BrowserStatusAllowed {
		t.Fatalf("status = %s, want allowed", allowed.Status)
	}
	afterAllow, changed, err := st.RecordBrowserSeed("poki.com", "seed match", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if changed || afterAllow.Status != model.BrowserStatusAllowed {
		t.Fatalf("allowlist was overridden: %+v changed=%t", afterAllow, changed)
	}

	blocked, err := st.SetBrowserDomainStatus(item.ID, model.BrowserStatusBlocked, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status != model.BrowserStatusBlocked {
		t.Fatalf("status = %s, want blocked", blocked.Status)
	}
	domains, err := st.BrowserBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 1 || domains[0] != "poki.com" {
		t.Fatalf("blocked domains = %v", domains)
	}
}

func TestBrowserAIVerdictThresholds(t *testing.T) {
	st := openBrowserTestStore(t)
	now := time.Now().UTC()

	blocked, action, changed, err := st.RecordBrowserAIVerdict("slither.io", 0.95, "browser game", "deepseek-flash", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || action != "ai-auto-blocked" || blocked.Status != model.BrowserStatusBlocked {
		t.Fatalf("high confidence result: %+v action=%s changed=%t", blocked, action, changed)
	}

	review, action, changed, err := st.RecordBrowserAIVerdict("example-game.net", 0.80, "likely game", "deepseek-flash", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if changed || action != "review" || review.Status != model.BrowserStatusReview {
		t.Fatalf("review result: %+v action=%s changed=%t", review, action, changed)
	}

	ignored, _, _, err := st.RecordBrowserAIVerdict("example.net", 0.50, "not a game", "deepseek-flash", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Domain != "" {
		t.Fatalf("low confidence verdict was stored: %+v", ignored)
	}

	blockedCount, reviewCount, allowedCount, err := st.BrowserDomainCounts()
	if err != nil {
		t.Fatal(err)
	}
	if blockedCount != 1 || reviewCount != 1 || allowedCount != 0 {
		t.Fatalf("counts = %d/%d/%d", blockedCount, reviewCount, allowedCount)
	}
}

func TestBrowserDetectionsDedupeAndAuditDomain(t *testing.T) {
	st := openBrowserTestStore(t)
	now := time.Now().UTC()
	detection := model.BrowserDetection{
		Time: now, Browser: "chrome", Domain: "poki.com",
		Source: model.BrowserSourceSeed, Action: "seed-blocked", Confidence: 1,
	}
	if err := st.RecordBrowserDetection(detection); err != nil {
		t.Fatal(err)
	}
	detection.Time = now.Add(5 * time.Minute)
	if err := st.RecordBrowserDetection(detection); err != nil {
		t.Fatal(err)
	}
	recent, err := st.RecentBrowserDetections(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Fatalf("detections = %d, want 1 deduped row", len(recent))
	}
	detection.Time = now.Add(20 * time.Minute)
	if err := st.RecordBrowserDetection(detection); err != nil {
		t.Fatal(err)
	}
	recent, err = st.RecentBrowserDetections(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("detections = %d, want 2 rows", len(recent))
	}

	if err := st.AddEvent(model.AuditEvent{
		Type: model.EventBrowser, Message: "blocked", Domain: "poki.com", Result: "test",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := st.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Domain != "poki.com" {
		t.Fatalf("audit domain missing: %+v", events)
	}
}
