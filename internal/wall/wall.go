// Package wall builds the wire format behind the demo wall.
//
// The wall is a presentation surface, not a second analyzer. Every number here
// comes from model.Cluster or analyze.Report; nothing is computed differently
// and nothing is estimated. What this package adds is shape: the internal model
// marks Cluster.Pods and Node.Pods as json:"-", because the CLI reports roll
// pods up into nodes and tenants before printing. The wall needs the individual
// pod, because a pod is a tile on screen, so it needs its own wire type rather
// than the model's.
//
// It also adds the one thing a snapshot cannot carry: transitions. A tile
// flashes when a pod is OOMKilled, and "was OOMKilled" is a difference between
// two snapshots, not a field in either. Diff does that comparison.
package wall

import (
	"strings"
	"time"

	"github.com/perfectscale-io/noisy-neighbors/internal/analyze"
	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Role is the visual state of a tile. The renderer picks a treatment per role,
// so these strings are part of the contract with the front end.
type Role string

const (
	// RoleAggressor consumes materially more than it requested. Its fill
	// spills past the tile border.
	RoleAggressor Role = "aggressor"
	// RoleStranded requested far more than it uses. A large, mostly empty tile.
	RoleStranded Role = "stranded"
	// RoleThrottled is being held under its CPU limit by CFS.
	RoleThrottled Role = "throttled"
	// RoleOOM was OOMKilled inside the analyzer's window.
	RoleOOM Role = "oom"
	// RoleBestEffort declared neither requests nor limits.
	RoleBestEffort Role = "besteffort"
	// RoleOK is everything else, including system pods.
	RoleOK Role = "ok"
)

// EventKind classifies an entry in the event rail.
type EventKind string

const (
	EventOOMKill  EventKind = "oomkill"
	EventRestart  EventKind = "restart"
	EventThrottle EventKind = "throttle"
	EventPodAdded EventKind = "added"
	EventPodGone  EventKind = "gone"
)

// Event is one line in the rail down the side of the wall.
//
// Every event is a transition detected between two snapshots, which is why it
// carries its own timestamp rather than borrowing the snapshot's.
type Event struct {
	At        time.Time `json:"at"`
	Kind      EventKind `json:"kind"`
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	Node      string    `json:"node"`
	Message   string    `json:"message"`
}

// Pod is one tile.
//
// The geometry the renderer draws from: the tile is sized by Req, and the fill
// inside it is Use. Use is a pointer because with no usage tier there is no
// fill to draw, and drawing an empty tile would misreport a missing measurement
// as zero consumption.
type Pod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node"`
	QoS       string `json:"qos"`
	Phase     string `json:"phase"`
	System    bool   `json:"system"`

	ReqCPU int64 `json:"reqCpu"`
	ReqMem int64 `json:"reqMem"`
	LimCPU int64 `json:"limCpu"`
	LimMem int64 `json:"limMem"`

	UseCPU *int64 `json:"useCpu,omitempty"`
	UseMem *int64 `json:"useMem,omitempty"`

	HasCPURequest bool `json:"hasCpuRequest"`
	HasMemRequest bool `json:"hasMemRequest"`
	HasCPULimit   bool `json:"hasCpuLimit"`
	HasMemLimit   bool `json:"hasMemLimit"`

	ThrottlePct *float64 `json:"throttlePct,omitempty"`
	Restarts    int32    `json:"restarts"`
	// OOMAgoSec is seconds since the last OOMKill. Nil when there has not been
	// one, or when it fell outside the analyzer's window.
	OOMAgoSec *float64 `json:"oomAgoSec,omitempty"`

	Role Role `json:"role"`
	// Roles carries every state that applied, because a pod can be both
	// throttled and recently OOMKilled and the rail should say so even though
	// the tile can only take one treatment.
	Roles []Role `json:"roles,omitempty"`
}

