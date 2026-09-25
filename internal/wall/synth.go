package wall

import (
	"time"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Synthetic generates a scripted run of the lab, as model.Cluster frames.
//
// It exists so the renderer can be built and reviewed with no cluster running,
// and so the demo has a fallback that exercises the real pipeline: these frames
// go through analyze.Run and wall.Build exactly as collected ones do, so a
// synthetic frame and a recorded frame are the same shape and the same code
// produced their roles and findings.
//
// The timeline follows the lab's actual physics, not the demo script's wishful
// version of them:
//
//   - billing is throttled from the first frame to the last. Its 50m CPU limit
//     against a busy loop does that on its own, with or without a neighbor.
//   - search OOMKills on its own cycle. It declares a 128Mi limit and warms
//     a 160Mi cache, so it dies on every warm-up. No neighbor is required.
//   - payments arriving does not kill anything. It makes the node's real usage
//     diverge from its requested usage, which is the condition every other
//     tenant was scheduled under a false premise about.
//
// That last distinction is the point. The wall shows co-tenancy and exposure.
// It does not show causation, because the cluster does not contain any.
type Synthetic struct {
	// Start is the wall-clock time of frame zero.
	Start time.Time
}

const (
	synthNode    = "ip-10-0-42-118.ec2.internal"
	synthSysNode = "ip-10-0-11-204.ec2.internal"
	nodeAllocCPU = 3920           // m5.xlarge, 4 vCPU less kube reserved
	nodeAllocMem = 15_258_000_000 // ~14.2Gi allocatable
	mib          = 1024 * 1024
	searchCycleS = 43.0 // observed: ~25s sleep, then it climbs to its limit
	firstOOMAtS  = 42.0
)

// Frame renders the cluster as it looks at elapsed time t.
//
// sinceButton is seconds since the aggressor was scaled up; negative means it
// has not been, and team-payments is absent from the frame. It is a separate
// clock from t on purpose: the button is pressed by a human on stage, so the
// aggressor's ramp cannot be a function of where the loop happened to be.
func (s Synthetic) Frame(t time.Duration, sinceButton float64) *model.Cluster {
	now := s.Start.Add(t)
	secs := t.Seconds()
	pressed := sinceButton >= 0

	tenant := &model.Node{
		Name:         synthNode,
		Allocatable:  model.Resources{CPUMilli: nodeAllocCPU, MemBytes: nodeAllocMem},
		Capacity:     model.Resources{CPUMilli: 4000, MemBytes: 16_384_000_000},
		Ready:        true,
		InstanceType: "m5.xlarge",
		Zone:         "us-east-1a",
	}
	system := &model.Node{
		Name:         synthSysNode,
		Allocatable:  model.Resources{CPUMilli: 1930, MemBytes: 7_300_000_000},
		Capacity:     model.Resources{CPUMilli: 2000, MemBytes: 8_192_000_000},
		Ready:        true,
		InstanceType: "m5.large",
		Zone:         "us-east-1a",
	}

	var pods []*model.Pod
	add := func(n *model.Node, p *model.Pod) {
		p.NodeName = n.Name
		n.Pods = append(n.Pods, p)
		pods = append(pods, p)
	}

	// team-analytics: requests 500m/1Gi twice over, uses almost nothing. The
	// stranded case, and the reason the node looks full on paper.
	for _, name := range []string{"batch-7d9f4c8b6d-2xq4n", "batch-7d9f4c8b6d-mk8vz"} {
		add(tenant, synthPod("team-analytics", name, "Burstable",
			res(500, 1024*mib), res(1000, 2048*mib),
			usage(1, 712*1024), nil, 0, nil))
	}

	// team-billing: 50m limit against a busy loop. Throttled in essentially
	// every CFS period, from the first frame. Running, zero restarts, nothing
	// in its events. This is the one only tier 2 can see.
	throttle := &model.ThrottleStats{Periods: 210, ThrottledPeriods: 208, Percent: 99.0}
	add(tenant, synthPod("team-billing", "worker-6bcd4ff9c9-bfh6x", "Burstable",
		res(50, 32*mib), res(50, 64*mib),
		usage(50, 340*1024), throttle, 0, nil))

	// team-scratch: no requests, no limits. First to be evicted under node
	// pressure, and invisible to any chargeback model built on requests.
	add(tenant, synthPod("team-scratch", "notebook-5c7d8f9b4c-w2lmp", "BestEffort",
		res(0, 0), res(0, 0),
		usage(1, 352*1024), nil, 0, nil))

	// team-search: honest request, honest limit, outgrows both on its own
	// cycle. Restarts accumulate; the most recent kill is what lastState holds.
	restarts, lastOOM := searchState(secs, now)
	searchMem := searchMemAt(secs)
	add(tenant, synthPod("team-search", "api-66c94864f4-t9s6d", "Guaranteed",
		res(100, 128*mib), res(200, 128*mib),
		usage(12, searchMem), nil, restarts, lastOOM))

	// team-payments: the aggressor. Absent until the button. Requests 64Mi and
	// declares no limit at all, then takes a full core and ~700Mi.
	if pressed {
		ramp := sinceButton / 12.0
		if ramp > 1 {
			ramp = 1
		}
		pMem := int64(float64(701*mib) * ramp)
		pCPU := int64(999 * ramp)
		p := synthPod("team-payments", "ingest-89f984698-4z2xb", "Burstable",
			res(50, 64*mib), res(0, 0),
			usage(pCPU, pMem), nil, 0, nil)
		// No limits block on purpose: nothing caps this container.
		p.Containers[0].HasCPULimit = false
		p.Containers[0].HasMemLimit = false
		add(tenant, p)
	}

	// A plausible system footprint, so the tenant node is not the only box on
	// the wall and co-tenancy counts are not flattered by an empty cluster.
	for _, sp := range []struct {
		name string
		cpu  int64
		mem  int64
	}{
		{"aws-node-4kd8v", 25, 48 * mib},
		{"kube-proxy-9wx2m", 10, 22 * mib},
		{"metrics-server-6b7f89c4d-lq8rn", 8, 34 * mib},
	} {
		add(tenant, synthPod("kube-system", sp.name, "Burstable",
			res(sp.cpu, sp.mem), res(0, 0),
			usage(sp.cpu/2, sp.mem/2), nil, 0, nil))
	}
	for _, sp := range []struct {
		name string
		cpu  int64
		mem  int64
	}{
		{"coredns-7d8f9b4c6d-x9v2k", 100, 70 * mib},
		{"karpenter-5f9c8d7b4-nm3qp", 200, 256 * mib},
		{"aws-node-7mq4x", 25, 48 * mib},
	} {
		add(system, synthPod("kube-system", sp.name, "Burstable",
			res(sp.cpu, sp.mem), res(0, 0),
			usage(sp.cpu/3, sp.mem/2), nil, 0, nil))
	}

	for _, n := range []*model.Node{tenant, system} {
		var u model.Resources
		for _, p := range n.Pods {
			if p.Usage != nil {
				u.Add(*p.Usage)
			}
		}
		// Node usage always exceeds the sum of its pods: the kubelet, the
		// container runtime and the kernel are on there too.
		u.CPUMilli += 90
		u.MemBytes += 900 * mib
		n.Usage = &u
	}

	return &model.Cluster{
		Nodes: []*model.Node{tenant, system},
		Pods:  pods,
		Quotas: []model.Quota{{
			Namespace: "team-analytics", Name: "analytics-quota",
			// A quota too loose to ever bind: governed on paper, constraining
			// nothing in practice.
			Hard: map[string]int64{"requests.cpu": 40000, "requests.memory": 80 * 1024 * mib},
			Used: map[string]int64{"requests.cpu": 1000, "requests.memory": 2048 * mib},
		}},
		Namespaces: []string{
			"team-analytics", "team-billing", "team-payments",
			"team-scratch", "team-search", "kube-system",
		},
		Caps: model.Capabilities{
			Core:    model.Capability{Name: "kube-apiserver", Tier: model.Tier0, TierName: model.Tier0.String(), Available: true},
			Metrics: model.Capability{Name: "metrics-server (metrics.k8s.io)", Tier: model.Tier1, TierName: model.Tier1.String(), Available: true},
			Kubelet: model.Capability{Name: "kubelet via nodes/proxy", Tier: model.Tier2, TierName: model.Tier2.String(), Available: true},
		},
		Usage: model.UsageSource{
			Kind: "kubelet", Windowed: true, Window: 21 * time.Second,
			Description: "kubelet counters, rates over a 21s window",
		},
		CollectedAt: now,
	}
}

// searchState returns the restart count and most recent OOMKill for team-search
// at elapsed time t. The first kill lands at firstOOMAtS and they repeat on the
// container's own cycle, which is what a CrashLoopBackOff looks like before the
// backoff grows long enough to go quiet.
func searchState(t float64, now time.Time) (int32, *time.Time) {
	if t < firstOOMAtS {
		return 0, nil
	}
	n := int32((t-firstOOMAtS)/searchCycleS) + 1
	lastAt := firstOOMAtS + float64(n-1)*searchCycleS
	when := now.Add(-time.Duration((t - lastAt) * float64(time.Second)))
	return n, &when
}

// searchMemAt climbs toward the 128Mi limit and resets on each kill.
func searchMemAt(t float64) int64 {
	phase := t
	if t >= firstOOMAtS {
		phase = (t - firstOOMAtS)
		for phase >= searchCycleS {
			phase -= searchCycleS
		}
	}
	// Flat while it "serves traffic", then a steep climb into the limit.
	const flatFor = 22.0
	if phase < flatFor {
		return 18 * mib
	}
	frac := (phase - flatFor) / (searchCycleS - flatFor)
	if frac > 1 {
		frac = 1
	}
	return int64(18*mib + frac*float64(109*mib))
}

func res(cpu, mem int64) model.Resources {
	return model.Resources{CPUMilli: cpu, MemBytes: mem}
}

func usage(cpu, mem int64) *model.Resources {
	return &model.Resources{CPUMilli: cpu, MemBytes: mem}
}

func synthPod(ns, name, qos string, req, lim model.Resources, use *model.Resources,
	throttle *model.ThrottleStats, restarts int32, lastOOM *time.Time) *model.Pod {

	c := model.ContainerState{
		Name:          shortWorkload(name),
		Requests:      req,
		Limits:        lim,
		Usage:         use,
		HasCPURequest: req.CPUMilli > 0,
		HasMemRequest: req.MemBytes > 0,
		HasCPULimit:   lim.CPUMilli > 0,
		HasMemLimit:   lim.MemBytes > 0,
		Throttle:      throttle,
		RestartCount:  restarts,
		LastOOMKill:   lastOOM,
	}
	return &model.Pod{
		Namespace:  ns,
		Name:       name,
		QoS:        qos,
		Phase:      "Running",
		Containers: []model.ContainerState{c},
		Requests:   req,
		Limits:     lim,
		Usage:      use,
		CreatedAt:  time.Now().Add(-30 * time.Minute),
	}
}

// shortWorkload trims a pod name back to its workload, so the container name
// looks like a container name rather than a replica set hash.
func shortWorkload(pod string) string {
	for i, r := range pod {
		if r == '-' {
			rest := pod[i+1:]
			if len(rest) >= 8 {
				return pod[:i]
			}
		}
	}
	return pod
}
