package collect

import (
	"context"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// collectCore gathers everything reachable from the kube-apiserver alone.
//
// This is the tier that works on every cluster with a plain read-only
// ClusterRole, and it is enough on its own to build the co-tenancy graph and the
// structural overcommit picture.
func collectCore(ctx context.Context, kube kubernetes.Interface, nsFilter []string) (*model.Cluster, error) {
	cluster := &model.Cluster{CollectedAt: time.Now()}

	nodeList, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	byNode := map[string]*model.Node{}
	for i := range nodeList.Items {
		n := convertNode(&nodeList.Items[i])
		cluster.Nodes = append(cluster.Nodes, n)
		byNode[n.Name] = n
	}

	podList, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	keep := namespaceFilter(nsFilter)
	nsSeen := map[string]bool{}
	for i := range podList.Items {
		p := &podList.Items[i]
		// Unscheduled pods have no node and therefore no blast radius. They show
		// up separately through FailedScheduling events.
		if p.Spec.NodeName == "" {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if !keep(p.Namespace) {
			continue
		}
		pod := convertPod(p)
		cluster.Pods = append(cluster.Pods, pod)
		nsSeen[pod.Namespace] = true
		if n, ok := byNode[pod.NodeName]; ok {
			n.Pods = append(n.Pods, pod)
		}
	}

	for ns := range nsSeen {
		cluster.Namespaces = append(cluster.Namespaces, ns)
	}
	sort.Strings(cluster.Namespaces)

	cluster.Quotas = collectQuotas(ctx, kube, keep)
	cluster.LimitRanges = collectLimitRanges(ctx, kube, keep)
	cluster.Events = collectEvents(ctx, kube, keep)

	return cluster, nil
}

func convertNode(n *corev1.Node) (out *model.Node) {
	out = &model.Node{
		Name:          n.Name,
		Unschedulable: n.Spec.Unschedulable,
		InstanceType:  n.Labels["node.kubernetes.io/instance-type"],
		Zone:          n.Labels["topology.kubernetes.io/zone"],
		Allocatable: model.Resources{
			CPUMilli: n.Status.Allocatable.Cpu().MilliValue(),
			MemBytes: n.Status.Allocatable.Memory().Value(),
		},
		Capacity: model.Resources{
			CPUMilli: n.Status.Capacity.Cpu().MilliValue(),
			MemBytes: n.Status.Capacity.Memory().Value(),
		},
	}
	for _, c := range n.Status.Conditions {
		on := c.Status == corev1.ConditionTrue
		switch c.Type {
		case corev1.NodeReady:
			out.Ready = on
		case corev1.NodeMemoryPressure:
			out.MemoryPressure = on
		case corev1.NodeDiskPressure:
			out.DiskPressure = on
		case corev1.NodePIDPressure:
			out.PIDPressure = on
		}
	}
	return out
}

func convertPod(p *corev1.Pod) *model.Pod {
	pod := &model.Pod{
		Namespace: p.Namespace,
		Name:      p.Name,
		NodeName:  p.Spec.NodeName,
		QoS:       string(p.Status.QOSClass),
		Phase:     string(p.Status.Phase),
		CreatedAt: p.CreationTimestamp.Time,
	}

	statuses := map[string]corev1.ContainerStatus{}
	for _, cs := range p.Status.ContainerStatuses {
		statuses[cs.Name] = cs
	}

	for _, c := range p.Spec.Containers {
		state := model.ContainerState{Name: c.Name}

		if q := c.Resources.Requests.Cpu(); q != nil && !q.IsZero() {
			state.Requests.CPUMilli = q.MilliValue()
			state.HasCPURequest = true
		}
		if q := c.Resources.Requests.Memory(); q != nil && !q.IsZero() {
			state.Requests.MemBytes = q.Value()
			state.HasMemRequest = true
		}
		if q := c.Resources.Limits.Cpu(); q != nil && !q.IsZero() {
			state.Limits.CPUMilli = q.MilliValue()
			state.HasCPULimit = true
		}
		if q := c.Resources.Limits.Memory(); q != nil && !q.IsZero() {
			state.Limits.MemBytes = q.Value()
			state.HasMemLimit = true
		}

		if cs, ok := statuses[c.Name]; ok {
			state.RestartCount = cs.RestartCount
			// lastState holds only the most recent termination. If a container
			// has OOMKilled repeatedly, everything before the last one is gone.
			if t := cs.LastTerminationState.Terminated; t != nil {
				if t.Reason == "OOMKilled" || t.ExitCode == 137 {
					when := t.FinishedAt.Time
					state.LastOOMKill = &when
				}
			}
		}

		pod.Requests.Add(state.Requests)
		pod.Limits.Add(state.Limits)
		pod.Containers = append(pod.Containers, state)
	}

	// Init containers do not add to the steady-state footprint, but a large
	// init request raises the pod's effective scheduling request. Kubernetes
	// takes the max of (sum of app containers, largest init container).
	var maxInit model.Resources
	for _, c := range p.Spec.InitContainers {
		if q := c.Resources.Requests.Cpu(); q != nil && q.MilliValue() > maxInit.CPUMilli {
			maxInit.CPUMilli = q.MilliValue()
		}
		if q := c.Resources.Requests.Memory(); q != nil && q.Value() > maxInit.MemBytes {
			maxInit.MemBytes = q.Value()
		}
	}
	if maxInit.CPUMilli > pod.Requests.CPUMilli {
		pod.Requests.CPUMilli = maxInit.CPUMilli
	}
	if maxInit.MemBytes > pod.Requests.MemBytes {
		pod.Requests.MemBytes = maxInit.MemBytes
	}

	return pod
}

func collectQuotas(ctx context.Context, kube kubernetes.Interface, keep func(string) bool) []model.Quota {
	list, err := kube.CoreV1().ResourceQuotas("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []model.Quota
	for i := range list.Items {
		q := &list.Items[i]
		if !keep(q.Namespace) {
			continue
		}
		out = append(out, model.Quota{
			Namespace: q.Namespace,
			Name:      q.Name,
			Hard:      quantityMap(q.Status.Hard),
			Used:      quantityMap(q.Status.Used),
		})
	}
	return out
}

func quantityMap(rl corev1.ResourceList) map[string]int64 {
	out := map[string]int64{}
	for name, q := range rl {
		key := string(name)
		switch {
		case strings.HasSuffix(key, "cpu"):
			out[key] = q.MilliValue()
		case strings.HasSuffix(key, "memory"):
			out[key] = q.Value()
		default:
			out[key] = q.Value()
		}
	}
	return out
}

func collectLimitRanges(ctx context.Context, kube kubernetes.Interface, keep func(string) bool) []model.LimitRangeInfo {
	list, err := kube.CoreV1().LimitRanges("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []model.LimitRangeInfo
	for i := range list.Items {
		lr := &list.Items[i]
		if !keep(lr.Namespace) {
			continue
		}
		info := model.LimitRangeInfo{Namespace: lr.Namespace, Name: lr.Name}
		for _, item := range lr.Spec.Limits {
			if len(item.DefaultRequest) > 0 {
				info.HasDefaultRequest = true
			}
			if len(item.Default) > 0 {
				info.HasDefaultLimit = true
			}
		}
		out = append(out, info)
	}
	return out
}

// interestingReasons are the event reasons that indicate one tenant's behavior
// affecting another's, or the scheduler running out of room.
var interestingReasons = map[string]bool{
	"Evicted":                true,
	"FailedScheduling":       true,
	"Preempted":              true,
	"OOMKilling":             true,
	"SystemOOM":              true,
	"NodeNotReady":           true,
	"FailedCreatePodSandBox": true,
}

func collectEvents(ctx context.Context, kube kubernetes.Interface, keep func(string) bool) []model.Event {
	list, err := kube.CoreV1().Events("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []model.Event
	for i := range list.Items {
		e := &list.Items[i]
		if !interestingReasons[e.Reason] {
			continue
		}
		if e.InvolvedObject.Kind == "Pod" && !keep(e.Namespace) {
			continue
		}
		last := e.LastTimestamp.Time
		if last.IsZero() {
			last = e.EventTime.Time
		}
		if last.IsZero() {
			last = e.CreationTimestamp.Time
		}
		node := ""
		if e.InvolvedObject.Kind == "Node" {
			node = e.InvolvedObject.Name
		} else if e.Source.Host != "" {
			node = e.Source.Host
		}
		out = append(out, model.Event{
			Namespace: e.Namespace,
			Reason:    e.Reason,
			Message:   e.Message,
			Object:    e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Node:      node,
			Count:     e.Count,
			LastSeen:  last,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

func namespaceFilter(include []string) func(string) bool {
	if len(include) == 0 {
		return func(string) bool { return true }
	}
	set := map[string]bool{}
	for _, ns := range include {
		set[strings.TrimSpace(ns)] = true
	}
	return func(ns string) bool { return set[ns] }
}
