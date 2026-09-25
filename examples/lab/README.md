# The kind lab

A four-node kind cluster with five tenants that are broken on purpose, so the
tool's output can be checked against a known answer.

```bash
make lab-up       # kind cluster, metrics-server, tenants
sleep 60          # let them misbehave
make demo         # run the scan
make lab-down
```

## The tenants

All five pin themselves to `noisy-lab-worker`: co-tenancy is the subject.

| Namespace | Setup | Expected finding |
|---|---|---|
| `team-payments` | requests 64Mi / 50m, no limits, fills ~700Mi and spins a core | `aggressor-memory`, `aggressor-cpu` |
| `team-search` | 128Mi limit; after 25s it holds 160Mi | `exposed-oomkill`, restarts climbing |
| `team-billing` | 50m CPU limit on a busy loop | `exposed-throttling` (tier 2 only) |
| `team-analytics` | 2 × (1Gi / 500m) requested, uses almost nothing; a quota far above cluster capacity | stranded memory, `quota-not-binding` |
| `team-scratch` | no resources block | `exposed-besteffort` once the node saturates |

No stress image is needed: `busybox` and `dd` produce all of it. `make
lab-status` shows restarts and termination reasons.

## Seeing degradation

```bash
make demo-degraded
```

The same scan with tiers 1 and 2 switched off: what someone gets on a
locked-down cluster with no metrics-server.

## Caveats

**Node saturation won't fire on kind.** Every kind node reports the whole VM's
memory as its own, so 700Mi reads as 11% of a node. The cgroup-driven findings
(OOMKills, throttling, aggressors, stranded capacity) all work. To see
saturation, lower the threshold:

```bash
./bin/kubectl-noisy_neighbors --context kind-noisy-lab --saturation-pct 10
```

**On macOS**, the Docker VM's memory caps every scenario. If nothing is under
pressure, give the VM more memory (e.g. `colima start --cpu 4 --memory 8`).
