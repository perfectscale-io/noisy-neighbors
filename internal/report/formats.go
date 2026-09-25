package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/perfectscale-io/noisy-neighbors/internal/analyze"
	"github.com/perfectscale-io/noisy-neighbors/internal/model"
)

// JSON writes the full analysis for machine consumption.
func JSON(r *analyze.Report, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Markdown writes a report suitable for pasting into an issue or a doc. This is
// the shareable artifact: the thing someone puts in front of the team that owns
// the noisy workload.
func Markdown(r *analyze.Report, w io.Writer) error {
	c := r.Cluster

	fmt.Fprintf(w, "# Noisy neighbors report\n\n")
	fmt.Fprintf(w, "%d nodes · %d pods · %d namespaces · collected %s\n\n",
		len(c.Nodes), len(c.Pods), len(c.Namespaces), c.CollectedAt.Format("2006-01-02 15:04:05 MST"))

	fmt.Fprintf(w, "## Data sources\n\n")
	fmt.Fprintf(w, "| Source | Tier | Available | Note |\n|---|---|---|---|\n")
	for _, cap := range c.Caps.All() {
		avail := "yes"
		if !cap.Available {
			avail = "no"
		}
		note := cap.Reason
		if note == "" {
			note = "-"
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s |\n", cap.Name, cap.Tier.String(), avail, note)
	}
	fmt.Fprintf(w, "\nUsage measurement: %s\n\n", c.Usage.Description)

	fmt.Fprintf(w, "## Blast radius\n\n")
	any := false
	for _, n := range r.Nodes {
		if n.Severity == analyze.SevInfo {
			continue
		}
		any = true
		fmt.Fprintf(w, "### `%s` — %s (%d tenants)\n\n", n.Name, n.Severity, n.TenantCount)

		fmt.Fprintf(w, "| | requested | limits | actual |\n|---|---|---|---|\n")
		actualCPU, actualMem := "n/a", "n/a"
		if n.ActualCPUPct != nil {
			actualCPU = fmt.Sprintf("%.0f%%", *n.ActualCPUPct)
		}
		if n.ActualMemPct != nil {
			actualMem = fmt.Sprintf("%.0f%%", *n.ActualMemPct)
		}
		fmt.Fprintf(w, "| cpu | %.0f%% | %.0f%% | %s |\n", n.RequestedCPUPct, n.LimitCPUPct, actualCPU)
		fmt.Fprintf(w, "| mem | %.0f%% | %.0f%% | %s |\n\n", n.RequestedMemPct, n.LimitMemPct, actualMem)

		if len(n.Aggressors) > 0 {
			fmt.Fprintf(w, "**Aggressors**\n\n")
			for _, f := range n.Aggressors {
				fmt.Fprintf(w, "- `%s` — %s\n", f.Workload, f.Message)
			}
			fmt.Fprintln(w)
		}
		if len(n.Exposed) > 0 {
			fmt.Fprintf(w, "**Exposed**\n\n")
			for _, f := range n.Exposed {
				fmt.Fprintf(w, "- `%s` — %s\n", f.Workload, f.Message)
			}
			fmt.Fprintln(w)
		}
	}
	if !any {
		fmt.Fprintf(w, "No aggressor/victim findings.\n\n")
	}

	if len(r.Tenants) > 0 {
		fmt.Fprintf(w, "## Tenants\n\n")
		fmt.Fprintf(w, "| Namespace | Pods | Nodes | Req CPU | Req Mem | Used Mem | Stranded Mem | OOM | Throttled |\n")
		fmt.Fprintf(w, "|---|---|---|---|---|---|---|---|---|\n")
		for _, t := range r.Tenants {
			usedMem := "-"
			if t.Usage != nil {
				usedMem = model.FormatMem(t.Usage.MemBytes)
			}
			stranded := "-"
			if t.StrandedMemBytes > 0 {
				stranded = model.FormatMem(t.StrandedMemBytes)
			}
			fmt.Fprintf(w, "| `%s` | %d | %d | %s | %s | %s | %s | %d | %d |\n",
				t.Namespace, t.Pods, len(t.Nodes),
				model.FormatCPU(t.Requests.CPUMilli), model.FormatMem(t.Requests.MemBytes),
				usedMem, stranded, t.RecentOOMKills, t.ThrottledContainers)
		}
		fmt.Fprintln(w)
	}

	all := append(append([]analyze.Finding{}, r.ClusterFindings...), r.QuotaFindings...)
	if len(all) > 0 {
		fmt.Fprintf(w, "## Cluster and quota findings\n\n")
		for _, f := range all {
			scope := f.Tenant
			if scope == "" {
				scope = "cluster"
			}
			fmt.Fprintf(w, "- **%s** `%s` — %s\n", f.Severity, scope, f.Message)
		}
		fmt.Fprintln(w)
	}

	if len(r.Skipped) > 0 {
		fmt.Fprintf(w, "## Not measured\n\n")
		for _, s := range r.Skipped {
			fmt.Fprintf(w, "- **%s**\n  - %s\n", s.Analysis, s.Reason)
			if s.Hint != "" {
				fmt.Fprintf(w, "  - %s\n", s.Hint)
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "---\n\n")
	fmt.Fprintf(w, "_Point-in-time snapshot. Findings assert co-location and structural risk, not proven causation._\n")

	return nil
}

// ExitCode maps the worst finding onto a process exit code so the tool can gate
// CI. 0 clean, 1 warnings, 2 critical.
func ExitCode(r *analyze.Report) int {
	worst := analyze.SevInfo
	bump := func(s analyze.Severity) {
		if s > worst {
			worst = s
		}
	}
	for _, n := range r.Nodes {
		bump(n.Severity)
	}
	for _, f := range r.ClusterFindings {
		bump(f.Severity)
	}
	for _, f := range r.QuotaFindings {
		bump(f.Severity)
	}

	switch worst {
	case analyze.SevCritical:
		return 2
	case analyze.SevWarn:
		return 1
	}
	return 0
}

// ParseFormat validates the --output flag.
func ParseFormat(s string) (string, error) {
	switch strings.ToLower(s) {
	case "table", "json", "markdown", "md":
		if s == "md" {
			return "markdown", nil
		}
		return strings.ToLower(s), nil
	}
	return "", fmt.Errorf("unknown output format %q (want table, json or markdown)", s)
}
