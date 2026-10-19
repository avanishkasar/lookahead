package trace

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestWriteReport(t *testing.T) {
	results := []*Result{
		{Trace: "bursty", Strategy: "static", Summary: Summary{Claims: 10, P50Ms: 100, P95Ms: 4000, P99Ms: 5000, ColdStartRate: 0.4, IdleSandboxMinutes: 2}},
		{Trace: "bursty", Strategy: "static", Summary: Summary{Claims: 10, P50Ms: 200, P95Ms: 6000, P99Ms: 7000, ColdStartRate: 0.6, IdleSandboxMinutes: 4}},
		{Trace: "bursty", Strategy: "hints", Summary: Summary{Claims: 10, P50Ms: 90, P95Ms: 150, P99Ms: math.NaN(), ColdStartRate: 0}},
		{Trace: "steady", Strategy: "static", Summary: Summary{Claims: 5}},
	}
	var buf bytes.Buffer
	if err := WriteReport(&buf, results); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"### bursty", "### steady",
		"| static | 2 | 10 | 150 (100–200) | 5000 (4000–6000) | 6000 (5000–7000) | 50.0 (40.0–60.0) |",
		"| hints | 1 | 10 | 90 | 150 | n/a | 0.0 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "### bursty") > strings.Index(out, "### steady") {
		t.Error("traces not sorted")
	}
}
