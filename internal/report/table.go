// Package report renders an analysis for humans and for machines.
package report

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/perfectscale-io/noisy-neighbors/internal/analyze"
	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// Options controls rendering.
type Options struct {
	Color    bool
	Wide     bool
	MaxNodes int
	Out      io.Writer
}

// DefaultOptions detects terminal capabilities.
func DefaultOptions() Options {
	out := os.Stdout
	color := false
	if f, ok := interface{}(out).(*os.File); ok {
		color = term.IsTerminal(int(f.Fd())) && os.Getenv("NO_COLOR") == ""
	}
	return Options{Color: color, MaxNodes: 10, Out: out}
}

const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiGreen  = "\033[32m"
	ansiDim    = "\033[2m"
	ansiBold   = "\033[1m"
)

type painter struct{ on bool }

func (p painter) c(code, s string) string {
	if !p.on {
		return s
	}
	return code + s + ansiReset
}
func (p painter) red(s string) string    { return p.c(ansiRed, s) }
func (p painter) yellow(s string) string { return p.c(ansiYellow, s) }
func (p painter) green(s string) string  { return p.c(ansiGreen, s) }
func (p painter) dim(s string) string    { return p.c(ansiDim, s) }
func (p painter) bold(s string) string   { return p.c(ansiBold, s) }

func (p painter) severity(s analyze.Severity) string {
	switch s {
	case analyze.SevCritical:
		return p.red(s.String())
	case analyze.SevWarn:
		return p.yellow(s.String())
	}
	return p.dim(s.String())
}

// Table writes the human-readable report.
func Table(r *analyze.Report, opts Options) error {
	w := opts.Out
	if w == nil {
		w = os.Stdout
	}
	p := painter{on: opts.Color}

	writeHeader(w, p, r)
	writeSources(w, p, r)
	writeBlastRadius(w, p, r, opts)
	writeTenants(w, p, r)
	writeFindings(w, p, r)
	writeSkipped(w, p, r)
	writeFooter(w, p, r)

	return nil
}

func rule(w io.Writer, p painter) {
	fmt.Fprintln(w, p.dim(strings.Repeat("─", 78)))
}

func section(w io.Writer, p painter, title string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, p.bold(title))
	rule(w, p)
}

func writeHeader(w io.Writer, p painter, r *analyze.Report) {
	c := r.Cluster
	fmt.Fprintf(w, "\n%s  %s\n",
		p.bold("noisy-neighbors"),
		p.dim(fmt.Sprintf("· %d nodes, %d pods, %d namespaces · %s",
			len(c.Nodes), len(c.Pods), len(c.Namespaces),
			c.CollectedAt.Format("2006-01-02 15:04:05 MST"))))
}

func writeSources(w io.Writer, p painter, r *analyze.Report) {
	section(w, p, "DATA SOURCES")
	for _, cap := range r.Cluster.Caps.All() {
		mark := p.green("✓")
		note := ""
		if !cap.Available {
			mark = p.yellow("✗")
			note = p.dim("  " + cap.Reason)
		}
		fmt.Fprintf(w, "  %s %-34s %s%s\n", mark, cap.Name, p.dim(cap.Tier.String()), note)
		if !cap.Available && cap.Hint != "" {
			fmt.Fprintf(w, "      %s\n", p.dim("→ "+cap.Hint))
		}
	}
	fmt.Fprintf(w, "\n  %s %s\n", p.dim("usage:"), r.Cluster.Usage.Description)
}

func writeBlastRadius(w io.Writer, p painter, r *analyze.Report, opts Options) {
	section(w, p, "BLAST RADIUS")

	shown := 0
	for _, n := range r.Nodes {
		// Quiet nodes are noise in a report about noise.
		if n.Severity == analyze.SevInfo && !opts.Wide {
			continue
		}
		if opts.MaxNodes > 0 && shown >= opts.MaxNodes {
			fmt.Fprintf(w, "\n  %s\n", p.dim(fmt.Sprintf("… %d more nodes with findings (use --max-nodes 0 for all)", len(r.Nodes)-shown)))
			break
		}
		shown++

		fmt.Fprintf(w, "\n%-8s %s  %s\n",
			p.severity(n.Severity),
			p.bold(n.Name),
			p.dim(fmt.Sprintf("%d tenants", n.TenantCount)))

		fmt.Fprintf(w, "  %s requested %s   limits %s\n",
			p.dim("cpu"),
			fmt.Sprintf("%5.0f%%", n.RequestedCPUPct),
			fmt.Sprintf("%5.0f%%", n.LimitCPUPct))
		fmt.Fprintf(w, "  %s requested %s   limits %s\n",
			p.dim("mem"),
			fmt.Sprintf("%5.0f%%", n.RequestedMemPct),
			fmt.Sprintf("%5.0f%%", n.LimitMemPct))

		if n.ActualCPUPct != nil && n.ActualMemPct != nil {
			// The gap between requested and actual is the whole point: it is
			// the distance between what the scheduler believes and what the
			// hardware is doing.
			fmt.Fprintf(w, "  %s cpu %s   mem %s\n",
				p.dim("actual   "),
				colorPct(p, *n.ActualCPUPct),
				colorPct(p, *n.ActualMemPct))
		}
		if n.MemoryPressure {
			fmt.Fprintf(w, "  %s\n", p.red("node reports MemoryPressure"))
		}

		for _, f := range n.Aggressors {
			fmt.Fprintf(w, "    %s %-48s %s\n",
				p.red("aggressor"), truncate(f.Workload, 48), f.Message)
		}
		for _, f := range n.Exposed {
			label := p.yellow("exposed  ")
			if f.Severity == analyze.SevCritical {
				label = p.red("exposed  ")
			}
			fmt.Fprintf(w, "    %s %-48s %s\n", label, truncate(f.Workload, 48), f.Message)
		}

		if opts.Wide {
			var names []string
			for _, t := range n.Tenants {
				names = append(names, t.Namespace)
			}
			fmt.Fprintf(w, "    %s %s\n", p.dim("sharing  "), p.dim(strings.Join(names, ", ")))
		}
	}

	if shown == 0 {
		fmt.Fprintf(w, "\n  %s\n", p.green("No aggressor/victim findings on any node."))
		if !r.Cluster.Caps.UsageAvailable() {
			fmt.Fprintf(w, "  %s\n", p.dim("Note: without usage data this only means nothing was structurally alarming."))
		}
	}
}

