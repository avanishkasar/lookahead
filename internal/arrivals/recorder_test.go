package arrivals

import (
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.t = t }

func TestSeriesBucketsAndExcludesPartialBucket(t *testing.T) {
	base := time.Unix(10_000, 0) // multiple of 5
	c := &clock{t: base.Add(17 * time.Second)}
	r := NewRecorder(c.now)
	k := Key{Namespace: "ns", Pool: "p"}

	r.Record(k, base.Add(1*time.Second))  // bucket [10000,10005)
	r.Record(k, base.Add(4*time.Second))  // same bucket
	r.Record(k, base.Add(6*time.Second))  // [10005,10010)
	r.Record(k, base.Add(16*time.Second)) // [10015,10020) partial at now=10017

	got := r.Series(k, c.now(), 5*time.Second, 20*time.Second)
	// end = 10015, start = 9995: buckets [9995,10000) [10000,10005) [10005,10010) [10010,10015)
	want := []float64{0, 2, 1, 0}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Series = %v, want %v", got, want)
		}
	}
}

func TestRecordDropsStaleAndClampsFuture(t *testing.T) {
	now := time.Unix(100_000, 0)
	r := NewRecorder(func() time.Time { return now })
	k := Key{Namespace: "ns", Pool: "p"}
	r.Record(k, now.Add(-MaxHistory))    // dropped
	r.Record(k, now.Add(10*time.Minute)) // counted at now
	got := r.Series(k, now.Add(time.Second), time.Second, 2*time.Second)
	if got[0]+got[1] != 1 {
		t.Fatalf("Series = %v, want exactly one arrival", got)
	}
}

func TestRingSlotReuse(t *testing.T) {
	c := &clock{t: time.Unix(50_000, 0)}
	r := NewRecorder(c.now)
	k := Key{Namespace: "ns", Pool: "p"}
	r.Record(k, c.now())
	// One full ring later the same slot holds a new second; the old count must not leak.
	later := c.now().Add(MaxHistory)
	c.set(later)
	r.Record(k, later)
	got := r.Series(k, later.Add(time.Second), time.Second, MaxHistory)
	var sum float64
	for _, v := range got {
		sum += v
	}
	if sum != 1 {
		t.Fatalf("sum = %v, want 1", sum)
	}
}

func TestUnknownKeyAndForget(t *testing.T) {
	now := time.Unix(1_000, 0)
	r := NewRecorder(func() time.Time { return now })
	k := Key{Namespace: "ns", Pool: "p"}
	if got := r.Series(k, now, 5*time.Second, 10*time.Second); len(got) != 2 || got[0] != 0 || got[1] != 0 {
		t.Fatalf("unknown key = %v", got)
	}
	r.Record(k, now.Add(-7*time.Second))
	r.Forget(k)
	if got := r.Series(k, now, 5*time.Second, 10*time.Second); got[0]+got[1] != 0 {
		t.Fatalf("after Forget = %v", got)
	}
	if got := r.Series(k, now, 5*time.Second, time.Second); got != nil {
		t.Fatalf("history shorter than bucket = %v, want nil", got)
	}
}

func TestConcurrentRecord(t *testing.T) {
	now := time.Unix(200_000, 0)
	r := NewRecorder(func() time.Time { return now })
	k := Key{Namespace: "ns", Pool: "p"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for j := range 1000 {
				r.Record(k, now.Add(-time.Duration(j%30)*time.Second))
				_ = r.Series(k, now, 5*time.Second, time.Minute)
			}
		})
	}
	wg.Wait()
	var sum float64
	for _, v := range r.Series(k, now.Add(5*time.Second), 5*time.Second, time.Minute) {
		sum += v
	}
	if sum != 8000 {
		t.Fatalf("sum = %v, want 8000", sum)
	}
}
