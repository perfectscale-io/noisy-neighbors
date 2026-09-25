package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Tier 2 reads the kubelet directly through the apiserver's nodes/proxy
// subresource. This is the same data Prometheus would scrape, so the signals
// Prometheus users take for granted -- real CFS throttling, cumulative OOM
// counts, per-pod network -- are available without running Prometheus.
//
// The catch is that the useful metrics are counters. A single scrape of
// container_cpu_cfs_throttled_periods_total tells you how much a container has
// been throttled since it started, which is close to meaningless: a container
// throttled hard during a slow startup and healthy ever since looks permanently
// broken. Two scrapes a known interval apart give a rate, which is the number
// worth reporting.

const (
	metricContainerCPUSeconds = "container_cpu_usage_seconds_total"
	metricContainerMemWS      = "container_memory_working_set_bytes"
	metricNodeCPUSeconds      = "node_cpu_usage_seconds_total"
	metricNodeMemWS           = "node_memory_working_set_bytes"
	metricThrottledPeriods    = "container_cpu_cfs_throttled_periods_total"
	metricPeriods             = "container_cpu_cfs_periods_total"
	metricOOMEvents           = "container_oom_events_total"
)

var resourceMetrics = map[string]bool{
	metricContainerCPUSeconds: true,
	metricContainerMemWS:      true,
	metricNodeCPUSeconds:      true,
	metricNodeMemWS:           true,
}

var cadvisorMetrics = map[string]bool{
	metricThrottledPeriods: true,
	metricPeriods:          true,
	metricOOMEvents:        true,
}

type containerCounters struct {
	cpuSeconds       float64
	memWorkingSet    int64
	throttledPeriods int64
	periods          int64
	oomEvents        int64

	sawCPU      bool
	sawMem      bool
	sawThrottle bool
	sawOOM      bool
}

type podCounters struct {
	rxBytes int64
	txBytes int64
	sawNet  bool
}

// nodeSample is one node's counters at one instant.
type nodeSample struct {
	node       string
	at         time.Time
	containers map[string]*containerCounters // "ns/pod/container"
	pods       map[string]*podCounters       // "ns/pod"
	nodeCPU    float64
	nodeMemWS  int64
	sawNodeCPU bool
	err        error
}

type summaryPayload struct {
	Node struct {
		NodeName string `json:"nodeName"`
	} `json:"node"`
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Network *struct {
			RxBytes *int64 `json:"rxBytes"`
			TxBytes *int64 `json:"txBytes"`
		} `json:"network"`
	} `json:"pods"`
}

