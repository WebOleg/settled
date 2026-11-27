#!/usr/bin/env bash
# Deletes the devnet's local Kubernetes cluster, and everything in it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/k8s.sh
. scripts/lib/k8s.sh
k8s_tools
"$K8S_BIN/kind" delete cluster --name "$K8S_CLUSTER"
