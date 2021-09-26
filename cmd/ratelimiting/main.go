package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	_ "unsafe"
)

// This is a test to verify the rate limiting implementation itself apart from the distributed part.

const seconds = 3

func main() {
	l := BucketLimiter{
		Count:     0,
		LastReset: runtimeNano(),
	}

	wg := sync.WaitGroup{}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := 0
			a := 0

			start := time.Now()
			for time.Since(start) < (seconds * time.Second) {
				blocked := l.CheckAndIncrement()
				if !blocked {
					a++
				}
				t++
				time.Sleep(10 * time.Millisecond)
			}

			fmt.Printf("avg: %f, t: %d\n", float64(a)/seconds, t)
		}()
	}

	wg.Wait()
}

// runtimeNano returns the current value of the runtime clock in nanoseconds.
//go:linkname runtimeNano runtime.nanotime
func runtimeNano() int64

const (
	interval = int64(time.Second)
	limit    = 50
)

type BucketLimiter struct {
	Count     uint64
	LastReset int64
}

func (l *BucketLimiter) CheckAndIncrement() bool {
	now := runtimeNano()
	if (now - atomic.LoadInt64(&l.LastReset)) > interval {
		atomic.StoreInt64(&l.LastReset, now)
		atomic.StoreUint64(&l.Count, 1)
		return true
	}

	return atomic.AddUint64(&l.Count, 1) >= limit
}
