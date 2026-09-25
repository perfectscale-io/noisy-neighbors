// Package analyze turns a collected snapshot into the aggressor/victim picture.
//
// The central claim of the report is relational: not "this workload is
// misconfigured" (plenty of tools say that) but "this workload's behavior is
// landing on these other tenants". That claim is only as strong as the data
// underneath it, so every finding records the tier it came from and the report
// is careful about the difference between co-location and causation.
package analyze

import (
	"fmt"
	"sort"
	"time"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Severity ranks findings.
type Severity int

const (
	SevInfo Severity = iota
	SevWarn
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "CRITICAL"
	case SevWarn:
		return "WARN"
	}
	return "INFO"
}

// Thresholds are the tunable knobs. Defaults are deliberately conservative:
// a report that cries wolf on a healthy cluster is worse than one that misses
// a marginal case.
type Thresholds struct {
	// NodeSaturationPct is the actual-usage level at which a node is treated as
	// saturated and its tenants as exposed.
	NodeSaturationPct float64
	// NodeCommitmentPct is the requested level at which a node is full on paper.
	NodeCommitmentPct float64
	// AggressorRatio is usage/request above which a workload is over-consuming.
	AggressorRatio float64
	// StrandRatio is request/usage above which a workload is holding capacity
	// it does not use.
	StrandRatio float64
	// MinCPUMilli and MinMemBytes suppress findings on workloads too small to
	// matter, which would otherwise dominate the output on any real cluster.
	MinCPUMilli int64
	MinMemBytes int64
	// ThrottleWarnPct and ThrottleCritPct are CFS throttling levels.
	ThrottleWarnPct float64
	ThrottleCritPct float64
	// OOMWindow is how far back a lastState OOMKill still counts as recent.
	OOMWindow time.Duration
}

// DefaultThresholds returns the built-in tuning.
func DefaultThresholds() Thresholds {
	return Thresholds{
		NodeSaturationPct: 85,
		NodeCommitmentPct: 90,
		AggressorRatio:    1.5,
		StrandRatio:       2.0,
		MinCPUMilli:       100,
		MinMemBytes:       256 * 1024 * 1024,
		ThrottleWarnPct:   5,
		ThrottleCritPct:   25,
		OOMWindow:         24 * time.Hour,
	}
}

// Finding is one observation about one workload or tenant.
type Finding struct {
	Severity Severity   `json:"severity"`
	Kind     string     `json:"kind"`
	Tenant   string     `json:"tenant"`
	Workload string     `json:"workload,omitempty"`
	Node     string     `json:"node,omitempty"`
	Message  string     `json:"message"`
	Tier     model.Tier `json:"-"`
	TierName string     `json:"tier"`
}

// Skipped records an analysis that could not run, and what would enable it.
// This is the visible half of graceful degradation: the report says what it does
// not know.
type Skipped struct {
	Analysis string `json:"analysis"`
	Reason   string `json:"reason"`
	Hint     string `json:"hint,omitempty"`
}

// TenantShare is one namespace's footprint on one node.
type TenantShare struct {
	Namespace string           `json:"namespace"`
	Pods      int              `json:"pods"`
	Requests  model.Resources  `json:"requests"`
	Limits    model.Resources  `json:"limits"`
	Usage     *model.Resources `json:"usage,omitempty"`
}

// NodeAnalysis is the per-node blast radius.
type NodeAnalysis struct {
	Name        string   `json:"node"`
	Severity    Severity `json:"severity"`
	SeverityStr string   `json:"severityLabel"`

	Allocatable model.Resources `json:"allocatable"`

	RequestedCPUPct float64  `json:"requestedCpuPct"`
	RequestedMemPct float64  `json:"requestedMemPct"`
	LimitCPUPct     float64  `json:"limitCpuPct"`
	LimitMemPct     float64  `json:"limitMemPct"`
	ActualCPUPct    *float64 `json:"actualCpuPct,omitempty"`
	ActualMemPct    *float64 `json:"actualMemPct,omitempty"`

	TenantCount int           `json:"tenantCount"`
	Tenants     []TenantShare `json:"tenants"`

	Aggressors []Finding `json:"aggressors"`
	Exposed    []Finding `json:"exposed"`

	MemoryPressure bool `json:"memoryPressure"`
}

