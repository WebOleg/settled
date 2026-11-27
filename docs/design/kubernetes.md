# Design: the devnet on Kubernetes

Stage 6 runs the devnet on a local Kubernetes cluster, the same system as the compose
files run, so CI can start it and run the end-to-end tests on every push. This document
describes what is in place; it grows as each part of the devnet moves.

## The cluster

The cluster is made by kind ([infra/k8s/kind.yaml](../../infra/k8s/kind.yaml)), which
runs Kubernetes nodes as Docker containers, the same way on a Mac and on a CI runner.
One node, its image pinned by digest: `kindest/node:v1.37.0`, the image kind v0.33.0
was released with. The API server listens on 127.0.0.1 only.

kind, kustomize and kubeconform are pinned in [tools/go.mod](../../tools/go.mod), their
sums checked by Go against `tools/go.sum`, and built into `.devnet/bin`
([scripts/lib/k8s.sh](../../scripts/lib/k8s.sh)): the same versions on every machine and
in CI, with nothing installed by hand. `kubectl` runs only with the devnet's context,
`kind-settled`.

## Every namespace starts closed

| Rule | How |
|---|---|
| Restricted Pod Security everywhere | The API server's admission configuration ([policies/admission.yaml](../../infra/k8s/policies/admission.yaml)) enforces the restricted Pod Security Standard on every namespace that does not say otherwise, as the Kubernetes tutorial on cluster-level Pod Security sets it up on kind; only `kube-system` and kind's storage provisioner are exempt. Each of the devnet's namespaces also labels itself to enforce, audit and warn on it, pinned to v1.37. Restricted means no root, no privilege escalation, every capability dropped, the runtime's seccomp profile, no host namespaces, ports or paths. |
| No traffic by default | Each namespace has a network policy that denies all traffic in and out, and one that allows DNS to the cluster's DNS only ([network-policies.yaml](../../infra/k8s/base/network-policies.yaml)). A workload that needs a connection gets a policy naming both ends and the port. kind enforces network policies with kube-network-policies. |
| One namespace per part | `settled-l1`, `settled-l2`, `settled-bridge`, `settled-obs`, as the compose projects are ([namespaces.yaml](../../infra/k8s/base/namespaces.yaml)). |

Workloads are held to more than the restricted standard asks, by
[cmd/k8s-lint](../../cmd/k8s-lint/main.go) in `make check`: every image pinned by
digest, a read-only root file system and no service account token unless the workload
states why in an annotation (`settled/writable-root`, `settled/service-account-token`),
CPU and memory requested and memory limited, no host path.

## Shown by

| Rule | Shown by |
|---|---|
| Each manifest is valid for Kubernetes 1.37, strictly, against schemas pinned to a commit | `make check-k8s` (kubeconform) |
| Each manifest holds to the rules above; each breach is named | `make check-k8s` ([cmd/k8s-lint](../../cmd/k8s-lint/main.go)), and its tests: one per rule |
| The cluster runs the pinned node image and Kubernetes version | `make k8s-up` |
| A privileged pod and a pod that may run as root are refused, in each namespace and in a new one with no label; a restricted pod is admitted | `make k8s-check` |
| Two pods in one namespace cannot reach each other until a policy names both ends and the port; then they can, by name; a pod in another namespace still cannot | `make k8s-check` |

## Known limits

| Limit | Why it is accepted here |
|---|---|
| One node. | The devnet's parts need no spreading across nodes to be shown; a production cluster has several, across zones, and the manifests do not assume one. |
| Secrets come from `.devnet` files. | On the devnet the scripts that make them already exist; a production cluster takes them from a secret manager (External Secrets, Vault), so none is in git or in a manifest. |
