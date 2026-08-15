package bridge

import (
	"sync"
	"time"
)

// progressTicker runs a callback periodically until it is stopped.
type progressTicker struct {
	ticker   *time.Ticker
	done     chan struct{}
	finished chan struct{}
	once     sync.Once
}

// createProgressTicker starts a progress callback at the requested interval.
// Non-positive intervals and nil callbacks disable the ticker.
func createProgressTicker(interval time.Duration, callback func() bool) *progressTicker {
	if interval <= 0 || callback == nil {
		return nil
	}

	pt := &progressTicker{
		ticker:   time.NewTicker(interval),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	go func() {
		defer close(pt.finished)
		for {
			select {
			case <-pt.ticker.C:
				callback()
			case <-pt.done:
				return
			}
		}
	}()

	return pt
}

// Stop prevents future callbacks and waits for an in-flight callback loop to exit.
func (pt *progressTicker) Stop() {
	if pt == nil {
		return
	}
	pt.once.Do(func() {
		pt.ticker.Stop()
		close(pt.done)
		<-pt.finished
	})
}