// TenantAnalysis is the per-namespace rollup.
type TenantAnalysis struct {
	Namespace string   `json:"namespace"`
	Pods      int      `json:"pods"`
	Nodes     []string `json:"nodes"`

	Requests model.Resources  `json:"requests"`
	Limits   model.Resources  `json:"limits"`
	Usage    *model.Resources `json:"usage,omitempty"`

	StrandedCPUMilli int64 `json:"strandedCpuMilli"`
	StrandedMemBytes int64 `json:"strandedMemBytes"`
	OverCPUMilli     int64 `json:"overCpuMilli"`
	OverMemBytes     int64 `json:"overMemBytes"`

	RecentOOMKills      int   `json:"recentOomKills"`
	ThrottledContainers int   `json:"throttledContainers"`
	Restarts            int32 `json:"restarts"`

	Guaranteed int `json:"guaranteed"`
	Burstable  int `json:"burstable"`
	BestEffort int `json:"bestEffort"`

	MissingRequests int  `json:"missingRequests"`
	MissingLimits   int  `json:"missingLimits"`
	HasQuota        bool `json:"hasQuota"`
	HasLimitRange   bool `json:"hasLimitRange"`
}

// Report is the full analysis.
type Report struct {
	Cluster *model.Cluster `json:"cluster"`

	Nodes   []NodeAnalysis   `json:"nodes"`
	Tenants []TenantAnalysis `json:"tenants"`

	ClusterFindings []Finding `json:"clusterFindings"`
	QuotaFindings   []Finding `json:"quotaFindings"`
	Skipped         []Skipped `json:"skipped"`

	Thresholds Thresholds `json:"thresholds"`
}

// Run produces the analysis.
func Run(cluster *model.Cluster, th Thresholds) *Report {
	r := &Report{Cluster: cluster, Thresholds: th}

	hasUsage := cluster.Caps.UsageAvailable()
	hasThrottle := cluster.Caps.Kubelet.Available && cluster.Usage.Windowed

	r.Skipped = skippedAnalyses(cluster, hasUsage, hasThrottle)
	r.Nodes = analyzeNodes(cluster, th, hasUsage)
	r.Tenants = analyzeTenants(cluster, th)
	r.QuotaFindings = analyzeQuotas(cluster, th)
	r.ClusterFindings = analyzeCluster(cluster, th)

	return r
}

func skippedAnalyses(cluster *model.Cluster, hasUsage, hasThrottle bool) []Skipped {
	var out []Skipped

	if !hasUsage {
		reason := "no usage source available"
		if m := cluster.Caps.Metrics.Reason; m != "" {
			reason += "; metrics-server: " + m
		}
		if k := cluster.Caps.Kubelet.Reason; k != "" {
			reason += "; kubelet: " + k
		}
		out = append(out, Skipped{
			Analysis: "actual usage vs requests (aggressors, stranded capacity, real node saturation)",
			Reason:   reason,
			Hint:     cluster.Caps.Metrics.Hint,
		})
	}
	if !cluster.Caps.Kubelet.Available {
		out = append(out, Skipped{
			Analysis: "CPU throttling, cumulative OOM counts, per-pod network",
			Reason:   "kubelet unreachable: " + cluster.Caps.Kubelet.Reason,
			Hint:     cluster.Caps.Kubelet.Hint,
		})
	} else if !hasThrottle {
		out = append(out, Skipped{
			Analysis: "CPU throttling and CPU usage rates",
			Reason:   "counters need two samples; --sample-window was zero",
			Hint:     "rerun with --sample-window 30s",
		})
	}

	out = append(out, Skipped{
		Analysis: "historical correlation (did the aggressor spike when the victim died?)",
		Reason:   "needs a time-series store; this tool takes a point-in-time sample, so findings assert co-location and structural risk rather than proven causation",
	})

	return out
}

