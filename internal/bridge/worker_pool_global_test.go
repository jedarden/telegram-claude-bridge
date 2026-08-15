package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkerPool_GlobalWorkerSemaphore(t *testing.T) {
	wp := NewWorkerPool(nil, nil, nil, 2)
	if cap(wp.globalWorkerTokens) != 2 {
		t.Fatalf("global worker semaphore capacity = %d, want 2", cap(wp.globalWorkerTokens))
	}

	if !wp.tryAcquireGlobalWorker() || !wp.tryAcquireGlobalWorker() {
		t.Fatal("expected both global worker slots to be available")
	}
	if wp.tryAcquireGlobalWorker() {
		t.Fatal("expected third worker to be rejected when both global slots are occupied")
	}

	wp.releaseGlobalWorker()
	if !wp.tryAcquireGlobalWorker() {
		t.Fatal("expected a released global worker slot to become available")
	}
}

func TestWorkerPool_SpawnWorker_GlobalLimitAcrossTopics(t *testing.T) {
	db := openTestDB(t)
	wp := NewWorkerPool(db, nil, nil, 1)
	if !wp.tryAcquireGlobalWorker() {
		t.Fatal("failed to reserve the only global worker slot")
	}
	t.Cleanup(wp.releaseGlobalWorker)

	group := &Group{MaxWorkers: 5}
	input := json.RawMessage(`{"prompt":"test task"}`)
	for _, threadID := range []int64{10, 20} {
		_, _, err := wp.SpawnWorker(context.Background(), 100, threadID, 1000, group, input)
		if err == nil {
			t.Fatalf("SpawnWorker topic %d succeeded despite the global limit", threadID)
		}
		if !strings.Contains(err.Error(), "global worker ceiling (1) reached") {
			t.Errorf("SpawnWorker topic %d error = %q, want global-limit error", threadID, err)
		}
	}
}

func TestNewWorkerPool_NoGlobalWorkerLimit(t *testing.T) {
	wp := NewWorkerPool(nil, nil, nil, 0)
	if wp.globalWorkerTokens != nil {
		t.Fatal("expected no semaphore when the global limit is disabled")
	}
	if !wp.tryAcquireGlobalWorker() {
		t.Fatal("expected an unlimited worker pool to acquire a slot")
	}
}
