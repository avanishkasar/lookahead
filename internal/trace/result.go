package trace

import (
	"math"
	"slices"
	"time"
)

// ClaimResult is the outcome of one replayed claim.
type ClaimResult struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	// ReadyAt is zero if the claim never became Ready within the replay's timeout.
	ReadyAt time.Time `json:"readyAt,omitzero"`
	// LaunchType is the agents.x-k8s.io/launch-type label of the bound Sandbox: warm or cold.
	LaunchType string `json:"launchType,omitempty"`
	Error      string `json:"error,omitempty"`
}

// PoolSample is one observation of the warm pool.
type PoolSample struct {
	At              time.Time `json:"at"`
	SpecReplicas    int32     `json:"specReplicas"`
	Replicas        int32     `json:"replicas"`
	ReadyReplicas   int32     `json:"readyReplicas"`
	ObservationLost bool      `json:"observationLost,omitempty"`
}

// Result is everything one replay produced.
type Result struct {
	Trace     string        `json:"trace"`
	Strategy  string        `json:"strategy"`
	RunID     string        `json:"runID"`
	StartedAt time.Time     `json:"startedAt"`
	EndedAt   time.Time     `json:"endedAt"`
	Claims    []ClaimResult `json:"claims"`
	Samples   []PoolSample  `json:"samples"`
	Summary   Summary       `json:"summary"`
}

// Summary is the benchmark metrics of a Result.
type Summary struct {
	Claims    int     `json:"claims"`
	Ready     int     `json:"ready"`
	TimedOut  int     `json:"timedOut"`
	P50Ms     float64 `json:"p50Ms"`
	P95Ms     float64 `json:"p95Ms"`
	P99Ms     float64 `json:"p99Ms"`
	ColdStart int     `json:"coldStarts"`
	// ColdStartRate is cold starts over claims that became Ready with a known launch type.
	ColdStartRate float64 `json:"coldStartRate"`
	// IdleSandboxMinutes integrates unclaimed pool members (status.replicas) over the replay:
	// provisioned sandboxes nobody is using.
	IdleSandboxMinutes float64 `json:"idleSandboxMinutes"`
	// IdleReadySandboxMinutes integrates status.readyReplicas only.
	IdleReadySandboxMinutes float64 `json:"idleReadySandboxMinutes"`
}

// Summarize computes the Summary of r.
func Summarize(r *Result) Summary {
	s := Summary{Claims: len(r.Claims)}
	var lat []float64
	known := 0
	for _, c := range r.Claims {
		if c.ReadyAt.IsZero() {
			s.TimedOut++
			continue
		}
		s.Ready++
		lat = append(lat, float64(c.ReadyAt.Sub(c.CreatedAt).Milliseconds()))
		switch c.LaunchType {
		case "cold":
			s.ColdStart++
			known++
		case "warm":
			known++
		}
	}
	slices.Sort(lat)
	s.P50Ms, s.P95Ms, s.P99Ms = Percentile(lat, 50), Percentile(lat, 95), Percentile(lat, 99)
	if known > 0 {
		s.ColdStartRate = float64(s.ColdStart) / float64(known)
	}
	for i := 1; i < len(r.Samples); i++ {
		prev, cur := r.Samples[i-1], r.Samples[i]
		if prev.ObservationLost {
			continue
		}
		dt := cur.At.Sub(prev.At).Minutes()
		s.IdleSandboxMinutes += float64(prev.Replicas) * dt
		s.IdleReadySandboxMinutes += float64(prev.ReadyReplicas) * dt
	}
	return s
}

// Percentile returns the p-th percentile of sorted using the nearest-rank method, or NaN for
// an empty slice.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	rank := max(int(math.Ceil(p/100*float64(len(sorted)))), 1)
	return sorted[rank-1]
}