func colorPct(p painter, v float64) string {
	s := fmt.Sprintf("%5.0f%%", v)
	switch {
	case v >= 90:
		return p.red(s)
	case v >= 75:
		return p.yellow(s)
	}
	return s
}

func writeTenants(w io.Writer, p painter, r *analyze.Report) {
	if len(r.Tenants) == 0 {
		return
	}
	section(w, p, "TENANTS")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	hasUsage := r.Cluster.Caps.UsageAvailable()

	if hasUsage {
		fmt.Fprintln(tw, "  NAMESPACE\tPODS\tNODES\tREQ CPU\tREQ MEM\tUSED CPU\tUSED MEM\tSTRANDED MEM\tOOM\tTHROTTLED")
	} else {
		fmt.Fprintln(tw, "  NAMESPACE\tPODS\tNODES\tREQ CPU\tREQ MEM\tLIM MEM\tQOS (G/B/BE)\tOOM\tNO-REQUESTS")
	}

	for _, t := range r.Tenants {
		if hasUsage {
			usedCPU, usedMem := "-", "-"
			if t.Usage != nil {
				usedCPU = model.FormatCPU(t.Usage.CPUMilli)
				usedMem = model.FormatMem(t.Usage.MemBytes)
			}
			stranded := "-"
			if t.StrandedMemBytes > 0 {
				stranded = model.FormatMem(t.StrandedMemBytes)
			}
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
				t.Namespace, t.Pods, len(t.Nodes),
				model.FormatCPU(t.Requests.CPUMilli), model.FormatMem(t.Requests.MemBytes),
				usedCPU, usedMem, stranded, t.RecentOOMKills, t.ThrottledContainers)
		} else {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\t%s\t%s\t%d/%d/%d\t%d\t%d\n",
				t.Namespace, t.Pods, len(t.Nodes),
				model.FormatCPU(t.Requests.CPUMilli), model.FormatMem(t.Requests.MemBytes),
				model.FormatMem(t.Limits.MemBytes),
				t.Guaranteed, t.Burstable, t.BestEffort,
				t.RecentOOMKills, t.MissingRequests)
		}
	}
	tw.Flush()

	if hasUsage {
		fmt.Fprintf(w, "\n  %s\n", p.dim("stranded mem = requested but unused, at a ratio the scheduler will not offer to anyone else"))
	}
}

func writeFindings(w io.Writer, p painter, r *analyze.Report) {
	all := append(append([]analyze.Finding{}, r.ClusterFindings...), r.QuotaFindings...)
	if len(all) == 0 {
		return
	}
	section(w, p, "CLUSTER AND QUOTA FINDINGS")

	for _, f := range all {
		scope := f.Tenant
		if scope == "" {
			scope = "cluster"
		}
		fmt.Fprintf(w, "  %-8s %-24s %s\n", p.severity(f.Severity), truncate(scope, 24), f.Message)
	}
}

func writeSkipped(w io.Writer, p painter, r *analyze.Report) {
	if len(r.Skipped) == 0 {
		return
	}
	section(w, p, "NOT MEASURED")

	for _, s := range r.Skipped {
		fmt.Fprintf(w, "  %s %s\n", p.yellow("—"), s.Analysis)
		fmt.Fprintf(w, "    %s\n", p.dim(s.Reason))
		if s.Hint != "" {
			fmt.Fprintf(w, "    %s\n", p.dim("→ "+s.Hint))
		}
	}
}

func writeFooter(w io.Writer, p painter, r *analyze.Report) {
	fmt.Fprintln(w)
	rule(w, p)
	fmt.Fprintf(w, "%s\n", p.dim(
		"This is a point-in-time snapshot. Findings assert co-location and structural risk,"))
	fmt.Fprintf(w, "%s\n\n", p.dim(
		"not proven causation: \"exposed to\" is not the same claim as \"was killed by\"."))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
