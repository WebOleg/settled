package loadstat

import (
	"testing"
	"time"
)

func ms(v ...int) []time.Duration {
	out := make([]time.Duration, len(v))
	for i, x := range v {
		out[i] = time.Duration(x) * time.Millisecond
	}
	return out
}

func TestSummarizeNearestRank(t *testing.T) {
	// 1..100 ms: the p-th percentile of 100 values is the p-th value.
	values := make([]int, 100)
	for i := range values {
		values[100-1-i] = i + 1 // reversed, so sorting is exercised
	}
	got := Summarize(ms(values...))
	want := Summary{Count: 100, P50: 50 * time.Millisecond, P95: 95 * time.Millisecond, P99: 99 * time.Millisecond, Max: 100 * time.Millisecond}
	if got != want {
		t.Fatalf("Summarize = %+v, want %+v", got, want)
	}
}

func TestSummarizeSmallSets(t *testing.T) {
	cases := []struct {
		in   []time.Duration
		want Summary
	}{
		{nil, Summary{}},
		{ms(7), Summary{Count: 1, P50: 7 * time.Millisecond, P95: 7 * time.Millisecond, P99: 7 * time.Millisecond, Max: 7 * time.Millisecond}},
		{ms(1, 2, 3, 4), Summary{Count: 4, P50: 2 * time.Millisecond, P95: 4 * time.Millisecond, P99: 4 * time.Millisecond, Max: 4 * time.Millisecond}},
	}
	for _, c := range cases {
		if got := Summarize(c.in); got != c.want {
			t.Errorf("Summarize(%v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestSummarizeKeepsInput(t *testing.T) {
	in := ms(3, 1, 2)
	Summarize(in)
	if in[0] != 3*time.Millisecond || in[1] != 1*time.Millisecond || in[2] != 2*time.Millisecond {
		t.Fatalf("input was modified: %v", in)
	}
}

func TestString(t *testing.T) {
	s := Summarize(ms(1000, 2000, 3000, 4000))
	if got, want := s.String(), "p50 2.0 s, p95 4.0 s, p99 4.0 s, max 4.0 s"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if got := (Summary{}).String(); got != "no samples" {
		t.Fatalf("empty String = %q", got)
	}
}

func TestRate(t *testing.T) {
	if got := Rate(6000, 2*time.Minute); got != 50 {
		t.Fatalf("Rate = %v, want 50", got)
	}
	if got := Rate(10, 0); got != 0 {
		t.Fatalf("Rate over zero span = %v, want 0", got)
	}
}