// scrapeNodes fetches one sample from every node, with bounded concurrency.
//
// A node that fails is recorded and skipped rather than failing the run: on a
// large cluster there is usually at least one node that is draining, unreachable
// or mid-upgrade, and a partial report that says which nodes it missed is far
// more useful than no report.
func scrapeNodes(ctx context.Context, kube kubernetes.Interface, nodes []string, concurrency int, withNetwork bool) map[string]*nodeSample {
	if concurrency < 1 {
		concurrency = 1
	}

	out := make(map[string]*nodeSample, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for _, name := range nodes {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			s := scrapeOneNode(ctx, kube, node, withNetwork)
			mu.Lock()
			out[node] = s
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

func scrapeOneNode(ctx context.Context, kube kubernetes.Interface, node string, withNetwork bool) *nodeSample {
	s := &nodeSample{
		node:       node,
		at:         time.Now(),
		containers: map[string]*containerCounters{},
		pods:       map[string]*podCounters{},
	}

	raw, err := kubeletGet(ctx, kube, node, "metrics", "resource")
	if err != nil {
		s.err = fmt.Errorf("metrics/resource: %w", err)
		return s
	}
	s.applyResourceMetrics(parsePromText(raw, resourceMetrics))

	// cadvisor is the expensive one and the only source of throttling. A
	// failure here is not fatal: usage still works.
	if raw, err := kubeletGet(ctx, kube, node, "metrics", "cadvisor"); err == nil {
		s.applyCadvisorMetrics(parsePromText(raw, cadvisorMetrics))
	}

	if withNetwork {
		if raw, err := kubeletGet(ctx, kube, node, "stats", "summary"); err == nil {
			var payload summaryPayload
			if json.Unmarshal(raw, &payload) == nil {
				for _, p := range payload.Pods {
					if p.Network == nil {
						continue
					}
					pc := &podCounters{sawNet: true}
					if p.Network.RxBytes != nil {
						pc.rxBytes = *p.Network.RxBytes
					}
					if p.Network.TxBytes != nil {
						pc.txBytes = *p.Network.TxBytes
					}
					s.pods[p.PodRef.Namespace+"/"+p.PodRef.Name] = pc
				}
			}
		}
	}

	return s
}

func kubeletGet(ctx context.Context, kube kubernetes.Interface, node string, path ...string) ([]byte, error) {
	return kube.CoreV1().RESTClient().Get().
		Resource("nodes").Name(node).SubResource("proxy").
		Suffix(path...).
		DoRaw(ctx)
}

func (s *nodeSample) container(key string) *containerCounters {
	if c, ok := s.containers[key]; ok {
		return c
	}
	c := &containerCounters{}
	s.containers[key] = c
	return c
}

func containerKey(l map[string]string) string {
	ns, pod, ctr := l["namespace"], l["pod"], l["container"]
	// An empty or "POD" container name is the pause container / pod-level
	// rollup, which would double-count against the real containers.
	if ns == "" || pod == "" || ctr == "" || ctr == "POD" {
		return ""
	}
	return ns + "/" + pod + "/" + ctr
}

func (s *nodeSample) applyResourceMetrics(samples []sample) {
	for _, sm := range samples {
		switch sm.Name {
		case metricNodeCPUSeconds:
			s.nodeCPU = sm.Value
			s.sawNodeCPU = true
			continue
		case metricNodeMemWS:
			s.nodeMemWS = int64(sm.Value)
			continue
		}

		key := containerKey(sm.Labels)
		if key == "" {
			continue
		}
		c := s.container(key)
		switch sm.Name {
		case metricContainerCPUSeconds:
			c.cpuSeconds = sm.Value
			c.sawCPU = true
		case metricContainerMemWS:
			c.memWorkingSet = int64(sm.Value)
			c.sawMem = true
		}
	}
}

func (s *nodeSample) applyCadvisorMetrics(samples []sample) {
	for _, sm := range samples {
		key := containerKey(sm.Labels)
		if key == "" {
			continue
		}
		c := s.container(key)
		switch sm.Name {
		case metricThrottledPeriods:
			c.throttledPeriods = int64(sm.Value)
			c.sawThrottle = true
		case metricPeriods:
			c.periods = int64(sm.Value)
			c.sawThrottle = true
		case metricOOMEvents:
			c.oomEvents = int64(sm.Value)
			c.sawOOM = true
		}
	}
}

// applyKubelet merges one or two rounds of samples into the cluster snapshot.
//
// When second is nil, only gauges are usable: memory working set is a real
// number, but CPU and throttling are counters with nothing to diff against.
func applyKubelet(cluster *model.Cluster, first, second map[string]*nodeSample) (windowed bool, window time.Duration, failed []string) {
	for node, s := range first {
		if s.err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", node, s.err))
		}
	}

	byPod := map[string]*model.Pod{}
	for _, p := range cluster.Pods {
		byPod[p.Key()] = p
	}
	byNode := map[string]*model.Node{}
	for _, n := range cluster.Nodes {
		byNode[n.Name] = n
	}

	// Establish the observed window from the samples themselves rather than the
	// requested duration: scraping a large cluster takes real time and the
	// actual gap is what the rates must be divided by.
	var totalWindow time.Duration
	var windowCount int
	if second != nil {
		for node, b := range second {
			a, ok := first[node]
			if !ok || a.err != nil || b.err != nil {
				continue
			}
			totalWindow += b.at.Sub(a.at)
			windowCount++
		}
	}
	if windowCount > 0 {
		window = totalWindow / time.Duration(windowCount)
		windowed = window > 0
	}

	for nodeName, a := range first {
		if a.err != nil {
			continue
		}
		b := a
		var elapsed float64
		if second != nil {
			if s, ok := second[nodeName]; ok && s.err == nil {
				b = s
				elapsed = s.at.Sub(a.at).Seconds()
			}
		}

		if n, ok := byNode[nodeName]; ok {
			usage := model.Resources{MemBytes: b.nodeMemWS}
			if elapsed > 0 && a.sawNodeCPU && b.sawNodeCPU {
				usage.CPUMilli = int64((b.nodeCPU - a.nodeCPU) / elapsed * 1000)
			}
			if usage.MemBytes > 0 || usage.CPUMilli > 0 {
				n.Usage = &usage
			}
		}

		for key, bc := range b.containers {
			ns, podName, ctrName, ok := splitContainerKey(key)
			if !ok {
				continue
			}
			pod, ok := byPod[ns+"/"+podName]
			if !ok {
				continue
			}
			idx := -1
			for i := range pod.Containers {
				if pod.Containers[i].Name == ctrName {
					idx = i
					break
				}
			}
			if idx < 0 {
				continue
			}

			usage := model.Resources{}
			if bc.sawMem {
				usage.MemBytes = bc.memWorkingSet
			}
			if ac, ok := a.containers[key]; ok && elapsed > 0 && ac.sawCPU && bc.sawCPU {
				delta := bc.cpuSeconds - ac.cpuSeconds
				if delta >= 0 {
					usage.CPUMilli = int64(delta / elapsed * 1000)
				}
			}
			if usage.MemBytes > 0 || usage.CPUMilli > 0 {
				u := usage
				pod.Containers[idx].Usage = &u
			}

			if ac, ok := a.containers[key]; ok && elapsed > 0 && ac.sawThrottle && bc.sawThrottle {
				periods := bc.periods - ac.periods
				throttled := bc.throttledPeriods - ac.throttledPeriods
				// A container with no CPU limit is never subject to CFS
				// throttling and reports zero periods. That is a finding of its
				// own, not a throttling measurement.
				if periods > 0 && throttled >= 0 {
					pod.Containers[idx].Throttle = &model.ThrottleStats{
						Periods:          periods,
						ThrottledPeriods: throttled,
						Percent:          float64(throttled) / float64(periods) * 100,
					}
				}
			}

			if bc.sawOOM {
				events := bc.oomEvents
				pod.Containers[idx].OOMEvents = &events
			}
		}

		for key, bp := range b.pods {
			pod, ok := byPod[key]
			if !ok || !bp.sawNet {
				continue
			}
			if ap, ok := a.pods[key]; ok && elapsed > 0 && ap.sawNet {
				rx := bp.rxBytes - ap.rxBytes
				tx := bp.txBytes - ap.txBytes
				if rx >= 0 && tx >= 0 {
					pod.NetRxBytes = &rx
					pod.NetTxBytes = &tx
				}
			}
		}
	}

	// Roll container usage up to the pod.
	for _, pod := range cluster.Pods {
		var total model.Resources
		any := false
		for i := range pod.Containers {
			if u := pod.Containers[i].Usage; u != nil {
				total.Add(*u)
				any = true
			}
		}
		if any {
			t := total
			pod.Usage = &t
		}
	}

	return windowed, window, failed
}

func splitContainerKey(key string) (ns, pod, container string, ok bool) {
	first := -1
	last := -1
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || last <= first {
		return "", "", "", false
	}
	return key[:first], key[first+1 : last], key[last+1:], true
}
