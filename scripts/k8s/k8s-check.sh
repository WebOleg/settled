#!/usr/bin/env bash
# Shows that the devnet's cluster enforces its rules, by trying to break them:
# - Pod Security: in each of the devnet's namespaces, and in a new namespace with no
#   label of its own (the cluster's default), a privileged pod and a pod that may run as
#   root are refused by the API server; a pod that holds to the restricted standard is
#   admitted.
# - Network policies: a server and a client in the bridge's namespace cannot reach each
#   other; with a policy that names both ends and the port, they can, by the server's
#   name, so DNS works too; a client in the L1's namespace still cannot.
# The probes run the PostgreSQL image the ledger runs, as its unprivileged user, and are
# removed at the end, whatever happens.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/k8s.sh
. scripts/lib/k8s.sh
fail() { echo "k8s-check: FAIL: $*"; exit 1; }
image="$(sed -n 's/^ *image: *\(postgres:[^ ]*@sha256:[0-9a-f]\{64\}\) *$/\1/p' infra/bridge/compose.yaml)"
[ -n "$image" ] || fail "no pinned PostgreSQL image in infra/bridge/compose.yaml"
probe_ns="settled-psa-probe"
cleanup() {
  kc -n settled-bridge delete pod,service,networkpolicy -l settled/probe=k8s-check --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kc -n settled-l1 delete pod -l settled/probe=k8s-check --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kc delete namespace "$probe_ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

server="$(kc version -o json | jq -r '.serverVersion.gitVersion')"
echo "cluster          $K8S_CONTEXT, Kubernetes $server, $(kc get nodes --no-headers | awk '{print $2}' | sort | uniq -c | tr -s ' ' | sed 's/^ //') node(s)"

# pod NAME LABEL SECURITY: a pod manifest of the probe image. SECURITY is restricted,
# privileged or root.
pod() {
  local name="$1" role="$2" kind="$3" psc csc
  psc='runAsNonRoot: true, runAsUser: 70, runAsGroup: 70, fsGroup: 70, seccompProfile: {type: RuntimeDefault}'
  csc='allowPrivilegeEscalation: false, readOnlyRootFilesystem: false, capabilities: {drop: [ALL]}'
  case "$kind" in
    privileged) csc='privileged: true' ;;
    root) psc='seccompProfile: {type: RuntimeDefault}'; csc='allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}' ;;
  esac
  cat <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels: {settled/probe: k8s-check, settled/role: $role}
spec:
  automountServiceAccountToken: false
  securityContext: {$psc}
  containers:
    - name: probe
      image: $image
      securityContext: {$csc}
      resources: {requests: {cpu: 10m, memory: 64Mi}, limits: {memory: 256Mi}}
      env:
        - {name: POSTGRES_HOST_AUTH_METHOD, value: trust}
        - {name: PGDATA, value: /data/pg}
      readinessProbe:
        exec: {command: [pg_isready, -q, -h, 127.0.0.1]}
        periodSeconds: 2
      volumeMounts: [{name: data, mountPath: /data}]
  volumes: [{name: data, emptyDir: {}}]
YAML
}

echo
echo "== Pod Security =="
kc create namespace "$probe_ns" >/dev/null
for ns in settled-l1 settled-l2 settled-bridge settled-obs "$probe_ns"; do
  for kind in privileged root; do
    if out="$(pod "psa-$kind" probe "$kind" | kc -n "$ns" apply --dry-run=server -f - 2>&1)"; then
      fail "$ns admitted a $kind pod"
    fi
    case "$out" in *'violates PodSecurity "restricted'*) ;; *) fail "$ns refused a $kind pod, but not under Pod Security: $out" ;; esac
  done
  pod psa-ok probe restricted | kc -n "$ns" apply --dry-run=server -f - >/dev/null 2>&1 || fail "$ns refused a pod that holds to the restricted standard"
  label="$( [ "$ns" = "$probe_ns" ] && echo "no label, the cluster's default" || echo "its label" )"
  echo "pod security     $ns ($label): privileged and root refused, restricted admitted"
done

echo
echo "== Network policies =="
pod np-server server restricted | kc -n settled-bridge apply -f - >/dev/null
kc -n settled-bridge apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Service
metadata: {name: np-server, labels: {settled/probe: k8s-check}}
spec:
  selector: {settled/probe: k8s-check, settled/role: server}
  ports: [{port: 5432, targetPort: 5432}]
YAML
pod np-client client restricted | kc -n settled-bridge apply -f - >/dev/null
pod np-client client restricted | kc -n settled-l1 apply -f - >/dev/null
for ns in settled-bridge settled-l1; do
  kc -n "$ns" wait --for=condition=Ready pod -l settled/probe=k8s-check --timeout=180s >/dev/null || fail "the probes in $ns are not ready"
done
reach() { kc -n "$1" exec np-client -- pg_isready -q -h "$2" -p 5432 -t 3 >/dev/null 2>&1; }
ip="$(kc -n settled-bridge get pod np-server -o jsonpath='{.status.podIP}')"
name="np-server.settled-bridge.svc.cluster.local"
if reach settled-bridge "$ip"; then fail "the client reached the server with no policy allowing it"; fi
echo "network          settled-bridge: client to server $ip:5432 refused by the default"
kc -n settled-bridge apply -f - >/dev/null <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: np-client-to-server, labels: {settled/probe: k8s-check}}
spec:
  podSelector: {matchLabels: {settled/probe: k8s-check, settled/role: client}}
  policyTypes: [Egress]
  egress:
    - to: [{podSelector: {matchLabels: {settled/probe: k8s-check, settled/role: server}}}]
      ports: [{protocol: TCP, port: 5432}]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: np-server-from-client, labels: {settled/probe: k8s-check}}
spec:
  podSelector: {matchLabels: {settled/probe: k8s-check, settled/role: server}}
  policyTypes: [Ingress]
  ingress:
    - from: [{podSelector: {matchLabels: {settled/probe: k8s-check, settled/role: client}}}]
      ports: [{protocol: TCP, port: 5432}]
YAML
ok=0
for _ in $(seq 1 15); do
  if reach settled-bridge "$name"; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || fail "the client could not reach $name with a policy allowing it"
echo "network          settled-bridge: client to $name allowed by the policy naming both ends"
if reach settled-l1 "$ip"; then fail "a client in settled-l1 reached the bridge's server"; fi
if reach settled-l1 "$name"; then fail "a client in settled-l1 reached the bridge's server by name"; fi
echo "network          settled-l1: client to the bridge's server still refused"
echo "k8s-check: ok"
