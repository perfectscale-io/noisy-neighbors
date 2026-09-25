# The EKS lab

A private, hardened EKS cluster with Karpenter, running the same five tenant
scenarios as the kind lab on one shared node. Built to be thrown away.

```bash
make eks-up        # ~20 min: VPC, EKS, Karpenter, metrics-server, tenants
make eks-verify    # check the security posture against the live cluster
make eks-scan      # run noisy-neighbors against it
make eks-down      # ~15 min, leaves only a KMS key pending deletion
```

## What gets built

```
  your laptop ── TLS, pinned to your /32 ──┐
                                           ▼
  ┌──────────────────────── VPC 10.42.0.0/16 ─────────────────────────┐
  │                                               EKS control plane   │
  │  public subnets ×3       private subnets ×3                       │
  │  ┌─────────┐             ┌──────────────────────────────────┐     │
  │  │   NAT   │◄── egress ──│ managed "system" ×2 (m5.large)   │     │
  │  └─────────┘             │ kube-system, Karpenter,          │     │
  │  (no nodes)              │ metrics-server                   │     │
  │                          ├──────────────────────────────────┤     │
  │                          │ "tenants" pool ×1 (m5.xlarge)    │     │
  │                          │ all five tenants                 │     │
  │                          └──────────────────────────────────┘     │
  └───────────────────────────────────────────────────────────────────┘
```

The tenant pool's CPU limit equals one node's vCPUs, so Karpenter can never
launch a second node and every tenant shares the first.

## Layout

| Path | What it is |
|---|---|
| `env.sh` | Every tunable. Everything else derives from it. |
| `cluster.yaml.tmpl` | eksctl config (`make eks-config` renders it) |
| `karpenter/nodepools.yaml.tmpl` | EC2NodeClass and node pools (`make eks-nodepools`) |
| `charts/noisy-tenants/` | Helm chart for the five tenants (`make eks-tenants`) |
| `values/` | Karpenter and metrics-server Helm values |
| `perfectscale/` | PerfectScale automation CRs (`make eks-ps-apply`) |
| `scripts/up.sh`, `down.sh` | Idempotent spin-up and ordered teardown |
| `scripts/verify.sh` | Security posture checks |
| `scripts/reauth.sh` | Re-pin the API endpoint to your current IP |
| `scripts/perfectscale.sh` | PerfectScale before/after (`make eks-ps-before`, `eks-ps-after`) |

## Security posture

`make eks-verify` checks each of these against the running cluster.

| Control | How |
|---|---|
| No worker reachable from the internet | Private subnets only, NAT egress, no public IPs |
| API server not world-open | Public endpoint pinned to your `/32`; private access on |
| No `aws-auth` ConfigMap | `authenticationMode: API` with EKS access entries |
| Secrets encrypted at rest | Customer-managed KMS key, rotation on |
| Audit trail | All control plane logs to CloudWatch, 7-day retention |
| No IMDS from pods | IMDSv1 off, `disablePodIMDS`, hop limit 1 on Karpenter nodes |
| Encrypted volumes | gp3, encrypted, on every node |
| Least-privilege Karpenter | EKS Pod Identity, scoped policies, one service account |
| No privileged tenants | Restricted Pod Security Standard enforced in every tenant namespace |
| Tenant isolation | Default-deny ingress, enforced by the VPC CNI |
| No SSH | No key pairs; use SSM |

Two deliberate lab trade-offs: a **single NAT gateway** (`NAT_MODE=HighlyAvailable`
for one per AZ), and a **public endpoint locked to one address** rather than a
fully private one, so `kubectl` works from a laptop without a VPN.

`up.sh` adds `--kubelet-insecure-tls` to metrics-server only if it can't
serve without it, and says so when it does.

## When kubectl stops answering

- **Credentials expired:** check `aws sts get-caller-identity` first.
- **Your IP changed:** the pinned endpoint drops packets, so `kubectl` hangs
  rather than failing. Re-pin it:

```bash
make eks-reauth
```

It refuses `0.0.0.0/0` or anything wider than `/24`, honors `ADMIN_CIDR`, and
waits for EKS to apply the change.

## Teardown and repeatability

`down.sh` removes the tenants, then deletes the node pools and **waits for
every Karpenter node to terminate** before deleting the cluster. Skipping that
wait orphans instances whose ENIs block the VPC from being deleted.

Both scripts are idempotent: an interrupted run resumes. The KMS key can't be
deleted immediately, so `down.sh` schedules it and the next `up.sh` restores
and reuses it. Everything is named from `CLUSTER_NAME`, so two labs can run
side by side:

```bash
make eks-up CLUSTER_NAME=nn-eu AWS_DEFAULT_REGION=eu-west-1 VPC_CIDR=10.43.0.0/16
```

## Cost

About **$0.55/hour** in `us-east-1`, roughly $400 a month if left running.

| | |
|---|---|
| EKS control plane | $0.10/hr |
| 2 × m5.large system nodes | $0.19/hr |
| 1 × m5.xlarge tenant node | $0.19/hr |
| NAT gateway | $0.045/hr + data |
| EBS gp3 | ~$0.01/hr |