// Key identifies a pod across snapshots.
func (p *Pod) Key() string { return p.Namespace + "/" + p.Name }

// Node is one box on the wall, sized to Allocatable.
type Node struct {
	Name         string `json:"name"`
	InstanceType string `json:"instanceType,omitempty"`
	Zone         string `json:"zone,omitempty"`

	AllocCPU int64 `json:"allocCpu"`
	AllocMem int64 `json:"allocMem"`

	// Requested and Limit totals, so the renderer can draw the line where the
	// scheduler thinks the node is full without re-summing the tiles.
	ReqCPU int64 `json:"reqCpu"`
	ReqMem int64 `json:"reqMem"`
	LimCPU int64 `json:"limCpu"`
	LimMem int64 `json:"limMem"`

	UseCPU *int64 `json:"useCpu,omitempty"`
	UseMem *int64 `json:"useMem,omitempty"`

	Severity    string   `json:"severity"`
	TenantCount int      `json:"tenantCount"`
	Tenants     []string `json:"tenants"`

	MemoryPressure bool `json:"memoryPressure"`
	Ready          bool `json:"ready"`

	Pods []*Pod `json:"pods"`
}

// Tenant is one row in the side table.
type Tenant struct {
	Namespace   string `json:"namespace"`
	Pods        int    `json:"pods"`
	System      bool   `json:"system"`
	ReqCPU      int64  `json:"reqCpu"`
	ReqMem      int64  `json:"reqMem"`
	UseCPU      *int64 `json:"useCpu,omitempty"`
	UseMem      *int64 `json:"useMem,omitempty"`
	StrandedMem int64  `json:"strandedMem"`
	OOMKills    int    `json:"oomKills"`
	Throttled   int    `json:"throttled"`
	Restarts    int32  `json:"restarts"`
}

