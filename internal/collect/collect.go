// Package collect acquires cluster state across three tiers of data source and
// degrades cleanly when a tier is unavailable.
//
//	tier 0  kube-apiserver          always available, plain read-only RBAC
//	tier 1  metrics.k8s.io          needs metrics-server
//	tier 2  kubelet via nodes/proxy needs the privileged nodes/proxy subresource
//
// No tier is required except tier 0. Whatever is missing becomes an explicit
// entry in the report rather than a silently absent section.
package collect

import (
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Clients bundles the typed clients the collectors need.
type Clients struct {
	Kube    kubernetes.Interface
	Metrics metricsclient.Interface
}

// NewClients builds the clients from a REST config. A failure to construct the
// metrics client is not fatal; it simply removes tier 1.
func NewClients(cfg *rest.Config) (*Clients, error) {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	c := &Clients{Kube: kube}
	if mc, err := metricsclient.NewForConfig(cfg); err == nil {
		c.Metrics = mc
	}
	return c, nil
}

// Options controls collection.
type Options struct {
	// Namespaces restricts the scan. Empty means every namespace.
	Namespaces []string
	// SampleWindow is the gap between the two kubelet scrapes. Zero disables
	// the second scrape, which costs CPU rates and throttling.
	SampleWindow time.Duration
	// Concurrency bounds parallel kubelet scrapes.
	Concurrency int
	// NoKubelet skips tier 2 entirely.
	NoKubelet bool
	// NoMetrics skips tier 1 entirely.
	NoMetrics bool
	// Network fetches per-pod network counters from /stats/summary.
	Network bool
	// Progress receives human-readable progress lines. May be nil.
	Progress func(string)
}

// Result is a collected snapshot plus notes about what could not be collected.
type Result struct {
	Cluster     *model.Cluster
	FailedNodes []string
}

// Collect runs the tiered acquisition.
func Collect(ctx context.Context, clients *Clients, opts Options) (*Result, error) {
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	caps := Probe(ctx, clients)
	if opts.NoMetrics && caps.Metrics.Available {
		caps.Metrics.Available = false
		caps.Metrics.Reason = "disabled with --no-metrics"
	}
	if opts.NoKubelet && caps.Kubelet.Available {
		caps.Kubelet.Available = false
		caps.Kubelet.Reason = "disabled with --no-kubelet"
	}

	if !caps.Core.Available {
		return nil, fmt.Errorf("cannot read the cluster: %s\n  hint: %s", caps.Core.Reason, caps.Core.Hint)
	}

	progress("reading pods, nodes, quotas and events")
	cluster, err := collectCore(ctx, clients.Kube, opts.Namespaces)
	if err != nil {
		return nil, fmt.Errorf("collecting cluster state: %w", err)
	}
	cluster.Caps = caps
	cluster.Usage = model.UsageSource{Kind: "none", Description: "no usage data; structural analysis only"}

	result := &Result{Cluster: cluster}

	// Tier 2 first: its windowed rates are strictly better than the
	// instantaneous readings from tier 1, so when both exist tier 1 becomes the
	// fallback for whatever tier 2 could not measure.
	kubeletApplied := false
	if caps.Kubelet.Available {
		nodeNames := make([]string, 0, len(cluster.Nodes))
		for _, n := range cluster.Nodes {
			nodeNames = append(nodeNames, n.Name)
		}

		concurrency := opts.Concurrency
		if concurrency <= 0 {
			concurrency = 10
		}

		progress(fmt.Sprintf("scraping %d kubelets", len(nodeNames)))
		first := scrapeNodes(ctx, clients.Kube, nodeNames, concurrency, opts.Network)

		var second map[string]*nodeSample
		if opts.SampleWindow > 0 {
			progress(fmt.Sprintf("sampling for %s to measure throttling and CPU rates", opts.SampleWindow))
			select {
			case <-time.After(opts.SampleWindow):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			progress("second kubelet scrape")
			second = scrapeNodes(ctx, clients.Kube, nodeNames, concurrency, opts.Network)
		}

		windowed, window, failed := applyKubelet(cluster, first, second)
		result.FailedNodes = failed
		kubeletApplied = true

		if windowed {
			cluster.Usage = model.UsageSource{
				Kind:        "kubelet",
				Windowed:    true,
				Window:      window,
				Description: fmt.Sprintf("kubelet counters, rates over a %s window", window.Round(time.Second)),
			}
		} else {
			cluster.Usage = model.UsageSource{
				Kind:        "kubelet",
				Description: "kubelet gauges only (memory); no sample window, so CPU rates and throttling are unavailable",
			}
		}
	}

	if caps.Metrics.Available {
		progress("reading metrics.k8s.io")
		if err := applyMetricsServer(ctx, clients.Metrics, cluster); err != nil {
			caps.Metrics.Available = false
			caps.Metrics.Reason = describeErr(err)
			cluster.Caps = caps
		} else if !kubeletApplied {
			cluster.Usage = model.UsageSource{
				Kind:        "metrics-server",
				Description: "metrics-server, point-in-time reading (no history, CPU smoothed over its scrape interval)",
			}
		}
	}

	return result, nil
}
