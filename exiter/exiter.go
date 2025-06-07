package exiter

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type Exiter interface {
	SetSeedCount(int)
	SetCancelFunc(context.CancelFunc)
	IncrSeedCompleted(int)
	IncrPlacesFound(int)
	IncrPlacesCompleted(int)
	Run(context.Context)
}

type exiter struct {
	seedCount       int
	seedCompleted   int
	placesFound     int
	placesCompleted int

	mu         *sync.Mutex
	cancelFunc context.CancelFunc
}

func New() Exiter {
	return &exiter{
		mu: &sync.Mutex{},
	}
}

func (e *exiter) SetSeedCount(val int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.seedCount = val
}

func (e *exiter) SetCancelFunc(fn context.CancelFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.cancelFunc = fn
}

func (e *exiter) IncrSeedCompleted(val int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.seedCompleted += val
}

func (e *exiter) IncrPlacesFound(val int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.placesFound += val
}

func (e *exiter) IncrPlacesCompleted(val int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.placesCompleted += val
}

func (e *exiter) Run(ctx context.Context) {
	fmt.Printf("[Exiter] Run started\n")
	defer fmt.Printf("[Exiter] Run finished\n")

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("[Exiter] Context done, exiting Run loop.\n")
			return
		case <-ticker.C:
			fmt.Printf("[Exiter] Tick: Checking if done.\n")
			if e.isDone() {
				fmt.Printf("[Exiter] All tasks complete. Calling cancelFunc().\n")
				e.cancelFunc()
				return
			}
			fmt.Printf("[Exiter] Tasks not yet complete.\n")
		}
	}
}

func (e *exiter) isDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Printf("[Exiter] State: seedCompleted=%d, seedCount=%d, placesFound=%d, placesCompleted=%d\n", e.seedCompleted, e.seedCount, e.placesFound, e.placesCompleted)

	if e.seedCompleted != e.seedCount {
		return false
	}

	if e.placesFound != e.placesCompleted {
		return false
	}

	return true
}