func analyzeNodes(cluster *model.Cluster, th Thresholds, hasUsage bool) []NodeAnalysis {
	var out []NodeAnalysis

	for _, n := range cluster.Nodes {
		if len(n.Pods) == 0 {
			continue
		}

		na := NodeAnalysis{
			Name:           n.Name,
			Allocatable:    n.Allocatable,
			MemoryPressure: n.MemoryPressure,
		}

		var req, lim model.Resources
		var usage model.Resources
		sawUsage := false

		shares := map[string]*TenantShare{}
		for _, p := range n.Pods {
			req.Add(p.Requests)
			lim.Add(p.Limits)

			s, ok := shares[p.Namespace]
			if !ok {
				s = &TenantShare{Namespace: p.Namespace}
				shares[p.Namespace] = s
			}
			s.Pods++
			s.Requests.Add(p.Requests)
			s.Limits.Add(p.Limits)

			if p.Usage != nil {
				usage.Add(*p.Usage)
				sawUsage = true
				if s.Usage == nil {
					s.Usage = &model.Resources{}
				}
				s.Usage.Add(*p.Usage)
			}
		}

		na.RequestedCPUPct = model.Pct(req.CPUMilli, n.Allocatable.CPUMilli)
		na.RequestedMemPct = model.Pct(req.MemBytes, n.Allocatable.MemBytes)
		na.LimitCPUPct = model.Pct(lim.CPUMilli, n.Allocatable.CPUMilli)
		na.LimitMemPct = model.Pct(lim.MemBytes, n.Allocatable.MemBytes)

		// Prefer the node's own reported usage over the sum of its pods: the
		// difference between them is system overhead that belongs to no tenant.
		effective := usage
		if n.Usage != nil {
			effective = *n.Usage
			sawUsage = true
		}
		if sawUsage {
			cpu := model.Pct(effective.CPUMilli, n.Allocatable.CPUMilli)
			mem := model.Pct(effective.MemBytes, n.Allocatable.MemBytes)
			na.ActualCPUPct = &cpu
			na.ActualMemPct = &mem
		}

		for _, s := range shares {
			na.Tenants = append(na.Tenants, *s)
		}
		sort.Slice(na.Tenants, func(i, j int) bool {
			return na.Tenants[i].Requests.MemBytes > na.Tenants[j].Requests.MemBytes
		})
		na.TenantCount = len(n.TenantNamespaces())

		na.Aggressors = findAggressors(n, th, hasUsage)
		na.Exposed = findExposed(n, th, na)
		na.Severity = nodeSeverity(na, th)
		na.SeverityStr = na.Severity.String()

		out = append(out, na)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		return len(out[i].Aggressors)+len(out[i].Exposed) > len(out[j].Aggressors)+len(out[j].Exposed)
	})
	return out
}

