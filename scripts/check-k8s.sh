#!/usr/bin/env bash
# Checks the devnet's Kubernetes manifests without a cluster: each kustomization under
# infra/k8s builds; every object is valid for the cluster's Kubernetes version, strictly
# (no unknown fields), against schemas pinned to a commit (kubeconform); and every object
# holds to the repository's own rules (cmd/k8s-lint): restricted Pod Security on every
# namespace, all traffic denied by default, workloads pinned, unprivileged and bounded.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/k8s.sh
. scripts/lib/k8s.sh
k8s_tools

built="$(mktemp)"
trap 'rm -f "$built"' EXIT
n=0
while IFS= read -r k; do
  dir="$(dirname "$k")"
  "$K8S_BIN/kustomize" build "$dir" > "$built"
  "$K8S_BIN/kubeconform" -strict -summary -kubernetes-version "$K8S_VERSION" -schema-location "$K8S_SCHEMAS" "$built" \
    | sed "s#^#$dir: #"
  go run ./cmd/k8s-lint < "$built" | sed "s#^#$dir: #"
  n=$((n + 1))
done < <(git ls-files 'infra/k8s/*kustomization.yaml')
[ "$n" -gt 0 ] || { echo "check-k8s: no kustomization under infra/k8s"; exit 1; }
echo "check-k8s: ok, $n kustomizations"
