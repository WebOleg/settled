#!/usr/bin/env bash
# Starts the devnet's local Kubernetes cluster (infra/k8s/kind.yaml) if it is not there,
# checks it runs the pinned node image and Kubernetes version, and applies what every
# namespace starts with (infra/k8s/base). Run again, it changes nothing that is in place.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/k8s.sh
. scripts/lib/k8s.sh
k8s_tools

image="$(sed -n 's/^ *image: *\(kindest\/node:[^ ]*@sha256:[0-9a-f]\{64\}\) *$/\1/p' infra/k8s/kind.yaml)"
[ -n "$image" ] || { echo "k8s-up: no pinned node image in infra/k8s/kind.yaml"; exit 1; }
clusters="$("$K8S_BIN/kind" get clusters 2>/dev/null)"
if grep -qx "$K8S_CLUSTER" <<<"$clusters"; then
  echo "k8s-up: cluster $K8S_CLUSTER is there"
else
  config="$(mktemp)"
  trap 'rm -f "$config"' EXIT
  sed "s#@REPO@#$K8S_ROOT#" infra/k8s/kind.yaml > "$config"
  "$K8S_BIN/kind" create cluster --config "$config" --wait 180s
fi
# The node runs the image the file pins, and the API server the version it holds.
node="$(docker inspect --format '{{.Config.Image}}' "$K8S_CLUSTER-control-plane")"
[ "$node" = "$image" ] || { echo "k8s-up: the node runs $node, not $image; make k8s-down and make k8s-up"; exit 1; }
server="$(kc version -o json | jq -r '.serverVersion.gitVersion')"
[ "$server" = "v$K8S_VERSION" ] || { echo "k8s-up: the API server is $server, not v$K8S_VERSION"; exit 1; }
"$K8S_BIN/kustomize" build infra/k8s/base | kc apply -f - >/dev/null
echo "k8s-up: ok, Kubernetes $server on $(kc get nodes --no-headers | wc -l | tr -d ' ') node; namespaces: $(kc get ns -l pod-security.kubernetes.io/enforce=restricted -o name | sed 's#namespace/##' | tr '\n' ' ')"
