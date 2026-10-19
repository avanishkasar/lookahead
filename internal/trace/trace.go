// Package trace generates, stores and summarises bursty SandboxClaim arrival traces.
package trace

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

// Kind is the kind of a trace event.
type Kind string

const (
	// KindClaim creates one SandboxClaim and deletes it after HoldMs.
	KindClaim Kind = "claim"
	// KindHint creates one DemandHint (only replayed for strategies that use hints).
	KindHint Kind = "hint"
)

// Event is one line of a trace (JSON Lines).
type Event struct {
	// AtMs is the offset from the start of the replay.
	AtMs int64 `json:"atMs"`
	Kind Kind  `json:"kind"`
	// HoldMs is how long a claim is kept after it is created.
	HoldMs int64 `json:"holdMs,omitempty"`
	// Replicas, NeededInMs and TTLSeconds describe a hint: Replicas sandboxes needed NeededInMs
	// after the hint is created.
	Replicas   int32 `json:"replicas,omitempty"`
	NeededInMs int64 `json:"neededInMs,omitempty"`
	TTLSeconds int32 `json:"ttlSeconds,omitempty"`
}

// Config describes a synthetic trace: a Poisson background plus periodic bursts, some of
// which are announced by a hint ahead of time.
type Config struct {
	Seed     uint64
	Duration time.Duration
	// BaseRate is the background Poisson arrival rate in claims per second.
	BaseRate float64
	// BurstEvery is the mean time between burst starts; 0 disables bursts.
	BurstEvery time.Duration
	// BurstJitter is the maximum +- offset applied to each burst start, as a fraction of
	// BurstEvery in [0, 0.5).
	BurstJitter float64
	// BurstSize claims arrive uniformly within BurstDuration.
	BurstSize     int
	BurstDuration time.Duration
	// Hold is the mean claim hold time; each hold is uniform in [0.5, 1.5] x Hold.
	Hold time.Duration
	// HintFraction of bursts are preceded by a hint for BurstSize sandboxes, HintLead earlier.
	HintFraction float64
	HintLead     time.Duration
	HintTTL      time.Duration
}

// Validate reports configuration errors.
func (c Config) Validate() error {
	switch {
	case c.Duration <= 0:
		return fmt.Errorf("duration must be positive")
	case c.BaseRate < 0:
		return fmt.Errorf("base rate must not be negative")
	case c.BurstEvery < 0 || c.BurstSize < 0 || c.BurstDuration < 0:
		return fmt.Errorf("burst parameters must not be negative")
	case c.BurstJitter < 0 || c.BurstJitter >= 0.5:
		return fmt.Errorf("burst jitter must be in [0, 0.5)")
	case c.Hold <= 0:
		return fmt.Errorf("hold must be positive")
	case c.HintFraction < 0 || c.HintFraction > 1:
		return fmt.Errorf("hint fraction must be in [0, 1]")
	case c.HintFraction > 0 && (c.HintLead < 0 || c.HintTTL <= 0):
		return fmt.Errorf("hints need a non-negative lead and a positive TTL")
	}
	return nil
}

// Generate builds a deterministic trace for c, sorted by AtMs.
func Generate(c Config) ([]Event, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewPCG(c.Seed, c.Seed^0x9e3779b97f4a7c15))
	end := c.Duration.Milliseconds()
	hold := func() int64 {
		return int64(float64(c.Hold.Milliseconds()) * (0.5 + rng.Float64()))
	}
	var events []Event

	if c.BaseRate > 0 {
		for t := 0.0; ; {
			t += rng.ExpFloat64() / c.BaseRate * 1000
			if int64(t) >= end {
				break
			}
			events = append(events, Event{AtMs: int64(t), Kind: KindClaim, HoldMs: hold()})
		}
	}

	if c.BurstEvery > 0 && c.BurstSize > 0 {
		every := c.BurstEvery.Milliseconds()
		for nominal := every; nominal < end; nominal += every {
			jitter := int64((rng.Float64()*2 - 1) * c.BurstJitter * float64(every))
			start := nominal + jitter
			if rng.Float64() < c.HintFraction {
				at := max(0, start-c.HintLead.Milliseconds())
				events = append(events, Event{
					AtMs: at, Kind: KindHint, Replicas: int32(c.BurstSize),
					NeededInMs: start - at, TTLSeconds: int32(math.Ceil(c.HintTTL.Seconds())),
				})
			}
			for range c.BurstSize {
				at := start + int64(rng.Float64()*float64(c.BurstDuration.Milliseconds()))
				if at < end {
					events = append(events, Event{AtMs: at, Kind: KindClaim, HoldMs: hold()})
				}
			}
		}
	}

	slices.SortStableFunc(events, func(a, b Event) int { return cmp.Compare(a.AtMs, b.AtMs) })
	return events, nil
}

// Write writes events as JSON Lines.
func Write(w io.Writer, events []Event) error {
	enc := json.NewEncoder(w)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			return fmt.Errorf("encode event %d: %w", i, err)
		}
	}
	return nil
}

// Read parses JSON Lines and checks the trace is ordered and well formed.
func Read(r io.Reader) ([]Event, error) {
	var events []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch {
		case e.Kind != KindClaim && e.Kind != KindHint:
			return nil, fmt.Errorf("line %d: unknown kind %q", line, e.Kind)
		case e.AtMs < 0:
			return nil, fmt.Errorf("line %d: negative atMs", line)
		case len(events) > 0 && e.AtMs < events[len(events)-1].AtMs:
			return nil, fmt.Errorf("line %d: events out of order", line)
		case e.Kind == KindClaim && e.HoldMs <= 0:
			return nil, fmt.Errorf("line %d: claim needs positive holdMs", line)
		case e.Kind == KindHint && (e.Replicas <= 0 || e.TTLSeconds <= 0 || e.NeededInMs < 0):
			return nil, fmt.Errorf("line %d: hint needs positive replicas and ttlSeconds", line)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read trace: %w", err)
	}
	return events, nil
}
