// Package loadstat summarises what a load test measured: latencies as percentiles and
// throughput as a rate. It holds no chain logic, so the numbers a load test reports are
// computed by code that is tested on its own.
package loadstat

import (
	"fmt"
	"math"
	"slices"
	"time"
)

// Summary is a set of latencies reduced to what a reader compares between runs.
type Summary struct {
	Count         int
	P50, P95, P99 time.Duration
	Max           time.Duration
}

// Summarize returns the percentiles of the given latencies, by the nearest-rank method:
// the p-th percentile is the smallest value with at least p percent of the values at or
// below it. Every reported number is a latency that was measured. The input is not
// modified.
func Summarize(latencies []time.Duration) Summary {
	if len(latencies) == 0 {
		return Summary{}
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	return Summary{
		Count: len(sorted),
		P50:   rank(sorted, 50),
		P95:   rank(sorted, 95),
		P99:   rank(sorted, 99),
		Max:   sorted[len(sorted)-1],
	}
}

func rank(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

// String renders the summary in seconds with one decimal.
func (s Summary) String() string {
	if s.Count == 0 {
		return "no samples"
	}
	return fmt.Sprintf("p50 %s, p95 %s, p99 %s, max %s", seconds(s.P50), seconds(s.P95), seconds(s.P99), seconds(s.Max))
}

// Rate is count events over d, per second. A zero or negative span gives zero.
func Rate(count int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(count) / d.Seconds()
}

func seconds(d time.Duration) string {
	return fmt.Sprintf("%.1f s", d.Seconds())
}
