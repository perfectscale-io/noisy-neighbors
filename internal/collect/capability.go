package collect

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Probe determines which data sources this credential can actually reach.
//
// Every probe is an attempted read rather than a SelfSubjectAccessReview: RBAC
// can say yes while the component behind it is missing or unhealthy, and a
// report that promises data it cannot deliver is worse than one that admits the
// gap up front.
func Probe(ctx context.Context, clients *Clients) model.Capabilities {
	caps := model.Capabilities{
		Core:    probeCore(ctx, clients.Kube),
		Metrics: probeMetrics(ctx, clients.Metrics),
	}
	caps.Kubelet = probeKubelet(ctx, clients.Kube)

	caps.Core.TierName = caps.Core.Tier.String()
	caps.Metrics.TierName = caps.Metrics.Tier.String()
	caps.Kubelet.TierName = caps.Kubelet.Tier.String()
	return caps
}

func probeCore(ctx context.Context, kube kubernetes.Interface) model.Capability {
	c := model.Capability{Name: "kube-apiserver", Tier: model.Tier0}

	if _, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		c.Reason = describeErr(err)
		c.Hint = "needs list on nodes and pods cluster-wide; see docs/rbac.yaml for a minimal read-only ClusterRole"
		return c
	}
	if _, err := kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		c.Reason = describeErr(err)
		c.Hint = "needs list on pods across all namespaces; see docs/rbac.yaml"
		return c
	}
	c.Available = true
	return c
}

func probeMetrics(ctx context.Context, mc metricsclient.Interface) model.Capability {
	c := model.Capability{Name: "metrics-server (metrics.k8s.io)", Tier: model.Tier1}
	if mc == nil {
		c.Reason = "metrics client not initialised"
		return c
	}

	_, err := mc.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{Limit: 1})
	if err == nil {
		c.Available = true
		return c
	}

	switch {
	case apierrors.IsNotFound(err), isNoMatch(err):
		c.Reason = "metrics.k8s.io API not served (metrics-server not installed)"
		c.Hint = "kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml"
	case apierrors.IsForbidden(err):
		c.Reason = "forbidden on metrics.k8s.io"
		c.Hint = "grant get/list on nodes and pods in apiGroup metrics.k8s.io"
	case apierrors.IsServiceUnavailable(err):
		c.Reason = "metrics.k8s.io registered but unavailable (metrics-server unhealthy)"
		c.Hint = "kubectl -n kube-system get deploy metrics-server && kubectl -n kube-system logs deploy/metrics-server"
	default:
		c.Reason = describeErr(err)
	}
	return c
}

func probeKubelet(ctx context.Context, kube kubernetes.Interface) model.Capability {
	c := model.Capability{Name: "kubelet via nodes/proxy", Tier: model.Tier2}

	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil || len(nodes.Items) == 0 {
		c.Reason = "no nodes available to probe"
		return c
	}
	name := nodes.Items[0].Name

	_, err = kube.CoreV1().RESTClient().Get().
		Resource("nodes").Name(name).SubResource("proxy").
		Suffix("metrics", "resource").
		DoRaw(ctx)
	if err == nil {
		c.Available = true
		return c
	}

	switch {
	case apierrors.IsForbidden(err):
		c.Reason = "forbidden on nodes/proxy"
		c.Hint = "grant get on the nodes/proxy subresource (privileged); or run with --no-kubelet to skip tier 2"
	case apierrors.IsNotFound(err):
		c.Reason = "kubelet /metrics/resource endpoint not found"
		c.Hint = "kubelet may be too old, or the node proxy path is blocked by a network policy"
	default:
		c.Reason = describeErr(err)
		c.Hint = "run with --no-kubelet to skip tier 2 and report on tiers 0 and 1 only"
	}
	return c
}

// isNoMatch covers the discovery-level "the server could not find the requested
// resource" that surfaces when an aggregated API is simply not registered.
func isNoMatch(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "could not find the requested resource") ||
		strings.Contains(s, "no matches for kind") ||
		strings.Contains(s, "the server is currently unable to handle the request")
}

func describeErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return fmt.Sprintf("%s", msg)
}
