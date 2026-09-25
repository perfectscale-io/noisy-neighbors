package analyze

import (
	"fmt"
	"sort"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// analyzeQuotas looks for quota theater: quotas that exist but do not constrain
// anything, quotas that collectively promise more than the cluster owns, and
// namespaces with no guard rails at all.
//
// None of this needs metrics. It is arithmetic on objects every cluster has,
// which makes it the one section that always renders.
func analyzeQuotas(cluster *model.Cluster, th Thresholds) []Finding {
	var out []Finding

	allocatable := cluster.Allocatable()

	// Sum every namespace's hard cap and compare it with what the cluster
	// actually has. A cluster whose quotas add up to several times its capacity
	// is not rationing anything; it is running first-come-first-served with
	// extra steps.
	var totalHardCPU, totalHardMem int64
	quotaByNS := map[string]bool{}
	for _, q := range cluster.Quotas {
		quotaByNS[q.Namespace] = true
		totalHardCPU += pickQuota(q.Hard, "requests.cpu", "cpu")
		totalHardMem += pickQuota(q.Hard, "requests.memory", "memory")
	}

	if totalHardCPU > 0 && allocatable.CPUMilli > 0 {
		ratio := float64(totalHardCPU) / float64(allocatable.CPUMilli)
		if ratio > 1.0 {
			sev := SevWarn
			if ratio > 2.0 {
				sev = SevCritical
			}
			out = append(out, Finding{
				Severity: sev,
				Kind:     "quota-oversubscribed-cpu",
				Tier:     model.Tier0,
				TierName: model.Tier0.String(),
				Message: fmt.Sprintf("namespace CPU quotas total %s against %s allocatable (%.1fx oversubscribed): quotas cap the blast radius but cannot prevent contention",
					model.FormatCPU(totalHardCPU), model.FormatCPU(allocatable.CPUMilli), ratio),
			})
		}
	}

	if totalHardMem > 0 && allocatable.MemBytes > 0 {
		ratio := float64(totalHardMem) / float64(allocatable.MemBytes)
		if ratio > 1.0 {
			sev := SevWarn
			if ratio > 2.0 {
				sev = SevCritical
			}
			out = append(out, Finding{
				Severity: sev,
				Kind:     "quota-oversubscribed-memory",
				Tier:     model.Tier0,
				TierName: model.Tier0.String(),
				Message: fmt.Sprintf("namespace memory quotas total %s against %s allocatable (%.1fx oversubscribed): if every tenant claimed its quota the cluster could not honour it",
					model.FormatMem(totalHardMem), model.FormatMem(allocatable.MemBytes), ratio),
			})
		}
	}

	// A quota with plenty of headroom is not evidence of good behavior; it may
	// simply be set too high to ever bind.
	for _, q := range cluster.Quotas {
		hardMem := pickQuota(q.Hard, "requests.memory", "memory")
		usedMem := pickQuota(q.Used, "requests.memory", "memory")
		if hardMem > 0 && usedMem > 0 {
			util := model.Pct(usedMem, hardMem)
			if util < 25 {
				out = append(out, Finding{
					Severity: SevInfo,
					Kind:     "quota-not-binding",
					Tenant:   q.Namespace,
					Tier:     model.Tier0,
					TierName: model.Tier0.String(),
					Message: fmt.Sprintf("memory quota %s, using %s (%.0f%%): a quota this loose is the same as no quota",
						model.FormatMem(hardMem), model.FormatMem(usedMem), util),
				})
			} else if util > 90 {
				out = append(out, Finding{
					Severity: SevWarn,
					Kind:     "quota-near-limit",
					Tenant:   q.Namespace,
					Tier:     model.Tier0,
					TierName: model.Tier0.String(),
					Message: fmt.Sprintf("memory quota %.0f%% consumed (%s of %s): new pods here will fail to schedule",
						util, model.FormatMem(usedMem), model.FormatMem(hardMem)),
				})
			}
		}
	}

	// Namespaces sharing nodes with others but with no quota and no LimitRange.
	lrByNS := map[string]bool{}
	for _, lr := range cluster.LimitRanges {
		lrByNS[lr.Namespace] = true
	}

	shared := sharedNamespaces(cluster)
	var ungoverned []string
	for _, ns := range cluster.Namespaces {
		if isSystemNamespace(ns) || !shared[ns] {
			continue
		}
		if !quotaByNS[ns] && !lrByNS[ns] {
			ungoverned = append(ungoverned, ns)
		}
	}
	sort.Strings(ungoverned)
	for _, ns := range ungoverned {
		out = append(out, Finding{
			Severity: SevWarn,
			Kind:     "no-guardrails",
			Tenant:   ns,
			Tier:     model.Tier0,
			TierName: model.Tier0.String(),
			Message:  "shares nodes with other tenants but has neither a ResourceQuota nor a LimitRange",
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	return out
}

// analyzeCluster produces the findings that only make sense cluster-wide.
func analyzeCluster(cluster *model.Cluster, th Thresholds) []Finding {
	var out []Finding

	allocatable := cluster.Allocatable()
	var req, lim model.Resources
	for _, p := range cluster.Pods {
		req.Add(p.Requests)
		lim.Add(p.Limits)
	}

	// The limit oversubscription ratio: what the cluster has promised if every
	// tenant simultaneously used everything it is allowed to. Nobody shows this
	// number, and on a shared cluster it is the honest measure of how much the
	// scheduler is relying on tenants not all misbehaving at once.
	if lim.MemBytes > 0 && allocatable.MemBytes > 0 {
		ratio := float64(lim.MemBytes) / float64(allocatable.MemBytes)
		if ratio > 1.0 {
			sev := SevInfo
			if ratio > 1.5 {
				sev = SevWarn
			}
			if ratio > 3.0 {
				sev = SevCritical
			}
			out = append(out, Finding{
				Severity: sev,
				Kind:     "limit-oversubscription",
				Tier:     model.Tier0,
				TierName: model.Tier0.String(),
				Message: fmt.Sprintf("memory limits total %.1fx allocatable (%s promised, %s exists): the cluster is solvent only because tenants do not all peak together",
					ratio, model.FormatMem(lim.MemBytes), model.FormatMem(allocatable.MemBytes)),
			})
		}
	}

	// Scheduling pressure caused by requests rather than real consumption.
	failedScheduling := 0
	evicted := 0
	preempted := 0
	for _, e := range cluster.Events {
		switch e.Reason {
		case "FailedScheduling":
			failedScheduling++
		case "Evicted":
			evicted++
		case "Preempted":
			preempted++
		}
	}

	if failedScheduling > 0 {
		sev := SevWarn
		if failedScheduling > 10 {
			sev = SevCritical
		}
		out = append(out, Finding{
			Severity: sev,
			Kind:     "failed-scheduling",
			Tier:     model.Tier0,
			TierName: model.Tier0.String(),
			Message: fmt.Sprintf("%d FailedScheduling events in the last hour: capacity other tenants could not schedule into",
				failedScheduling),
		})
	}
	if evicted > 0 {
		out = append(out, Finding{
			Severity: SevCritical,
			Kind:     "evictions",
			Tier:     model.Tier0,
			TierName: model.Tier0.String(),
			Message:  fmt.Sprintf("%d Evicted events in the last hour: node pressure reached the point of reclaiming pods", evicted),
		})
	}
	if preempted > 0 {
		out = append(out, Finding{
			Severity: SevWarn,
			Kind:     "preemptions",
			Tier:     model.Tier0,
			TierName: model.Tier0.String(),
			Message:  fmt.Sprintf("%d Preempted events in the last hour: higher-priority tenants displaced lower-priority ones", preempted),
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Severity > out[j].Severity })
	return out
}

// sharedNamespaces returns namespaces that co-habit a node with another tenant.
func sharedNamespaces(cluster *model.Cluster) map[string]bool {
	out := map[string]bool{}
	for _, n := range cluster.Nodes {
		names := n.Namespaces()
		var tenants []string
		for _, ns := range names {
			if !isSystemNamespace(ns) {
				tenants = append(tenants, ns)
			}
		}
		if len(tenants) > 1 {
			for _, ns := range tenants {
				out[ns] = true
			}
		}
	}
	return out
}

func isSystemNamespace(ns string) bool { return model.IsSystemNamespace(ns) }

func pickQuota(m map[string]int64, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return 0
}
