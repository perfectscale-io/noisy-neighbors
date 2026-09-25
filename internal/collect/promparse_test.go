package collect

import "testing"

// The parser only ever reads kubelet output, but kubelet output is where the
// throttling numbers come from, so a mistake here is silent and wrong rather
// than loud and wrong.
func TestParsePromText(t *testing.T) {
	input := []byte(`# HELP container_cpu_cfs_throttled_periods_total Number of throttled period intervals.
# TYPE container_cpu_cfs_throttled_periods_total counter
container_cpu_cfs_throttled_periods_total{container="worker",namespace="team-billing",pod="worker-abc"} 4211
container_cpu_cfs_periods_total{container="worker",namespace="team-billing",pod="worker-abc"} 4600
container_cpu_cfs_periods_total{container="POD",namespace="team-billing",pod="worker-abc"} 99
container_memory_working_set_bytes{container="api",namespace="team-search",pod="api-xyz"} 1.34217728e+08
node_cpu_usage_seconds_total 12345.678
some_other_metric{foo="bar"} 1
`)

	wanted := map[string]bool{
		metricThrottledPeriods: true,
		metricPeriods:          true,
		metricContainerMemWS:   true,
		metricNodeCPUSeconds:   true,
	}

	samples := parsePromText(input, wanted)
	if len(samples) != 5 {
		t.Fatalf("expected 5 samples after filtering, got %d", len(samples))
	}

	byName := map[string][]sample{}
	for _, s := range samples {
		byName[s.Name] = append(byName[s.Name], s)
	}

	if got := byName[metricThrottledPeriods][0].Value; got != 4211 {
		t.Errorf("throttled periods: want 4211, got %v", got)
	}
	if got := byName[metricThrottledPeriods][0].Labels["namespace"]; got != "team-billing" {
		t.Errorf("namespace label: want team-billing, got %q", got)
	}

	// Scientific notation is how the kubelet renders large byte counts.
	if got := byName[metricContainerMemWS][0].Value; got != 134217728 {
		t.Errorf("working set: want 134217728, got %v", got)
	}

	// A metric with no labels at all must still parse.
	if got := byName[metricNodeCPUSeconds][0].Value; got != 12345.678 {
		t.Errorf("node cpu: want 12345.678, got %v", got)
	}
	if len(byName[metricNodeCPUSeconds][0].Labels) != 0 {
		t.Errorf("expected no labels on node metric")
	}
}

// The pause container reports the pod-level rollup under container="POD" (and
// container="" on some kubelet versions). Counting it would double every pod.
func TestContainerKeySkipsPodRollup(t *testing.T) {
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"namespace": "a", "pod": "b", "container": "c"}, "a/b/c"},
		{map[string]string{"namespace": "a", "pod": "b", "container": "POD"}, ""},
		{map[string]string{"namespace": "a", "pod": "b", "container": ""}, ""},
		{map[string]string{"namespace": "a", "pod": "b"}, ""},
		{map[string]string{"pod": "b", "container": "c"}, ""},
	}
	for _, tc := range cases {
		if got := containerKey(tc.labels); got != tc.want {
			t.Errorf("containerKey(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

func TestSplitContainerKey(t *testing.T) {
	ns, pod, ctr, ok := splitContainerKey("team-search/api-xyz/api")
	if !ok || ns != "team-search" || pod != "api-xyz" || ctr != "api" {
		t.Errorf("got %q %q %q ok=%v", ns, pod, ctr, ok)
	}
	if _, _, _, ok := splitContainerKey("nope"); ok {
		t.Error("expected failure on malformed key")
	}
}

func TestParseLabelsWithEscapes(t *testing.T) {
	labels, rest := parseLabels(`{a="x\"y",b="z"} 42`)
	if labels["a"] != `x"y` {
		t.Errorf("escaped quote: got %q", labels["a"])
	}
	if labels["b"] != "z" {
		t.Errorf("second label: got %q", labels["b"])
	}
	if rest != " 42" {
		t.Errorf("remainder: got %q", rest)
	}
}
