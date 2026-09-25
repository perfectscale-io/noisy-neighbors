// Package model holds the domain types shared by the collectors, the analyzer
// and the reporters.
//
// Everything here is deliberately source-agnostic: a Pod looks the same whether
// its usage numbers came from metrics-server or from a direct kubelet scrape,
// and pointer fields are nil when a tier was unavailable. That is what lets the
// analyzer degrade cleanly instead of guessing.
package model

import (
	"fmt"
	"time"
)

// Tier identifies the data source a piece of information requires.
type Tier int

const (
	// Tier0 needs nothing but the kube-apiserver. Available on every cluster.
	Tier0 Tier = iota
	// Tier1 needs metrics.k8s.io, i.e. metrics-server.
	Tier1
	// Tier2 needs the nodes/proxy subresource to scrape kubelets directly.
	Tier2
)

func (t Tier) String() string {
	switch t {
	case Tier0:
		return "tier 0 (apiserver)"
	case Tier1:
		return "tier 1 (metrics-server)"
	case Tier2:
		return "tier 2 (kubelet)"
	}
	return "unknown"
}

// Capability records whether one data source is usable, and if not, why not and
// what the operator can do about it.
type Capability struct {
	Name      string `json:"name"`
	Tier      Tier   `json:"-"`
	TierName  string `json:"tier"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

// Capabilities is the result of the pre-flight probe.
type Capabilities struct {
	Core    Capability `json:"core"`
	Metrics Capability `json:"metrics"`
	Kubelet Capability `json:"kubelet"`
}

// UsageAvailable reports whether any source of actual-usage data was found.
func (c Capabilities) UsageAvailable() bool {
	return c.Metrics.Available || c.Kubelet.Available
}

// All returns the capabilities in tier order, for reporting.
func (c Capabilities) All() []Capability {
	return []Capability{c.Core, c.Metrics, c.Kubelet}
}

// Resources is a CPU/memory pair. CPU is millicores, memory is bytes.
type Resources struct {
	CPUMilli int64 `json:"cpuMilli"`
	MemBytes int64 `json:"memBytes"`
}

// Add accumulates other into r.
func (r *Resources) Add(other Resources) {
	r.CPUMilli += other.CPUMilli
	r.MemBytes += other.MemBytes
}

// IsZero reports whether both dimensions are unset.
func (r Resources) IsZero() bool { return r.CPUMilli == 0 && r.MemBytes == 0 }

// ThrottleStats is a windowed CFS throttling measurement.
//
// Periods and ThrottledPeriods are deltas across the sample window, not the
// cumulative counters since container start. Reporting the cumulative ratio is
// a common mistake: a container throttled badly at startup and healthy since
// will look permanently broken.
type ThrottleStats struct {
	Periods          int64   `json:"periods"`
	ThrottledPeriods int64   `json:"throttledPeriods"`
	Percent          float64 `json:"percent"`
}

// ContainerState is the per-container view, merged across every tier.
type ContainerState struct {
	Name     string     `json:"name"`
	Requests Resources  `json:"requests"`
	Limits   Resources  `json:"limits"`
	Usage    *Resources `json:"usage,omitempty"`

	HasCPURequest bool `json:"hasCpuRequest"`
	HasMemRequest bool `json:"hasMemRequest"`
	HasCPULimit   bool `json:"hasCpuLimit"`
	HasMemLimit   bool `json:"hasMemLimit"`

	// Tier 0 forensics. LastOOMKill is one generation deep only: the kubelet
	// keeps just the most recent termination in lastState.
	LastOOMKill  *time.Time `json:"lastOomKill,omitempty"`
	RestartCount int32      `json:"restartCount"`

	// Tier 2.
	Throttle  *ThrottleStats `json:"throttle,omitempty"`
	OOMEvents *int64         `json:"oomEvents,omitempty"`
}

// Pod is a scheduled pod with whatever measurements we could gather.
type Pod struct {
	Namespace  string           `json:"namespace"`
	Name       string           `json:"name"`
	NodeName   string           `json:"node"`
	QoS        string           `json:"qos"`
	Phase      string           `json:"phase"`
	Containers []ContainerState `json:"containers"`

	Requests Resources  `json:"requests"`
	Limits   Resources  `json:"limits"`
	Usage    *Resources `json:"usage,omitempty"`

	// Tier 2 network counters, deltas across the sample window.
	NetRxBytes *int64 `json:"netRxBytes,omitempty"`
	NetTxBytes *int64 `json:"netTxBytes,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

// Key is the namespace/name identifier used throughout the reports.
func (p *Pod) Key() string { return p.Namespace + "/" + p.Name }

// LastOOMKill returns the most recent OOMKill across the pod's containers.
func (p *Pod) LastOOMKill() *time.Time {
	var latest *time.Time
	for i := range p.Containers {
		t := p.Containers[i].LastOOMKill
		if t == nil {
			continue
		}
		if latest == nil || t.After(*latest) {
			latest = t
		}
	}
	return latest
}

// WorstThrottle returns the most heavily throttled container in the pod.
func (p *Pod) WorstThrottle() (string, *ThrottleStats) {
	var worst *ThrottleStats
	var name string
	for i := range p.Containers {
		t := p.Containers[i].Throttle
		if t == nil || t.Periods == 0 {
			continue
		}
		if worst == nil || t.Percent > worst.Percent {
			worst = t
			name = p.Containers[i].Name
		}
	}
	return name, worst
}

// Restarts totals container restarts in the pod.
func (p *Pod) Restarts() int32 {
	var n int32
	for i := range p.Containers {
		n += p.Containers[i].RestartCount
	}
	return n
}

// Node is a cluster node plus the pods scheduled onto it.
type Node struct {
	Name          string     `json:"name"`
	Allocatable   Resources  `json:"allocatable"`
	Capacity      Resources  `json:"capacity"`
	Usage         *Resources `json:"usage,omitempty"`
	Pods          []*Pod     `json:"-"`
	Unschedulable bool       `json:"unschedulable"`

	MemoryPressure bool `json:"memoryPressure"`
	DiskPressure   bool `json:"diskPressure"`
	PIDPressure    bool `json:"pidPressure"`
	Ready          bool `json:"ready"`

	InstanceType string `json:"instanceType,omitempty"`
	Zone         string `json:"zone,omitempty"`
}

// SystemNamespaces are infrastructure, not tenants. They consume real
// resources and belong in the tenant table, but counting them as neighbors
// would make every node look shared and every co-tenancy number wrong.
var SystemNamespaces = map[string]bool{
	"kube-system":        true,
	"kube-public":        true,
	"kube-node-lease":    true,
	"local-path-storage": true,
	"gmp-system":         true,
	"gke-managed-system": true,
}

// IsSystemNamespace reports whether ns is infrastructure rather than a tenant.
func IsSystemNamespace(ns string) bool { return SystemNamespaces[ns] }

// TenantNamespaces returns the non-system namespaces sharing this node.
func (n *Node) TenantNamespaces() []string {
	var out []string
	for _, ns := range n.Namespaces() {
		if !IsSystemNamespace(ns) {
			out = append(out, ns)
		}
	}
	return out
}

// Namespaces returns every distinct namespace with a pod on this node.
func (n *Node) Namespaces() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range n.Pods {
		if !seen[p.Namespace] {
			seen[p.Namespace] = true
			out = append(out, p.Namespace)
		}
	}
	return out
}

