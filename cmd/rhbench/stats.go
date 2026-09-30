package main

import (
	"fmt"
	"slices"
	"time"
)

// Summary describes a set of latency samples.
type Summary struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean_ms"`
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
}

// summarize computes nearest-rank percentiles of samples, in milliseconds.
func summarize(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	var total time.Duration
	for _, s := range sorted {
		total += s
	}
	return Summary{
		N:    len(sorted),
		Mean: ms(total / time.Duration(len(sorted))),
		P50:  ms(percentile(sorted, 50)),
		P90:  ms(percentile(sorted, 90)),
		P99:  ms(percentile(sorted, 99)),
		Max:  ms(sorted[len(sorted)-1]),
	}
}

// percentile returns the nearest-rank p-th percentile of sorted samples.
func percentile(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	return sorted[max(rank, 1)-1]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (s Summary) String() string {
	return fmt.Sprintf("n=%d p50=%.2fms p90=%.2fms p99=%.2fms max=%.2fms mean=%.2fms", s.N, s.P50, s.P90, s.P99, s.Max, s.Mean)
}
