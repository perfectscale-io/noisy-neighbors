# noisy-neighbors

A `kubectl` plugin for shared Kubernetes clusters. It names the tenants using
more than they asked for, and the tenants on the same node exposed to them.

It uses data every cluster already has. No Prometheus, no agent, no CRDs: one
read-only binary.

## What it looks like

From the bundled lab (`make lab-up && make demo`):

```
BLAST RADIUS
──────────────────────────────────────────────────────────────────────────────

CRITICAL noisy-lab-worker  5 tenants
  cpu requested    32%   limits    56%
  mem requested    29%   limits    54%
  actual    cpu    26%   mem    11%
    aggressor team-payments/ingest-89f984698-4z2xb             memory req 64.0Mi, using 701.9Mi (1097% of request)
    aggressor team-payments/ingest-89f984698-4z2xb             cpu req 50m, using 999m (1998% of request)
    exposed   team-billing/worker-6bcd4ff9c9-bfh6x:worker      CPU throttled in 100% of periods over the sample window
    exposed   team-search/api-66c94864f4-t9s6d                 OOMKilled 2m ago (4 restarts); shares this node with 4 other tenants

TENANTS
──────────────────────────────────────────────────────────────────────────────
  NAMESPACE           PODS  NODES  REQ CPU  REQ MEM  USED CPU  USED MEM  STRANDED MEM  OOM  THROTTLED
  team-analytics      2     1      1000m    2.0Gi    0m        692.0Ki   2.0Gi         0    0
  team-search         1     1      100m     128.0Mi  0m        344.0Ki   127.7Mi       1    0
  team-billing        1     1      50m      32.0Mi   50m       332.0Ki   31.7Mi        0    1
  team-payments       1     1      50m      64.0Mi   999m      701.9Mi   -             0    0
  team-scratch        1     1      0m       0B       1m        344.0Ki   -             0    0
```

`team-payments` asked for 64Mi and uses 702Mi, so the scheduler packed four
other tenants around a number that isn't true. `team-billing` is throttled in
every CFS period while reporting `Running` with zero restarts.

| Tool | View |
|---|---|
| KRR, Goldilocks | per-workload right-sizing |
| kube-capacity | per-node requests vs limits vs utilization |
| OpenCost | per-namespace cost |
| **noisy-neighbors** | **per-node aggressor → exposed-tenant relationships** |

## Install

```bash
git clone https://github.com/perfectscale-io/noisy-neighbors
cd noisy-neighbors
make install        # builds into ~/.local/bin
kubectl noisy-neighbors
```

## Usage

```bash
kubectl noisy-neighbors                            # full scan, 30s sample window
kubectl noisy-neighbors --no-kubelet               # skip the privileged reads
kubectl noisy-neighbors --sample-window 0          # fastest; no throttling data
kubectl noisy-neighbors --namespaces team-a,team-b # scope the scan
kubectl noisy-neighbors --wide                     # every node, plus co-tenant lists
kubectl noisy-neighbors -o markdown > report.md    # shareable report
kubectl noisy-neighbors -o json                    # machine-readable
kubectl noisy-neighbors --exit-code                # 1 on warnings, 2 on critical
```

Thresholds are flags: `--aggressor-ratio`, `--strand-ratio`,
`--saturation-pct`, `--throttle-warn-pct`, `--oom-window`.

## Data sources

Only tier 0 is required. The report lists every source it used and anything
it couldn't measure, and never fills a gap with an estimate.

| Tier | Source | Needs | Adds |
|---|---|---|---|
| 0 | kube-apiserver | read-only ClusterRole | co-tenancy, overcommit, OOMKills, QoS, quotas, scheduling events |
| 1 | metrics.k8s.io | metrics-server | usage vs requests: aggressors, stranded capacity, real saturation |
| 2 | kubelet via `nodes/proxy` | `get` on `nodes/proxy` | CFS throttling, OOM counters, per-pod network (`--network`) |

Throttling is a counter, so one read only says what happened since the
container started. The tool takes two samples `--sample-window` apart and
reports the rate between them, which is why a full run takes 30 seconds.

## Permissions

```bash
kubectl apply -f docs/rbac.yaml
```

`noisy-neighbors` covers tiers 0 and 1. `noisy-neighbors-kubelet` adds
`nodes/proxy`. The tool only GETs `/metrics/resource`, `/metrics/cadvisor`
and `/stats/summary`, but the permission allows more; use `--no-kubelet` if
you'd rather not grant it.

## Labs

Five tenants, each broken in a different way, on one shared node:

| Tenant | Role | Setup |
|---|---|---|
| `team-payments` | aggressor | requests 64Mi, no limit, uses ~700Mi and a full core |
| `team-search` | OOM victim | 128Mi limit, needs 160Mi |
| `team-billing` | throttled | 50m CPU limit on a busy loop |
| `team-analytics` | over-requester | 2 × 1Gi requested, uses almost nothing |
| `team-scratch` | BestEffort | no requests or limits |

```bash
make lab-up && make demo    # kind, local; see examples/lab
make eks-up && make eks-scan    # private, hardened EKS with Karpenter; see deploy/eks
```

## The wall

A live view for showing a room: each tenant's request (dashed ring) against
its usage (solid disc), drawn by area.

![The wall](webinar/screenshots/02-bubbles-before.png)

```bash
make wall-synth     # scripted run, no cluster needed
make wall           # live, against the current kube context
make wall-record    # live, saving every frame to golden-run.jsonl
make wall-replay    # play a recording back as a fallback
```

Open http://localhost:8099. Keys: `B`/`R` bring the aggressor in and out,
`M`/`C` switch memory and CPU, `L` shows bars, `D` the full console, `T` the
tenant's own dashboard.

The wall uses the analyzer's roles, so it never disagrees with the scan. It
listens on `127.0.0.1` and refuses cross-origin writes. `-allow-button`
(on in `make wall`) lets `B` scale the lab aggressor; it's the only write in
this repo.

## Right-sizing with PerfectScale

The tool stops at a diagnosis. On the EKS lab,
[`deploy/eks/perfectscale`](deploy/eks/perfectscale) configures
[PerfectScale](https://www.perfectscale.io) automation as CRs, with requests
and limits allowed to go up as well as down, and a timed before/after:

```bash
make eks-ps-apply    # apply the CRs (PerfectScale agent must be installed)
make eks-ps-before   # automation off, tenants back on their original spec
make eks-ps-after    # automation on; prints how long each tenant took
make eks-ps-status   # original spec vs running pod
```

## What it doesn't do

- **Remediate.** It ends at a diagnosis.
- **Prove causation.** It's a snapshot. Findings say "exposed to", never
  "killed by".
- **Keep history.** Every number is now, or a rate over the sample window.

## More

- [`webinar/`](webinar): screens from *Operating Kubernetes multitenancy*
- [SECURITY.md](SECURITY.md): reporting a vulnerability
- [CONTRIBUTING.md](CONTRIBUTING.md)

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
Copyright 2026 DoiT International.
