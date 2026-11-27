package main

import (
	"strings"
	"testing"
)

const good = `
apiVersion: v1
kind: Namespace
metadata:
  name: ns
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/enforce-version: v1.37
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/audit-version: v1.37
    pod-security.kubernetes.io/warn: restricted
    pod-security.kubernetes.io/warn-version: v1.37
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: default-deny, namespace: ns}
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: ns}
spec:
  template:
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: app
          image: example/app:1@sha256:0000000000000000000000000000000000000000000000000000000000000000
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          resources:
            requests: {cpu: 10m, memory: 16Mi}
            limits: {memory: 32Mi}
`

func run(t *testing.T, manifests string) []string {
	t.Helper()
	objects, err := read(strings.NewReader(manifests))
	if err != nil {
		t.Fatal(err)
	}
	return lint(objects)
}

func TestLint_whatHoldsToTheRulesPasses(t *testing.T) {
	if b := run(t, good); len(b) != 0 {
		t.Fatalf("breaches: %v", b)
	}
}

// Each change breaks one rule, and the linter names it.
func TestLint_eachBreachIsNamed(t *testing.T) {
	for _, c := range []struct{ name, from, to, want string }{
		{"baseline only", "enforce: restricted", "enforce: baseline", "does not enforce the restricted"},
		{"unpinned version", "warn-version: v1.37", "warn-version: latest", "does not pin the version it warns"},
		{"no default deny", "policyTypes: [Ingress, Egress]", "policyTypes: [Ingress]", "has no policy that denies all traffic"},
		{"a tag, not a digest", "app:1@sha256:0000000000000000000000000000000000000000000000000000000000000000", "app:1", "is not pinned by digest"},
		{"root allowed", "runAsNonRoot: true", "runAsNonRoot: false", "does not set runAsNonRoot"},
		{"no seccomp", "{type: RuntimeDefault}", "{type: Unconfined}", "seccomp profile is not RuntimeDefault"},
		{"escalation", "allowPrivilegeEscalation: false", "allowPrivilegeEscalation: true", "allows privilege escalation"},
		{"capabilities kept", "{drop: [ALL]}", "{drop: [NET_RAW]}", "does not drop every capability"},
		{"writable root", "readOnlyRootFilesystem: true", "readOnlyRootFilesystem: false", "writes its root file system"},
		{"no memory limit", "limits: {memory: 32Mi}", "limits: {}", "has no memory limit"},
		{"no cpu request", "requests: {cpu: 10m, memory: 16Mi}", "requests: {memory: 16Mi}", "requests no cpu"},
		{"a token", "automountServiceAccountToken: false", "automountServiceAccountToken: true", "mounts a service account token"},
		{"host network", "automountServiceAccountToken: false", "automountServiceAccountToken: false\n      hostNetwork: true", "shares a host namespace"},
	} {
		if !strings.Contains(good, c.from) {
			t.Fatalf("%s: the fixture has no %q", c.name, c.from)
		}
		b := run(t, strings.Replace(good, c.from, c.to, 1))
		if len(b) != 1 || !strings.Contains(b[0], c.want) {
			t.Errorf("%s: %v, want one breach saying %q", c.name, b, c.want)
		}
	}
}

// A workload states why it needs what the rules otherwise forbid, in an annotation.
func TestLint_aStatedReasonAllowsAWritableRootAndAToken(t *testing.T) {
	m := strings.Replace(good, "readOnlyRootFilesystem: true", "readOnlyRootFilesystem: false", 1)
	m = strings.Replace(m, "automountServiceAccountToken: false", "automountServiceAccountToken: true", 1)
	m = strings.Replace(m, "  template:\n", "  template:\n    metadata:\n      annotations:\n        settled/writable-root: the client writes its lock file next to its binary\n        settled/service-account-token: reads its own pods\n", 1)
	if b := run(t, m); len(b) != 0 {
		t.Fatalf("breaches: %v", b)
	}
}
