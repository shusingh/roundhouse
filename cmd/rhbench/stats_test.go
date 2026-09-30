package main

import (
	"testing"
	"time"
)

func TestSummarizeUsesNearestRank(t *testing.T) {
	t.Parallel()
	var samples []time.Duration
	for i := 100; i >= 1; i-- { // unsorted input
		samples = append(samples, time.Duration(i)*time.Millisecond)
	}
	s := summarize(samples)
	if s.N != 100 || s.P50 != 50 || s.P90 != 90 || s.P99 != 99 || s.Max != 100 || s.Mean != 50.5 {
		t.Errorf("summary = %+v, want p50 50, p90 90, p99 99, max 100, mean 50.5", s)
	}
	if one := summarize([]time.Duration{7 * time.Millisecond}); one.P50 != 7 || one.P99 != 7 {
		t.Errorf("single-sample summary = %+v", one)
	}
	if empty := summarize(nil); empty.N != 0 {
		t.Errorf("empty summary = %+v", empty)
	}
}

func TestPoolIdleParsesTheMetric(t *testing.T) {
	t.Parallel()
	body := `# HELP roundhouse_pool_sandboxes x
roundhouse_pool_sandboxes{image="default",memory_mib="256",state="idle",vcpus="1"} 3
roundhouse_pool_sandboxes{image="default",memory_mib="256",state="starting",vcpus="1"} 1
roundhouse_pool_sandboxes{image="default",memory_mib="512",state="idle",vcpus="1"} 9
`
	if n := poolIdle(body, "default", 1, 256); n != 3 {
		t.Errorf("poolIdle = %d, want 3", n)
	}
	if n := poolIdle(body, "default", 2, 256); n != 0 {
		t.Errorf("poolIdle for an absent pool = %d, want 0", n)
	}
}

func TestPSSParsesSmapsRollup(t *testing.T) {
	t.Parallel()
	rollup := "00400000-7fff0000 ---p 00000000 00:00 0 [rollup]\nRss:              262144 kB\nPss:               40960 kB\nPss_Anon:          10240 kB\n"
	if kib := fieldKiB(rollup, "Pss:"); kib != 40960 {
		t.Errorf("Pss = %d kB, want 40960", kib)
	}
	if kib := fieldKiB(rollup, "Rss:"); kib != 262144 {
		t.Errorf("Rss = %d kB, want 262144", kib)
	}
}
