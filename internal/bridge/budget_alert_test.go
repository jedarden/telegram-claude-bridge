package bridge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// recordCostAndAlert records a cost event for the group and runs the budget
// crossing check, mirroring what persistSession does after RecordCostEvent.
func recordCostAndAlert(t *testing.T, sm *SessionManager, group *Group, threadID int64, amount float64) {
	t.Helper()
	ctx := context.Background()
	if err := sm.db.RecordCostEvent(ctx, &CostEvent{
		ChatID:    group.ChatID,
		ThreadID:  threadID,
		CostUSD:   amount,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("RecordCostEvent: %v", err)
	}
	sm.maybeSendBudgetAlert(ctx, group.ChatID, threadID, group)
}

func TestMaybeSendBudgetAlert_OncePerThreshold(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    -100777,
		CWD:       "/tmp/test",
		MaxBudget: 10.0,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender, rec := newRecordingProxy(t)
	sm := newTestSessionManager(t, db, sender)

	// 50% — below both thresholds, nothing sent.
	recordCostAndAlert(t, sm, group, 11, 5.0)
	if texts := recordedSendTexts(rec); len(texts) != 0 {
		t.Fatalf("below 80%%: got sends %q, want none", texts)
	}

	// 85% — one warning to the crossing topic.
	recordCostAndAlert(t, sm, group, 11, 3.5)
	texts := recordedSendTexts(rec)
	if len(texts) != 1 {
		t.Fatalf("after crossing 80%%: got %d sends, want 1 (%q)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "Approaching budget limit") || !strings.Contains(texts[0], "85.0%") {
		t.Errorf("80%% alert text: got %q", texts[0])
	}

	// 90%, from a different topic — no repeat warning.
	recordCostAndAlert(t, sm, group, 12, 0.5)
	if texts := recordedSendTexts(rec); len(texts) != 1 {
		t.Fatalf("between thresholds: got %d sends, want 1 (%q)", len(texts), texts)
	}

	// 105% — one exceeded alert.
	recordCostAndAlert(t, sm, group, 12, 1.5)
	texts = recordedSendTexts(rec)
	if len(texts) != 2 {
		t.Fatalf("after crossing 100%%: got %d sends, want 2 (%q)", len(texts), texts)
	}
	if !strings.Contains(texts[1], "Budget exceeded") || !strings.Contains(texts[1], "105.0%") {
		t.Errorf("100%% alert text: got %q", texts[1])
	}

	// Over budget, more spend — silence, not spam.
	recordCostAndAlert(t, sm, group, 11, 1.0)
	if texts := recordedSendTexts(rec); len(texts) != 2 {
		t.Fatalf("over budget: got %d sends, want 2 (%q)", len(texts), texts)
	}
}

func TestMaybeSendBudgetAlert_JumpPast80FiresOnly100(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    -100778,
		CWD:       "/tmp/test",
		MaxBudget: 10.0,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	sender, rec := newRecordingProxy(t)
	sm := newTestSessionManager(t, db, sender)

	// A single event leaps from 0% to 120%: only the exceeded alert fires —
	// warning about "approaching" would be noise.
	recordCostAndAlert(t, sm, group, 30, 12.0)
	texts := recordedSendTexts(rec)
	if len(texts) != 1 {
		t.Fatalf("jump past 80%%: got %d sends, want 1 (%q)", len(texts), texts)
	}
	if !strings.Contains(texts[0], "Budget exceeded") {
		t.Errorf("alert text: got %q", texts[0])
	}
}

func TestMaybeSendBudgetAlert_NoBudgetConfigured(t *testing.T) {
	db := openTestDB(t)

	group := &Group{
		ChatID:    -100779,
		CWD:       "/tmp/test",
		MaxBudget: 0, // budget tracking disabled
		CreatedAt: time.Now().UTC(),
	}

	sender, rec := newRecordingProxy(t)
	sm := newTestSessionManager(t, db, sender)

	recordCostAndAlert(t, sm, group, 11, 50.0)
	if texts := recordedSendTexts(rec); len(texts) != 0 {
		t.Fatalf("no budget configured: got sends %q, want none", texts)
	}
}

func TestCmdBudget_Set_RearmsThresholdAlerts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	admin := &AllowedUser{UserID: 12345, Role: "admin", AddedAt: time.Now().UTC()}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		MaxBudget: 10.0,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Both thresholds already fired under the old budget.
	for _, thr := range []int{80, 100} {
		if _, err := db.ShouldSendBudgetAlert(ctx, 100, thr); err != nil {
			t.Fatalf("claim threshold %d: %v", thr, err)
		}
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget 20", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "20")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}
	if !strings.Contains(reply, "updated to: $20.00") {
		t.Fatalf("should confirm budget set, got: %s", reply)
	}

	// Raising the budget re-arms both thresholds so the new budget's
	// crossings alert again.
	for _, thr := range []int{80, 100} {
		first, err := db.ShouldSendBudgetAlert(ctx, 100, thr)
		if err != nil {
			t.Fatalf("claim after raise, threshold %d: %v", thr, err)
		}
		if !first {
			t.Errorf("threshold %d should be claimable again after budget change", thr)
		}
	}
}
