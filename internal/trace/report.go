package trace

import (
	"fmt"
	"io"
	"math"
	"slices"
)

// aggregate is the mean and spread of one metric over repeated runs.
type aggregate struct{ mean, min, max float64 }

func agg(vals []float64) aggregate {
	a := aggregate{min: math.Inf(1), max: math.Inf(-1)}
	n := 0
	for _, v := range vals {
		if math.IsNaN(v) {
			continue
		}
		a.mean += v
		a.min = math.Min(a.min, v)
		a.max = math.Max(a.max, v)
		n++
	}
	if n == 0 {
		return aggregate{mean: math.NaN(), min: math.NaN(), max: math.NaN()}
	}
	a.mean /= float64(n)
	return a
}

func (a aggregate) format(scale float64, decimals int) string {
	if math.IsNaN(a.mean) {
		return "n/a"
	}
	if a.min == a.max {
		return fmt.Sprintf("%.*f", decimals, a.mean*scale)
	}
	return fmt.Sprintf("%.*f (%.*f–%.*f)", decimals, a.mean*scale, decimals, a.min*scale, decimals, a.max*scale)
}

// WriteReport renders one markdown table per trace with a row per strategy. Repeated runs of
// the same trace and strategy (different seeds or repetitions) are averaged, with min–max shown.
func WriteReport(w io.Writer, results []*Result) error {
	type key struct{ trace, strategy string }
	groups := map[key][]*Result{}
	traces := map[string]bool{}
	for _, r := range results {
		k := key{r.Trace, r.Strategy}
		groups[k] = append(groups[k], r)
		traces[r.Trace] = true
	}
	traceNames := make([]string, 0, len(traces))
	for t := range traces {
		traceNames = append(traceNames, t)
	}
	slices.Sort(traceNames)

	for _, tn := range traceNames {
		var strategies []string
		for k := range groups {
			if k.trace == tn {
				strategies = append(strategies, k.strategy)
			}
		}
		slices.Sort(strategies)
		if _, err := fmt.Fprintf(w, "### %s\n\n| Strategy | Runs | Claims | p50 ms | p95 ms | p99 ms | Cold-start rate %% | Timed out | Idle sandbox-min | Idle ready sandbox-min |\n|---|---|---|---|---|---|---|---|---|---|\n", tn); err != nil {
			return err
		}
		for _, sn := range strategies {
			runs := groups[key{tn, sn}]
			col := func(f func(Summary) float64) aggregate {
				vals := make([]float64, len(runs))
				for i, r := range runs {
					vals[i] = f(r.Summary)
				}
				return agg(vals)
			}
			if _, err := fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s |\n", sn, len(runs),
				col(func(s Summary) float64 { return float64(s.Claims) }).format(1, 0),
				col(func(s Summary) float64 { return s.P50Ms }).format(1, 0),
				col(func(s Summary) float64 { return s.P95Ms }).format(1, 0),
				col(func(s Summary) float64 { return s.P99Ms }).format(1, 0),
				col(func(s Summary) float64 { return s.ColdStartRate }).format(100, 1),
				col(func(s Summary) float64 { return float64(s.TimedOut) }).format(1, 0),
				col(func(s Summary) float64 { return s.IdleSandboxMinutes }).format(1, 1),
				col(func(s Summary) float64 { return s.IdleReadySandboxMinutes }).format(1, 1),
			); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
}
