package forecast

import (
	"sync"
	"time"
)

type recommendation struct {
	at    time.Time
	value int32
}

// Stabilizer applies the highest recommendation seen within a window, per key, so targets go up
// immediately and come down only after staying low for the whole window (the same scale-down
// rule the HPA uses). It is safe for concurrent use.
type Stabilizer struct {
	mu   sync.Mutex
	recs map[string][]recommendation
}

// NewStabilizer returns an empty Stabilizer.
func NewStabilizer() *Stabilizer {
	return &Stabilizer{recs: map[string][]recommendation{}}
}

// Seeded reports whether key has any recommendation recorded.
func (s *Stabilizer) Seeded(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.recs[key]) > 0
}

// Recommend records value at now and returns the maximum recommendation within window.
func (s *Stabilizer) Recommend(key string, now time.Time, value int32, window time.Duration) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-window)
	kept := s.recs[key][:0]
	for _, r := range s.recs[key] {
		if r.at.After(cutoff) {
			kept = append(kept, r)
		}
	}
	kept = append(kept, recommendation{at: now, value: value})
	s.recs[key] = kept
	best := value
	for _, r := range kept {
		if r.value > best {
			best = r.value
		}
	}
	return best
}

// Forget drops all state for key.
func (s *Stabilizer) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, key)
}
