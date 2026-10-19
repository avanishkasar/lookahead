// Package arrivals counts SandboxClaim arrivals per warm pool.
package arrivals

import (
	"sync"
	"time"
)

// MaxHistory is the longest history a Recorder keeps. It bounds memory per pool to two arrays
// of this many seconds.
const MaxHistory = time.Hour

const slots = int64(MaxHistory / time.Second)

// Key identifies a warm pool.
type Key struct {
	Namespace string
	Pool      string
}

type ring struct {
	second [slots]int64
	count  [slots]uint32
}

// Recorder keeps per-second arrival counts for the last MaxHistory per pool. It is safe for
// concurrent use; the informer event handler writes while reconcilers read.
type Recorder struct {
	mu    sync.Mutex
	pools map[Key]*ring
	now   func() time.Time
}

// NewRecorder returns an empty Recorder that uses now as its clock (time.Now if nil).
func NewRecorder(now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{pools: map[Key]*ring{}, now: now}
}

// Record counts one arrival for key at t. Arrivals older than MaxHistory are dropped. Arrivals
// in the future (apiserver and controller clock skew) are counted at the current second.
func (r *Recorder) Record(key Key, t time.Time) {
	now := r.now()
	if t.After(now) {
		t = now
	}
	if now.Sub(t) >= MaxHistory {
		return
	}
	sec := t.Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	rg, ok := r.pools[key]
	if !ok {
		rg = &ring{}
		for i := range rg.second {
			rg.second[i] = -1
		}
		r.pools[key] = rg
	}
	slot := sec % slots
	if rg.second[slot] != sec {
		rg.second[slot] = sec
		rg.count[slot] = 0
	}
	rg.count[slot]++
}

// Series returns arrivals per bucket over the last history, oldest first, using only buckets
// that have fully elapsed at now: a partial bucket would read as a drop in demand. Buckets
// are aligned to multiples of bucket since the Unix epoch so results are stable across calls.
func (r *Recorder) Series(key Key, now time.Time, bucket, history time.Duration) []float64 {
	bs := max(int64(bucket/time.Second), 1)
	history = min(history, MaxHistory)
	n := int64(history/time.Second) / bs
	if n < 1 {
		return nil
	}
	end := (now.Unix() / bs) * bs // exclusive
	start := end - n*bs
	out := make([]float64, n)

	r.mu.Lock()
	defer r.mu.Unlock()
	rg, ok := r.pools[key]
	if !ok {
		return out
	}
	for sec := start; sec < end; sec++ {
		slot := ((sec % slots) + slots) % slots
		if rg.second[slot] == sec {
			out[(sec-start)/bs] += float64(rg.count[slot])
		}
	}
	return out
}

// Forget drops the history of key.
func (r *Recorder) Forget(key Key) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pools, key)
}
