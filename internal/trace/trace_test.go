package trace

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func baseConfig() Config {
	return Config{
		Seed: 7, Duration: 10 * time.Minute, BaseRate: 0.2,
		BurstEvery: 2 * time.Minute, BurstJitter: 0.1, BurstSize: 30, BurstDuration: 10 * time.Second,
		Hold: time.Minute, HintFraction: 1, HintLead: 30 * time.Second, HintTTL: 2 * time.Minute,
	}
}

func TestGenerateDeterministicAndSorted(t *testing.T) {
	a, err := Generate(baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate(baseConfig())
	if len(a) != len(b) {
		t.Fatalf("same seed, different lengths %d vs %d", len(a), len(b))
	}
	claims, hints := 0, 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("event %d differs", i)
		}
		if i > 0 && a[i].AtMs < a[i-1].AtMs {
			t.Fatalf("unsorted at %d", i)
		}
		switch a[i].Kind {
		case KindClaim:
			claims++
			if a[i].HoldMs < 30_000 || a[i].HoldMs > 90_000 {
				t.Fatalf("hold %d out of range", a[i].HoldMs)
			}
		case KindHint:
			hints++
			if a[i].Replicas != 30 || a[i].NeededInMs != 30_000 || a[i].TTLSeconds != 120 {
				t.Fatalf("bad hint %+v", a[i])
			}
		}
	}
	// 4 bursts (2,4,6,8 min) of 30 plus ~120 background (0.2/s for 600s)
	if hints != 4 {
		t.Fatalf("hints = %d, want 4", hints)
	}
	if claims < 120+60 || claims > 120+180 {
		t.Fatalf("claims = %d, implausible", claims)
	}
	c := baseConfig()
	c.Seed = 8
	other, _ := Generate(c)
	if len(other) == len(a) && other[0] == a[0] {
		t.Fatal("different seeds produced the same trace")
	}
}

func TestGenerateNoHints(t *testing.T) {
	c := baseConfig()
	c.HintFraction = 0
	ev, err := Generate(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ev {
		if e.Kind == KindHint {
			t.Fatal("hint generated with HintFraction 0")
		}
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"duration": func(c *Config) { c.Duration = 0 },
		"rate":     func(c *Config) { c.BaseRate = -1 },
		"jitter":   func(c *Config) { c.BurstJitter = 0.5 },
		"hold":     func(c *Config) { c.Hold = 0 },
		"fraction": func(c *Config) { c.HintFraction = 2 },
		"ttl":      func(c *Config) { c.HintTTL = 0 },
	} {
		c := baseConfig()
		mut(&c)
		if _, err := Generate(c); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	ev, _ := Generate(baseConfig())
	var buf bytes.Buffer
	if err := Write(&buf, ev); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ev) {
		t.Fatalf("round trip lost events: %d vs %d", len(got), len(ev))
	}
}

func TestReadRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"kind":     `{"atMs":1,"kind":"x"}`,
		"order":    "{\"atMs\":5,\"kind\":\"claim\",\"holdMs\":1}\n{\"atMs\":1,\"kind\":\"claim\",\"holdMs\":1}",
		"hold":     `{"atMs":1,"kind":"claim"}`,
		"hint":     `{"atMs":1,"kind":"hint","replicas":0,"ttlSeconds":1}`,
		"json":     `{`,
		"negative": `{"atMs":-1,"kind":"claim","holdMs":1}`,
	} {
		if _, err := Read(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestSummarize(t *testing.T) {
	t0 := time.Unix(0, 0)
	r := &Result{
		Claims: []ClaimResult{
			{CreatedAt: t0, ReadyAt: t0.Add(100 * time.Millisecond), LaunchType: "warm"},
			{CreatedAt: t0, ReadyAt: t0.Add(200 * time.Millisecond), LaunchType: "warm"},
			{CreatedAt: t0, ReadyAt: t0.Add(5 * time.Second), LaunchType: "cold"},
			{CreatedAt: t0, ReadyAt: t0.Add(300 * time.Millisecond)}, // unknown launch type
			{CreatedAt: t0}, // timed out
		},
		Samples: []PoolSample{
			{At: t0, Replicas: 10, ReadyReplicas: 6},
			{At: t0.Add(time.Minute), Replicas: 4, ReadyReplicas: 4, ObservationLost: true},
			{At: t0.Add(2 * time.Minute), Replicas: 4, ReadyReplicas: 4},
		},
	}
	s := Summarize(r)
	if s.Claims != 5 || s.Ready != 4 || s.TimedOut != 1 || s.ColdStart != 1 {
		t.Fatalf("counts = %+v", s)
	}
	if math.Abs(s.ColdStartRate-1.0/3) > 1e-9 {
		t.Fatalf("cold rate = %v", s.ColdStartRate)
	}
	if s.P50Ms != 200 || s.P95Ms != 5000 || s.P99Ms != 5000 {
		t.Fatalf("percentiles = %v %v %v", s.P50Ms, s.P95Ms, s.P99Ms)
	}
	// second interval starts at a lost observation and is skipped
	if s.IdleSandboxMinutes != 10 || s.IdleReadySandboxMinutes != 6 {
		t.Fatalf("idle = %v / %v", s.IdleSandboxMinutes, s.IdleReadySandboxMinutes)
	}
	if !math.IsNaN(Percentile(nil, 50)) {
		t.Fatal("empty percentile should be NaN")
	}
}
