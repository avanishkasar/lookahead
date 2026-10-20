package inspect

import (
	"sort"
	"sync"
	"time"
)

// Sample is one observation of a pool, for timelines.
type Sample struct {
	At       time.Time `json:"at"`
	Spec     int32     `json:"specReplicas"`
	Ready    int32     `json:"readyReplicas"`
	Desired  int32     `json:"desiredReplicas"`
	Forecast int32     `json:"forecastReplicas"`
	Hints    int32     `json:"hintReplicas"`
}

// History keeps a window of samples for one pool, plus every claim and hint seen in that window,
// so timelines still show objects that have since been deleted. It is safe for concurrent use.
type History struct {
	mu      sync.Mutex
	window  time.Duration
	samples []Sample
	claims  map[string]Claim
	hints   map[string]Hint
}

// NewHistory returns a History that keeps the last window of observations.
func NewHistory(window time.Duration) *History {
	return &History{window: window, claims: map[string]Claim{}, hints: map[string]Hint{}}
}

// Observe records s and drops observations older than the window.
func (h *History) Observe(s *Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sm := Sample{At: s.At, Spec: s.Spec, Ready: s.Ready}
	if p := s.Policy; p != nil {
		sm.Desired, sm.Forecast, sm.Hints = p.Desired, p.Forecast, p.HintReplicas
	}
	h.samples = append(h.samples, sm)
	for _, c := range s.Claims {
		h.claims[c.Name] = c
	}
	for _, hn := range s.Hints {
		h.hints[hn.Name] = hn
	}

	cutoff := s.At.Add(-h.window)
	i := sort.Search(len(h.samples), func(i int) bool { return !h.samples[i].At.Before(cutoff) })
	h.samples = append(h.samples[:0], h.samples[i:]...)
	for k, c := range h.claims {
		if c.Created.Before(cutoff) {
			delete(h.claims, k)
		}
	}
	for k, hn := range h.hints {
		if hn.Expires.Before(cutoff) {
			delete(h.hints, k)
		}
	}
}

// Samples returns the samples in the window, oldest first.
func (h *History) Samples() []Sample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Sample(nil), h.samples...)
}

// Claims returns the claims seen in the window, oldest first.
func (h *History) Claims() []Claim {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Claim, 0, len(h.claims))
	for _, c := range h.claims {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Hints returns the hints seen in the window, including deleted ones, by needed time.
func (h *History) Hints() []Hint {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Hint, 0, len(h.hints))
	for _, hn := range h.hints {
		out = append(out, hn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NeededAt.Before(out[j].NeededAt) })
	return out
}
