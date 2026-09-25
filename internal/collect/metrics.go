package collect

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// applyMetricsServer fills in usage from metrics.k8s.io.
//
// These numbers are a point-in-time reading: metrics-server keeps roughly the
// last scrape in memory and is explicitly not a historical store. CPU in
// particular is a rate averaged over its own scrape window, which smooths
// exactly the spikes a noisy-neighbor hunt cares about. Prefer tier 2 when
// both are available.
func applyMetricsServer(ctx context.Context, mc metricsclient.Interface, cluster *model.Cluster) error {
	podMetrics, err := mc.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	byKey := map[string]*model.Pod{}
	for _, p := range cluster.Pods {
		byKey[p.Key()] = p
	}

	for i := range podMetrics.Items {
		pm := &podMetrics.Items[i]
		pod, ok := byKey[pm.Namespace+"/"+pm.Name]
		if !ok {
			continue
		}

		usageByContainer := map[string]model.Resources{}
		var total model.Resources
		for _, c := range pm.Containers {
			r := model.Resources{
				CPUMilli: c.Usage.Cpu().MilliValue(),
				MemBytes: c.Usage.Memory().Value(),
			}
			usageByContainer[c.Name] = r
			total.Add(r)
		}

		for j := range pod.Containers {
			if r, ok := usageByContainer[pod.Containers[j].Name]; ok {
				usage := r
				pod.Containers[j].Usage = &usage
			}
		}
		podTotal := total
		pod.Usage = &podTotal
	}

	nodeMetrics, err := mc.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err == nil {
		byName := map[string]*model.Node{}
		for _, n := range cluster.Nodes {
			byName[n.Name] = n
		}
		for i := range nodeMetrics.Items {
			nm := &nodeMetrics.Items[i]
			if n, ok := byName[nm.Name]; ok {
				usage := model.Resources{
					CPUMilli: nm.Usage.Cpu().MilliValue(),
					MemBytes: nm.Usage.Memory().Value(),
				}
				n.Usage = &usage
			}
		}
	}

	return nil
}