// findAggressors identifies workloads consuming materially more than they
// declared. On a single-tenant node that is just a bad estimate; on a shared
// node it is capacity taken from a neighbor that the scheduler believed was
// free.
func findAggressors(n *model.Node, th Thresholds, hasUsage bool) []Finding {
	var out []Finding
	if !hasUsage {
		return nil
	}

	multiTenant := len(n.TenantNamespaces()) > 1

	for _, p := range n.Pods {
		if p.Usage == nil {
			continue
		}

		if p.Requests.MemBytes > 0 && p.Usage.MemBytes > th.MinMemBytes {
			ratio := float64(p.Usage.MemBytes) / float64(p.Requests.MemBytes)
			if ratio >= th.AggressorRatio {
				sev := SevWarn
				if multiTenant && ratio >= th.AggressorRatio*2 {
					sev = SevCritical
				}
				out = append(out, Finding{
					Severity: sev,
					Kind:     "aggressor-memory",
					Tenant:   p.Namespace,
					Workload: p.Key(),
					Node:     n.Name,
					Tier:     model.Tier1,
					TierName: model.Tier1.String(),
					Message: fmt.Sprintf("memory req %s, using %s (%.0f%% of request)",
						model.FormatMem(p.Requests.MemBytes), model.FormatMem(p.Usage.MemBytes), ratio*100),
				})
			}
		}

		if p.Requests.CPUMilli > 0 && p.Usage.CPUMilli > th.MinCPUMilli {
			ratio := float64(p.Usage.CPUMilli) / float64(p.Requests.CPUMilli)
			if ratio >= th.AggressorRatio {
				sev := SevWarn
				if multiTenant && ratio >= th.AggressorRatio*2 {
					sev = SevCritical
				}
				out = append(out, Finding{
					Severity: sev,
					Kind:     "aggressor-cpu",
					Tenant:   p.Namespace,
					Workload: p.Key(),
					Node:     n.Name,
					Tier:     model.Tier1,
					TierName: model.Tier1.String(),
					Message: fmt.Sprintf("cpu req %s, using %s (%.0f%% of request)",
						model.FormatCPU(p.Requests.CPUMilli), model.FormatCPU(p.Usage.CPUMilli), ratio*100),
				})
			}
		}

		// A pod with no memory request at all is scheduled as though it were
		// free. On a shared node that is the purest form of the problem.
		if p.Requests.MemBytes == 0 && p.Usage.MemBytes > th.MinMemBytes && multiTenant {
			out = append(out, Finding{
				Severity: SevCritical,
				Kind:     "aggressor-unrequested",
				Tenant:   p.Namespace,
				Workload: p.Key(),
				Node:     n.Name,
				Tier:     model.Tier1,
				TierName: model.Tier1.String(),
				Message: fmt.Sprintf("no memory request, using %s; scheduler treats this as free capacity",
					model.FormatMem(p.Usage.MemBytes)),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	return out
}

// findExposed identifies tenants bearing the consequences: recently OOMKilled,
// currently throttled, or simply sitting on a saturated node in a QoS class that
// gets evicted first.
func findExposed(n *model.Node, th Thresholds, na NodeAnalysis) []Finding {
	var out []Finding
	cutoff := time.Now().Add(-th.OOMWindow)
	saturated := na.ActualMemPct != nil && *na.ActualMemPct >= th.NodeSaturationPct
	multiTenant := len(n.TenantNamespaces()) > 1

	for _, p := range n.Pods {
		if t := p.LastOOMKill(); t != nil && t.After(cutoff) {
			out = append(out, Finding{
				Severity: SevCritical,
				Kind:     "exposed-oomkill",
				Tenant:   p.Namespace,
				Workload: p.Key(),
				Node:     n.Name,
				Tier:     model.Tier0,
				TierName: model.Tier0.String(),
				Message: fmt.Sprintf("OOMKilled %s ago (%d restarts); shares this node with %d other tenants",
					units(time.Since(*t)), p.Restarts(), otherTenants(n, p.Namespace)),
			})
		}

		if name, thr := p.WorstThrottle(); thr != nil && thr.Percent >= th.ThrottleWarnPct {
			sev := SevWarn
			if thr.Percent >= th.ThrottleCritPct {
				sev = SevCritical
			}
			out = append(out, Finding{
				Severity: sev,
				Kind:     "exposed-throttling",
				Tenant:   p.Namespace,
				Workload: p.Key() + ":" + name,
				Node:     n.Name,
				Tier:     model.Tier2,
				TierName: model.Tier2.String(),
				Message:  fmt.Sprintf("CPU throttled in %.0f%% of periods over the sample window", thr.Percent),
			})
		}

		if saturated && multiTenant && p.QoS == "BestEffort" {
			out = append(out, Finding{
				Severity: SevWarn,
				Kind:     "exposed-besteffort",
				Tenant:   p.Namespace,
				Workload: p.Key(),
				Node:     n.Name,
				Tier:     model.Tier0,
				TierName: model.Tier0.String(),
				Message:  "BestEffort pod on a saturated shared node: first in line for eviction",
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	return out
}

func nodeSeverity(na NodeAnalysis, th Thresholds) Severity {
	sev := SevInfo

	for _, f := range na.Aggressors {
		if f.Severity > sev {
			sev = f.Severity
		}
	}
	for _, f := range na.Exposed {
		if f.Severity > sev {
			sev = f.Severity
		}
	}

	if na.MemoryPressure {
		sev = SevCritical
	}
	if na.ActualMemPct != nil && *na.ActualMemPct >= th.NodeSaturationPct && na.TenantCount > 1 && sev < SevWarn {
		sev = SevWarn
	}
	return sev
}

func analyzeTenants(cluster *model.Cluster, th Thresholds) []TenantAnalysis {
	byNS := map[string]*TenantAnalysis{}
	nodesByNS := map[string]map[string]bool{}
	cutoff := time.Now().Add(-th.OOMWindow)

	quotaNS := map[string]bool{}
	for _, q := range cluster.Quotas {
		quotaNS[q.Namespace] = true
	}
	lrNS := map[string]bool{}
	for _, lr := range cluster.LimitRanges {
		lrNS[lr.Namespace] = true
	}

	for _, p := range cluster.Pods {
		t, ok := byNS[p.Namespace]
		if !ok {
			t = &TenantAnalysis{
				Namespace:     p.Namespace,
				HasQuota:      quotaNS[p.Namespace],
				HasLimitRange: lrNS[p.Namespace],
			}
			byNS[p.Namespace] = t
			nodesByNS[p.Namespace] = map[string]bool{}
		}

		t.Pods++
		nodesByNS[p.Namespace][p.NodeName] = true
		t.Requests.Add(p.Requests)
		t.Limits.Add(p.Limits)
		t.Restarts += p.Restarts()

		switch p.QoS {
		case "Guaranteed":
			t.Guaranteed++
		case "Burstable":
			t.Burstable++
		case "BestEffort":
			t.BestEffort++
		}

		if ot := p.LastOOMKill(); ot != nil && ot.After(cutoff) {
			t.RecentOOMKills++
		}

		for i := range p.Containers {
			c := &p.Containers[i]
			if !c.HasCPURequest || !c.HasMemRequest {
				t.MissingRequests++
			}
			if !c.HasMemLimit {
				t.MissingLimits++
			}
			if c.Throttle != nil && c.Throttle.Percent >= th.ThrottleWarnPct {
				t.ThrottledContainers++
			}
		}

		if p.Usage != nil {
			if t.Usage == nil {
				t.Usage = &model.Resources{}
			}
			t.Usage.Add(*p.Usage)

			// Stranded capacity is request minus usage: real hardware the
			// scheduler will not offer to anyone else.
			if d := p.Requests.CPUMilli - p.Usage.CPUMilli; d > 0 &&
				p.Usage.CPUMilli > 0 &&
				float64(p.Requests.CPUMilli)/float64(p.Usage.CPUMilli) >= th.StrandRatio {
				t.StrandedCPUMilli += d
			}
			if d := p.Requests.MemBytes - p.Usage.MemBytes; d > 0 &&
				p.Usage.MemBytes > 0 &&
				float64(p.Requests.MemBytes)/float64(p.Usage.MemBytes) >= th.StrandRatio {
				t.StrandedMemBytes += d
			}
			if d := p.Usage.CPUMilli - p.Requests.CPUMilli; d > 0 {
				t.OverCPUMilli += d
			}
			if d := p.Usage.MemBytes - p.Requests.MemBytes; d > 0 {
				t.OverMemBytes += d
			}
		}
	}

	var out []TenantAnalysis
	for ns, t := range byNS {
		for node := range nodesByNS[ns] {
			t.Nodes = append(t.Nodes, node)
		}
		sort.Strings(t.Nodes)
		out = append(out, *t)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].StrandedMemBytes != out[j].StrandedMemBytes {
			return out[i].StrandedMemBytes > out[j].StrandedMemBytes
		}
		return out[i].Requests.MemBytes > out[j].Requests.MemBytes
	})
	return out
}

func units(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// otherTenants counts the non-system namespaces on n besides ns.
func otherTenants(n *model.Node, ns string) int {
	count := 0
	for _, other := range n.TenantNamespaces() {
		if other != ns {
			count++
		}
	}
	return count
}