// Quota is a ResourceQuota's hard limits and live usage. Keys are Kubernetes
// resource names ("requests.cpu", "limits.memory", "cpu", "memory", "pods").
// CPU values are millicores, memory values are bytes, everything else is a
// plain count.
type Quota struct {
	Namespace string           `json:"namespace"`
	Name      string           `json:"name"`
	Hard      map[string]int64 `json:"hard"`
	Used      map[string]int64 `json:"used"`
}

// LimitRangeInfo notes the presence of a LimitRange in a namespace.
type LimitRangeInfo struct {
	Namespace         string `json:"namespace"`
	Name              string `json:"name"`
	HasDefaultRequest bool   `json:"hasDefaultRequest"`
	HasDefaultLimit   bool   `json:"hasDefaultLimit"`
}

// Event is a trimmed-down cluster event. The apiserver's --event-ttl defaults
// to one hour, so this is a recent window, never a history.
type Event struct {
	Namespace string    `json:"namespace"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Object    string    `json:"object"`
	Node      string    `json:"node,omitempty"`
	Count     int32     `json:"count"`
	LastSeen  time.Time `json:"lastSeen"`
}

// UsageSource describes where actual-usage numbers came from, so the report can
// state its own provenance instead of implying a precision it does not have.
type UsageSource struct {
	Kind        string        `json:"kind"` // "kubelet", "metrics-server", "none"
	Windowed    bool          `json:"windowed"`
	Window      time.Duration `json:"-"`
	Description string        `json:"description"`
}

// Cluster is the full collected snapshot.
type Cluster struct {
	Nodes       []*Node          `json:"nodes"`
	Pods        []*Pod           `json:"-"`
	Quotas      []Quota          `json:"quotas"`
	LimitRanges []LimitRangeInfo `json:"limitRanges"`
	Events      []Event          `json:"events"`
	Namespaces  []string         `json:"namespaces"`

	Caps        Capabilities `json:"capabilities"`
	Usage       UsageSource  `json:"usageSource"`
	CollectedAt time.Time    `json:"collectedAt"`
}

// Allocatable totals allocatable capacity across all nodes.
func (c *Cluster) Allocatable() Resources {
	var total Resources
	for _, n := range c.Nodes {
		total.Add(n.Allocatable)
	}
	return total
}

// FormatCPU always renders millicores.
//
// kubectl switches to whole cores above 1000m, which reads fine in a table and
// badly in a sentence: "using 1.0" invites the question "1.0 what". Millicores
// everywhere costs a couple of columns and removes the ambiguity.
func FormatCPU(milli int64) string {
	return fmt.Sprintf("%dm", milli)
}

// FormatMem renders bytes in binary units.
func FormatMem(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ci", float64(b)/float64(div), "KMGTP"[exp])
}

// Pct is a percentage helper that treats a zero denominator as zero.
func Pct(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}