// Capability mirrors model.Capability for the provenance strip.
type Capability struct {
	Name      string `json:"name"`
	Tier      string `json:"tier"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Finding is a flattened analyze.Finding for the blast-radius panel.
type Finding struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Tenant   string `json:"tenant"`
	Workload string `json:"workload,omitempty"`
	Node     string `json:"node,omitempty"`
	Message  string `json:"message"`
}

// Snapshot is one frame of the wall.
type Snapshot struct {
	At time.Time `json:"at"`
	// Seq increments per frame so the front end can tell a genuinely new frame
	// from a reconnect replaying the last one.
	Seq int64 `json:"seq"`

	Nodes   []*Node  `json:"nodes"`
	Tenants []Tenant `json:"tenants"`

	Aggressors []Finding `json:"aggressors"`
	Exposed    []Finding `json:"exposed"`

	// Events are the transitions detected against the previous frame.
	Events []Event `json:"events"`

	Caps        []Capability `json:"caps"`
	UsageSource string       `json:"usageSource"`
	HasUsage    bool         `json:"hasUsage"`
	HasThrottle bool         `json:"hasThrottle"`

	// NotMeasured is the visible half of graceful degradation, carried through
	// to the wall so the demo can show a tier being switched off.
	NotMeasured []string `json:"notMeasured,omitempty"`

	// Source is "live", "replay" or "synthetic". The renderer shows it, because
	// a demo that silently falls back to a recording is a demo that lies.
	Source string `json:"source"`
}

// Build converts a collected cluster and its analysis into one wall frame.
func Build(cluster *model.Cluster, rep *analyze.Report, th analyze.Thresholds, source string) *Snapshot {
	snap := &Snapshot{
		At:          cluster.CollectedAt,
		Source:      source,
		HasUsage:    cluster.Caps.UsageAvailable(),
		HasThrottle: cluster.Caps.Kubelet.Available,
		UsageSource: cluster.Usage.Description,
	}

	for _, c := range cluster.Caps.All() {
		snap.Caps = append(snap.Caps, Capability{
			Name:      c.Name,
			Tier:      c.TierName,
			Available: c.Available,
			Reason:    c.Reason,
		})
	}
	for _, s := range rep.Skipped {
		snap.NotMeasured = append(snap.NotMeasured, s.Analysis+": "+s.Reason)
	}

	// Index the analysis by node so each box carries its own severity and
	// tenant list rather than the renderer recomputing them.
	byNode := make(map[string]*analyze.NodeAnalysis, len(rep.Nodes))
	for i := range rep.Nodes {
		byNode[rep.Nodes[i].Name] = &rep.Nodes[i]
	}

	// Roles come from the analyzer's own findings wherever a finding exists, so
	// a tile and the blast-radius panel beside it can never disagree. An
	// earlier version of this re-derived the aggressor rule here and got it
	// subtly wrong: it gated on the size of the request where the analyzer
	// gates on the size of the usage, so the lab's headline aggressor -- 64Mi
	// requested, 700Mi used -- rendered as an ordinary tile while the panel
	// next to it called it an aggressor.
	roles := rolesFromFindings(rep)

	for _, n := range cluster.Nodes {
		wn := &Node{
			Name:           n.Name,
			InstanceType:   n.InstanceType,
			Zone:           n.Zone,
			AllocCPU:       n.Allocatable.CPUMilli,
			AllocMem:       n.Allocatable.MemBytes,
			MemoryPressure: n.MemoryPressure,
			Ready:          n.Ready,
			Severity:       "OK",
		}
		if n.Usage != nil {
			cpu, mem := n.Usage.CPUMilli, n.Usage.MemBytes
			wn.UseCPU, wn.UseMem = &cpu, &mem
		}
		if na := byNode[n.Name]; na != nil {
			wn.Severity = na.SeverityStr
			wn.TenantCount = na.TenantCount
		}
		wn.Tenants = n.TenantNamespaces()

		for _, p := range n.Pods {
			wp := buildPod(p, th, roles[p.Key()])
			wn.ReqCPU += wp.ReqCPU
			wn.ReqMem += wp.ReqMem
			wn.LimCPU += wp.LimCPU
			wn.LimMem += wp.LimMem
			wn.Pods = append(wn.Pods, wp)
		}
		snap.Nodes = append(snap.Nodes, wn)
	}

	for _, t := range rep.Tenants {
		wt := Tenant{
			Namespace:   t.Namespace,
			Pods:        t.Pods,
			System:      model.IsSystemNamespace(t.Namespace),
			ReqCPU:      t.Requests.CPUMilli,
			ReqMem:      t.Requests.MemBytes,
			StrandedMem: t.StrandedMemBytes,
			OOMKills:    t.RecentOOMKills,
			Throttled:   t.ThrottledContainers,
			Restarts:    t.Restarts,
		}
		if t.Usage != nil {
			cpu, mem := t.Usage.CPUMilli, t.Usage.MemBytes
			wt.UseCPU, wt.UseMem = &cpu, &mem
		}
		snap.Tenants = append(snap.Tenants, wt)
	}

	for i := range rep.Nodes {
		for _, f := range rep.Nodes[i].Aggressors {
			snap.Aggressors = append(snap.Aggressors, flatten(f))
		}
		for _, f := range rep.Nodes[i].Exposed {
			snap.Exposed = append(snap.Exposed, flatten(f))
		}
	}

	return snap
}

func flatten(f analyze.Finding) Finding {
	return Finding{
		Severity: f.Severity.String(),
		Kind:     f.Kind,
		Tenant:   f.Tenant,
		Workload: f.Workload,
		Node:     f.Node,
		Message:  f.Message,
	}
}

// rolesFromFindings indexes the analyzer's findings by workload.
//
// Throttling findings name a container ("ns/pod:container") because that is the
// level CFS acts at, so the container suffix is trimmed back to the pod the
// tile represents.
func rolesFromFindings(rep *analyze.Report) map[string][]Role {
	out := map[string][]Role{}
	add := func(workload string, r Role) {
		if workload == "" {
			return
		}
		if i := strings.IndexByte(workload, ':'); i >= 0 {
			workload = workload[:i]
		}
		for _, have := range out[workload] {
			if have == r {
				return
			}
		}
		out[workload] = append(out[workload], r)
	}

	for i := range rep.Nodes {
		for _, f := range rep.Nodes[i].Aggressors {
			add(f.Workload, RoleAggressor)
		}
		for _, f := range rep.Nodes[i].Exposed {
			switch {
			case strings.Contains(f.Kind, "oomkill"):
				add(f.Workload, RoleOOM)
			case strings.Contains(f.Kind, "throttling"):
				add(f.Workload, RoleThrottled)
			case strings.Contains(f.Kind, "besteffort"):
				add(f.Workload, RoleBestEffort)
			}
		}
	}
	return out
}

func buildPod(p *model.Pod, th analyze.Thresholds, found []Role) *Pod {
	wp := &Pod{
		Namespace: p.Namespace,
		Name:      p.Name,
		Node:      p.NodeName,
		QoS:       p.QoS,
		Phase:     p.Phase,
		System:    model.IsSystemNamespace(p.Namespace),
		ReqCPU:    p.Requests.CPUMilli,
		ReqMem:    p.Requests.MemBytes,
		LimCPU:    p.Limits.CPUMilli,
		LimMem:    p.Limits.MemBytes,
		Restarts:  p.Restarts(),
	}
	if p.Usage != nil {
		cpu, mem := p.Usage.CPUMilli, p.Usage.MemBytes
		wp.UseCPU, wp.UseMem = &cpu, &mem
	}
	for i := range p.Containers {
		c := &p.Containers[i]
		wp.HasCPURequest = wp.HasCPURequest || c.HasCPURequest
		wp.HasMemRequest = wp.HasMemRequest || c.HasMemRequest
		wp.HasCPULimit = wp.HasCPULimit || c.HasCPULimit
		wp.HasMemLimit = wp.HasMemLimit || c.HasMemLimit
	}
	if _, t := p.WorstThrottle(); t != nil {
		pct := t.Percent
		wp.ThrottlePct = &pct
	}
	if oom := p.LastOOMKill(); oom != nil {
		ago := time.Since(*oom).Seconds()
		if ago >= 0 && time.Since(*oom) <= th.OOMWindow {
			wp.OOMAgoSec = &ago
		}
	}
	assignRoles(wp, th, found)
	return wp
}

// assignRoles picks the tile treatment.
//
// Everything the analyzer reports as a finding is taken from the analyzer.
// Stranding is the exception: it is a tenant-level number in the report with no
// per-pod finding behind it, so it is computed here, using the report's own
// StrandRatio and its own guard that usage must be non-zero.
//
// Order matters. A pod can qualify for several roles at once and a tile can
// only take one treatment, so the most operationally urgent wins: being killed
// beats being throttled, and being throttled beats holding idle capacity.
func assignRoles(p *Pod, th analyze.Thresholds, found []Role) {
	p.Role = RoleOK
	p.Roles = append(p.Roles, found...)

	has := func(r Role) bool {
		for _, x := range p.Roles {
			if x == r {
				return true
			}
		}
		return false
	}

	if p.UseCPU != nil && p.UseMem != nil && !has(RoleAggressor) {
		strandMem := p.HasMemRequest && *p.UseMem > 0 &&
			float64(p.ReqMem)/float64(*p.UseMem) >= th.StrandRatio
		strandCPU := p.HasCPURequest && *p.UseCPU > 0 &&
			float64(p.ReqCPU)/float64(*p.UseCPU) >= th.StrandRatio
		// Zero usage is deliberately not stranding, matching the report.
		// metrics-server rounds an idle container's CPU down to 0m, so treating
		// zero as "the extreme of stranding" flagged every quiet pod with a
		// token request, including ones just right-sized to the minimum.
		if strandMem || strandCPU {
			p.Roles = append(p.Roles, RoleStranded)
		}
	}

	// QoS is on the pod, not in a finding, unless the node was saturated enough
	// for the analyzer to call it exposed.
	if p.QoS == "BestEffort" && !has(RoleBestEffort) {
		p.Roles = append(p.Roles, RoleBestEffort)
	}

	for _, r := range []Role{RoleOOM, RoleAggressor, RoleThrottled, RoleStranded, RoleBestEffort} {
		if has(r) {
			p.Role = r
			return
		}
	}
}

// Diff finds the transitions between two frames and fills cur.Events.
//
// Without this the wall is a gauge: it would show that a pod is currently in a
// bad state but never that it just entered one, and the moment of the kill is
// the moment the demo exists to show.
func Diff(prev, cur *Snapshot) {
	if cur == nil {
		return
	}
	if prev == nil {
		return
	}

	prevPods := map[string]*Pod{}
	for _, n := range prev.Nodes {
		for _, p := range n.Pods {
			prevPods[p.Key()] = p
		}
	}
	curPods := map[string]*Pod{}
	for _, n := range cur.Nodes {
		for _, p := range n.Pods {
			curPods[p.Key()] = p
		}
	}

	at := cur.At
	for key, p := range curPods {
		old, existed := prevPods[key]
		if !existed {
			if !p.System {
				cur.Events = append(cur.Events, Event{
					At: at, Kind: EventPodAdded, Namespace: p.Namespace,
					Pod: p.Name, Node: p.Node,
					Message: "scheduled onto " + p.Node,
				})
			}
			continue
		}

		// A restart count that went up is the only reliable signal that a pod
		// died and came back between two frames. lastState carries the reason.
		if p.Restarts > old.Restarts {
			kind, msg := EventRestart, "restarted"
			if p.OOMAgoSec != nil {
				kind, msg = EventOOMKill, "OOMKilled and restarted"
			}
			cur.Events = append(cur.Events, Event{
				At: at, Kind: kind, Namespace: p.Namespace,
				Pod: p.Name, Node: p.Node, Message: msg,
			})
			continue
		}
		// A fresh OOMKill without a restart bump: the kill is visible in
		// lastState before the counter catches up on some kubelet versions.
		if p.OOMAgoSec != nil && old.OOMAgoSec != nil && *p.OOMAgoSec < *old.OOMAgoSec {
			cur.Events = append(cur.Events, Event{
				At: at, Kind: EventOOMKill, Namespace: p.Namespace,
				Pod: p.Name, Node: p.Node, Message: "OOMKilled",
			})
			continue
		}
		if p.OOMAgoSec != nil && old.OOMAgoSec == nil {
			cur.Events = append(cur.Events, Event{
				At: at, Kind: EventOOMKill, Namespace: p.Namespace,
				Pod: p.Name, Node: p.Node, Message: "OOMKilled",
			})
			continue
		}

		// Crossing into throttling is worth a line, because this is the case
		// that never generates an event of its own anywhere in Kubernetes.
		wasThrottled := old.ThrottlePct != nil && *old.ThrottlePct >= 5
		isThrottled := p.ThrottlePct != nil && *p.ThrottlePct >= 5
		if isThrottled && !wasThrottled {
			cur.Events = append(cur.Events, Event{
				At: at, Kind: EventThrottle, Namespace: p.Namespace,
				Pod: p.Name, Node: p.Node,
				Message: "entered CPU throttling",
			})
		}
	}

	for key, old := range prevPods {
		if _, still := curPods[key]; !still && !old.System {
			cur.Events = append(cur.Events, Event{
				At: at, Kind: EventPodGone, Namespace: old.Namespace,
				Pod: old.Name, Node: old.Node, Message: "gone",
			})
		}
	}
}
