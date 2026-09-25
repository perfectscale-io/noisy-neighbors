# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub instead: the **Security** tab of this
repository, then **Report a vulnerability**. Only the maintainers can see
the report. We aim to acknowledge it within three business days.

## What is in scope

- The `kubectl noisy-neighbors` plugin. It is read-only by design; anything
  that makes it write to a cluster, or read more than
  [docs/rbac.yaml](docs/rbac.yaml) grants, is a vulnerability.
- The `noisy-wall` server. It binds to loopback by default and refuses
  cross-origin writes; a way around either is a vulnerability.
- The EKS lab under `deploy/eks`. A default that leaves the cluster more
  exposed than [its README](deploy/eks/README.md#security-posture) says is
  a vulnerability.

## What is not

- The lab tenants misbehave on purpose. An OOMKill or CPU starvation in
  `examples/lab` or `deploy/eks/charts/noisy-tenants` is the lab working.
- Running `noisy-wall -listen 0.0.0.0:8099 -allow-button` exposes a button
  that writes to your cluster as you. The server warns when you do this.

## Supported versions

Only the latest commit on `main` is supported.
