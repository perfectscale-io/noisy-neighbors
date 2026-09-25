# Contributing

Issues and pull requests are welcome.

## Before you open a pull request

```bash
make test        # go vet and go test
make scan        # gitleaks over the history, govulncheck over the code
```

CI runs the same checks, plus `gofmt`. A pull request needs one approving
review from a code owner and green CI before it merges.

## Ground rules

- **The plugin stays read-only.** It is meant to be safe to point at a
  production cluster. A change that needs a new verb or resource in
  [docs/rbac.yaml](docs/rbac.yaml) needs a very good reason in the PR.
- **Never commit anything from a real cluster.** No kubeconfigs, account
  IDs, node names, IP addresses or recordings (`*.jsonl`). The `.gitignore`
  covers the usual places; check `git diff --staged` anyway.
- **No secrets, ever.** If you commit one by mistake, treat it as leaked:
  revoke it first, then clean up. Removing it from the history does not
  un-leak it.

## Reporting a security problem

See [SECURITY.md](SECURITY.md). Please don't use a public issue.
