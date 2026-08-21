package bridge

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateProgressTicker_DisabledForZeroInterval(t *testing.T) {
	if pt := createProgressTicker(0, func() bool { return true }); pt != nil {
		pt.Stop()
		t.Error("interval 0 should disable the ticker (progress_interval_sec = 0 means no heartbeats)")
	}
	if pt := createProgressTicker(-time.Second, func() bool { return true }); pt != nil {
		pt.Stop()
		t.Error("negative interval should disable the ticker")
	}
	if pt := createProgressTicker(time.Second, nil); pt != nil {
		pt.Stop()
		t.Error("nil callback should disable the ticker")
	}
}

func TestCreateProgressTicker_FiresAtInterval(t *testing.T) {
	var calls int32
	pt := createProgressTicker(10*time.Millisecond, func() bool {
		atomic.AddInt32(&calls, 1)
		return true
	})
	if pt == nil {
		t.Fatal("ticker with positive interval should be created")
	}
	defer pt.Stop()

	// Wait for at least two fires so periodicity (not a one-shot) is covered.
	deadline := time.After(2 * time.Second)
	for {
		if atomic.LoadInt32(&calls) >= 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("ticker fired %d times in 2s, want >= 2", atomic.LoadInt32(&calls))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestProgressTicker_StopHaltsCallbacks(t *testing.T) {
	var calls int32
	pt := createProgressTicker(10*time.Millisecond, func() bool {
		atomic.AddInt32(&calls, 1)
		return true
	})
	if pt == nil {
		t.Fatal("ticker with positive interval should be created")
	}

	pt.Stop() // must return promptly and prevent further callbacks
	after := atomic.LoadInt32(&calls)

	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != after {
		t.Errorf("callbacks continued after Stop: %d before, %d after", after, got)
	}

	// Stop is idempotent.
	pt.Stop()
}
