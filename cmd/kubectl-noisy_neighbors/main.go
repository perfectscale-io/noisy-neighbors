// Command kubectl-noisy_neighbors finds which tenants on a shared Kubernetes
// cluster are consuming what the scheduler promised to someone else.
//
// The underscore in the binary name is deliberate: kubectl maps an underscore in
// a plugin filename onto a dash in the command, so this installs as
// `kubectl noisy-neighbors`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	"github.com/perfectscale-io/noisy-neighbors/internal/analyze"
	"github.com/perfectscale-io/noisy-neighbors/internal/collect"
	"github.com/perfectscale-io/noisy-neighbors/internal/report"
)

// version is overridden at build time via -ldflags.
var version = "dev"

type options struct {
	configFlags *genericclioptions.ConfigFlags

	namespaces   []string
	sampleWindow time.Duration
	concurrency  int
	noKubelet    bool
	noMetrics    bool
	network      bool

	output   string
	wide     bool
	maxNodes int
	quiet    bool
	exitCode bool

	saturationPct  float64
	aggressorRatio float64
	strandRatio    float64
	throttleWarn   float64
	oomWindow      time.Duration
}

func main() {
	if err := newCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	o := &options{configFlags: genericclioptions.NewConfigFlags(true)}

	cmd := &cobra.Command{
		Use:   "kubectl noisy-neighbors",
		Short: "Find which tenants on a shared cluster are hurting the others",
		Long: `Builds the aggressor/victim picture for a multi-tenant Kubernetes cluster.

Three tiers of data are used, and only the first is required:

  tier 0  kube-apiserver           co-tenancy, overcommit, OOMKill forensics, quotas
  tier 1  metrics.k8s.io           actual usage vs requests
  tier 2  kubelet via nodes/proxy  CFS throttling, OOM counters, per-pod network

Whatever is unavailable is reported as unavailable rather than silently skipped.`,
		Example: `  # Full scan with a 30s sampling window for throttling
  kubectl noisy-neighbors

  # Skip the privileged nodes/proxy reads
  kubectl noisy-neighbors --no-kubelet

  # A shareable report for the team that owns the noisy workload
  kubectl noisy-neighbors -o markdown > report.md

  # Gate CI on critical findings
  kubectl noisy-neighbors --exit-code`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(cmd *cobra.Command, args []string) error { return o.run(cmd.Context()) },
	}

	o.configFlags.AddFlags(cmd.Flags())
	f := cmd.Flags()

	f.StringSliceVar(&o.namespaces, "namespaces", nil, "restrict the scan to these namespaces (default: all)")
	f.DurationVar(&o.sampleWindow, "sample-window", 30*time.Second, "gap between the two kubelet scrapes; 0 disables rates and throttling")
	f.IntVar(&o.concurrency, "concurrency", 10, "parallel kubelet scrapes")
	f.BoolVar(&o.noKubelet, "no-kubelet", false, "skip tier 2 (no nodes/proxy reads)")
	f.BoolVar(&o.noMetrics, "no-metrics", false, "skip tier 1 (no metrics.k8s.io reads)")
	f.BoolVar(&o.network, "network", false, "also collect per-pod network counters from the kubelet summary API")

	f.StringVarP(&o.output, "output", "o", "table", "output format: table, json or markdown")
	f.BoolVar(&o.wide, "wide", false, "show every node, including quiet ones, and list co-tenants")
	f.IntVar(&o.maxNodes, "max-nodes", 10, "maximum nodes to detail; 0 for all")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "suppress progress output")
	f.BoolVar(&o.exitCode, "exit-code", false, "exit 1 on warnings and 2 on critical findings")

	f.Float64Var(&o.saturationPct, "saturation-pct", 85, "actual usage at which a node counts as saturated")
	f.Float64Var(&o.aggressorRatio, "aggressor-ratio", 1.5, "usage/request ratio above which a workload is an aggressor")
	f.Float64Var(&o.strandRatio, "strand-ratio", 2.0, "request/usage ratio above which capacity counts as stranded")
	f.Float64Var(&o.throttleWarn, "throttle-warn-pct", 5, "CFS throttling percentage that triggers a finding")
	f.DurationVar(&o.oomWindow, "oom-window", 24*time.Hour, "how recent an OOMKill must be to count")

	cmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run:   func(cmd *cobra.Command, args []string) { fmt.Println(version) },
	})

	return cmd
}

func (o *options) run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A sampling run sits idle for the whole window, so make Ctrl-C responsive.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	format, err := report.ParseFormat(o.output)
	if err != nil {
		return err
	}

	cfg, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	clients, err := collect.NewClients(cfg)
	if err != nil {
		return err
	}

	progress := func(msg string) {
		if !o.quiet && format == "table" {
			fmt.Fprintf(os.Stderr, "  … %s\n", msg)
		}
	}

	result, err := collect.Collect(ctx, clients, collect.Options{
		Namespaces:   o.namespaces,
		SampleWindow: o.sampleWindow,
		Concurrency:  o.concurrency,
		NoKubelet:    o.noKubelet,
		NoMetrics:    o.noMetrics,
		Network:      o.network,
		Progress:     progress,
	})
	if err != nil {
		return err
	}

	for _, f := range result.FailedNodes {
		fmt.Fprintf(os.Stderr, "  ! node scrape failed: %s\n", f)
	}

	th := analyze.DefaultThresholds()
	th.NodeSaturationPct = o.saturationPct
	th.AggressorRatio = o.aggressorRatio
	th.StrandRatio = o.strandRatio
	th.ThrottleWarnPct = o.throttleWarn
	th.OOMWindow = o.oomWindow

	r := analyze.Run(result.Cluster, th)

	switch format {
	case "json":
		if err := report.JSON(r, os.Stdout); err != nil {
			return err
		}
	case "markdown":
		if err := report.Markdown(r, os.Stdout); err != nil {
			return err
		}
	default:
		opts := report.DefaultOptions()
		opts.Wide = o.wide
		opts.MaxNodes = o.maxNodes
		if err := report.Table(r, opts); err != nil {
			return err
		}
	}

	if o.exitCode {
		os.Exit(report.ExitCode(r))
	}
	return nil
}
