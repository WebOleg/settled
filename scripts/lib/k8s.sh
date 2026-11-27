#!/usr/bin/env bash
# Sourced by the Kubernetes scripts. k8s_tools builds kind, kustomize and kubeconform at
# the versions tools/go.mod pins, with their sums checked by Go against tools/go.sum,
# into .devnet/bin, which is not tracked; the same versions on every machine and in CI.
# kc is kubectl on the devnet's cluster only, never another context.

K8S_ROOT="$(git rev-parse --show-toplevel)"
K8S_CLUSTER="settled"
K8S_CONTEXT="kind-$K8S_CLUSTER"
K8S_BIN="$K8S_ROOT/.devnet/bin"
# The Kubernetes version of the node image in infra/k8s/kind.yaml, for kubeconform.
K8S_VERSION="1.37.0"
# The schemas kubeconform checks against, pinned to a commit of their repository.
K8S_SCHEMAS="https://raw.githubusercontent.com/yannh/kubernetes-json-schema/e62c03bd5a52fd34ff6ac28d083e2a300e0ae7c1/{{.NormalizedKubernetesVersion}}-standalone{{.StrictSuffix}}/{{.ResourceKind}}{{.KindSuffix}}.json"
export K8S_ROOT K8S_CLUSTER K8S_CONTEXT K8S_BIN K8S_VERSION K8S_SCHEMAS

k8s_tools() {
  mkdir -p "$K8S_BIN"
  go build -C "$K8S_ROOT/tools" -o "$K8S_BIN/" \
    sigs.k8s.io/kind github.com/yannh/kubeconform/cmd/kubeconform sigs.k8s.io/kustomize/kustomize/v5
}

kc() { kubectl --context "$K8S_CONTEXT" "$@"; }
