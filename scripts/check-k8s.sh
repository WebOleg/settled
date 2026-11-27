#!/usr/bin/env bash
# Checks the devnet's Kubernetes manifests without a cluster: each kustomization under
# infra/k8s builds; every object is valid for the cluster's Kubernetes version, strictly
# (no unknown fields), against schemas pinned to a commit (kubeconform); and every object
# holds to the repository's own rules (cmd/k8s-lint): restricted Pod Security on every
# namespace, all traffic denied by default, workloads pinned, unprivileged and bounded.
# kubeconform must have checked as many objects as the build holds, none skipped: a
# check that finds nothing to check fails.
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
  lint="$(go run ./cmd/k8s-lint < "$built")" || { printf '%s\n' "$lint" | sed "s#^#$dir: #"; exit 1; }
  objects="$(printf '%s\n' "$lint" | sed -n 's/^k8s-lint: ok, \([0-9]*\) objects$/\1/p')"
  # Read from stdin: kubeconform skips a file whose name does not end in .yaml.
  if ! schema="$("$K8S_BIN/kubeconform" -strict -summary -output json -kubernetes-version "$K8S_VERSION" -schema-location "$K8S_SCHEMAS" < "$built")"; then
    "$K8S_BIN/kubeconform" -strict -kubernetes-version "$K8S_VERSION" -schema-location "$K8S_SCHEMAS" < "$built" | sed "s#^#$dir: #"
    exit 1
  fi
  summary="$(printf '%s' "$schema" | jq -r '.summary | "\(.valid) \(.invalid) \(.errors) \(.skipped)"')"
  [ "$summary" = "$objects 0 0 0" ] || { echo "$dir: kubeconform checked valid, invalid, errors, skipped: $summary; the build holds $objects objects"; exit 1; }
  echo "$dir: $objects objects valid for Kubernetes $K8S_VERSION, none skipped; each holds to the repository's rules"
  n=$((n + 1))
done < <(git ls-files 'infra/k8s/*kustomization.yaml')
[ "$n" -gt 0 ] || { echo "check-k8s: no kustomization under infra/k8s"; exit 1; }
echo "check-k8s: ok, $n kustomizations"
