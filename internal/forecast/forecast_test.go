package forecast

import (
	"math"
	"testing"
	"time"
)

func TestSmooth(t *testing.T) {
	tests := []struct {
		name      string
		series    []float64
		p         Params
		wantLevel float64
		wantTrend float64
	}{
		{name: "empty", series: nil, p: Params{Alpha: 0.5, Beta: 0.1}},
		{name: "single", series: []float64{4}, p: Params{Alpha: 0.5, Beta: 0.1}, wantLevel: 4},
		{name: "constant ewma", series: []float64{3, 3, 3, 3}, p: Params{Alpha: 0.3}, wantLevel: 3},
		{
			// level1 = 0.5*2 + 0.5*(0+0) = 1; trend1 = 1*(1-0) + 0 = 1
			// level2 = 0.5*4 + 0.5*(1+1) = 3; trend2 = 1*(3-1) = 2
			name: "holt hand computed", series: []float64{0, 2, 4}, p: Params{Alpha: 0.5, Beta: 1},
			wantLevel: 3, wantTrend: 2,
		},
		{
			name: "ewma ignores trend", series: []float64{0, 10}, p: Params{Alpha: 0.5, Beta: 0},
			wantLevel: 5, wantTrend: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, tr := Smooth(tc.series, tc.p)
			if math.Abs(l-tc.wantLevel) > 1e-9 || math.Abs(tr-tc.wantTrend) > 1e-9 {
				t.Fatalf("Smooth() = (%v, %v), want (%v, %v)", l, tr, tc.wantLevel, tc.wantTrend)
			}
		})
	}
}

func TestExpectedArrivalsFloorsNegativeBuckets(t *testing.T) {
	// level 2, trend -1: buckets 1, 0, -1 -> 1 + 0 + 0
	if got := ExpectedArrivals(2, -1, 3); got != 1 {
		t.Fatalf("ExpectedArrivals = %v, want 1", got)
	}
	if got := ExpectedArrivals(1, 0.5, 2); got != 1.5+2 {
		t.Fatalf("ExpectedArrivals = %v, want 3.5", got)
	}
	if got := ExpectedArrivals(5, 0, 0); got != 0 {
		t.Fatalf("zero horizon = %v, want 0", got)
	}
}

func TestReplicas(t *testing.T) {
	tests := []struct {
		expected, z float64
		want        int32
	}{
		{0, 1, 0},
		{-3, 1, 0},
		{math.NaN(), 1, 0},
		{4, 0, 4},
		{4, 1, 6},    // 4 + 2
		{0.1, 0, 1},  // any expected demand rounds up to one sandbox
		{9, 1.5, 14}, // 9 + 4.5 -> 13.5 -> 14
		{1e12, 0, math.MaxInt32},
	}
	for _, tc := range tests {
		if got := Replicas(tc.expected, tc.z); got != tc.want {
			t.Errorf("Replicas(%v, %v) = %d, want %d", tc.expected, tc.z, got, tc.want)
		}
	}
}

func TestClamp(t *testing.T) {
	if got := Clamp(5, 0, 3); got != 3 {
		t.Errorf("upper clamp = %d", got)
	}
	if got := Clamp(1, 2, 3); got != 2 {
		t.Errorf("lower clamp = %d", got)
	}
	if got := Clamp(5, 4, 2); got != 2 {
		t.Errorf("max wins over min = %d", got)
	}
	if got := Clamp(math.MaxInt64, 0, math.MaxInt64); got != math.MaxInt32 {
		t.Errorf("int32 overflow = %d", got)
	}
}

func TestHorizonBuckets(t *testing.T) {
	cases := []struct {
		lead, bucket int32
		want         int
	}{
		{30, 5, 6}, {31, 5, 7}, {1, 5, 1}, {0, 5, 1}, {30, 0, 1},
	}
	for _, c := range cases {
		if got := HorizonBuckets(c.lead, c.bucket); got != c.want {
			t.Errorf("HorizonBuckets(%d,%d) = %d, want %d", c.lead, c.bucket, got, c.want)
		}
	}
}

func TestStabilizer(t *testing.T) {
	s := NewStabilizer()
	t0 := time.Unix(1000, 0)
	w := 60 * time.Second
	if s.Seeded("a") {
		t.Fatal("new stabilizer is seeded")
	}
	if got := s.Recommend("a", t0, 10, w); got != 10 {
		t.Fatalf("first = %d", got)
	}
	if got := s.Recommend("a", t0.Add(10*time.Second), 3, w); got != 10 {
		t.Fatalf("scale down within window = %d, want 10", got)
	}
	if got := s.Recommend("a", t0.Add(20*time.Second), 15, w); got != 15 {
		t.Fatalf("scale up = %d, want 15", got)
	}
	if got := s.Recommend("a", t0.Add(81*time.Second), 2, w); got != 2 {
		t.Fatalf("after window = %d, want 2", got)
	}
	if got := s.Recommend("b", t0, 1, w); got != 1 {
		t.Fatalf("keys are independent: %d", got)
	}
	if got := s.Recommend("a", t0.Add(82*time.Second), 0, 0); got != 0 {
		t.Fatalf("zero window = %d, want 0", got)
	}
	s.Forget("a")
	if s.Seeded("a") {
		t.Fatal("forget did not drop state")
	}
}
