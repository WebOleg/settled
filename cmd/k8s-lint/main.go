// Command k8s-lint checks the devnet's Kubernetes manifests, as kustomize builds them, for
// the rules this repository holds itself to beyond what the API schema says (kubeconform
// checks that): every namespace enforces the restricted Pod Security Standard and starts
// from a policy that denies all traffic; every workload runs as the restricted standard
// asks and more, with its image pinned by digest, a read-only root file system, no
// service account token unless it says why, and resources requested and limited. It reads
// the manifests on stdin and prints each breach; it fails if there is one.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Annotations by which a workload states why it needs what the rules otherwise forbid.
const (
	writableRoot = "settled/writable-root"
	tokenReason  = "settled/service-account-token"
)

// object is the part of a manifest the rules read.
type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   meta   `yaml:"metadata"`
	Spec       yaml.Node
}

type meta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type networkPolicy struct {
	PodSelector struct {
		MatchLabels      map[string]string `yaml:"matchLabels"`
		MatchExpressions []any             `yaml:"matchExpressions"`
	} `yaml:"podSelector"`
	PolicyTypes []string `yaml:"policyTypes"`
	Ingress     []any    `yaml:"ingress"`
	Egress      []any    `yaml:"egress"`
}

type podTemplate struct {
	Metadata meta    `yaml:"metadata"`
	Spec     podSpec `yaml:"spec"`
}

type podSpec struct {
	AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
	HostNetwork                  bool  `yaml:"hostNetwork"`
	HostPID                      bool  `yaml:"hostPID"`
	HostIPC                      bool  `yaml:"hostIPC"`
	SecurityContext              struct {
		RunAsNonRoot   *bool `yaml:"runAsNonRoot"`
		SeccompProfile struct {
			Type string `yaml:"type"`
		} `yaml:"seccompProfile"`
	} `yaml:"securityContext"`
	Volumes []struct {
		Name     string `yaml:"name"`
		HostPath any    `yaml:"hostPath"`
	} `yaml:"volumes"`
	InitContainers []container `yaml:"initContainers"`
	Containers     []container `yaml:"containers"`
}

type container struct {
	Name            string `yaml:"name"`
	Image           string `yaml:"image"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   *bool `yaml:"readOnlyRootFilesystem"`
		Privileged               bool  `yaml:"privileged"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
			Add  []string `yaml:"add"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
	Resources struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	} `yaml:"resources"`
}

func main() {
	objects, err := read(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "k8s-lint: %v\n", err)
		os.Exit(1)
	}
	breaches := lint(objects)
	for _, b := range breaches {
		fmt.Println(b)
	}
	if len(breaches) > 0 {
		fmt.Fprintf(os.Stderr, "k8s-lint: %d breaches in %d objects\n", len(breaches), len(objects))
		os.Exit(1)
	}
	fmt.Printf("k8s-lint: ok, %d objects\n", len(objects))
}

func read(r io.Reader) ([]object, error) {
	dec := yaml.NewDecoder(bufio.NewReader(r))
	var out []object
	for {
		var o object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if o.Kind != "" {
			out = append(out, o)
		}
	}
}

// lint returns each breach as "kind namespace/name: what".
func lint(objects []object) []string {
	var breaches []string
	add := func(o object, format string, args ...any) {
		breaches = append(breaches, fmt.Sprintf("%s %s/%s: %s", o.Kind, o.Metadata.Namespace, o.Metadata.Name, fmt.Sprintf(format, args...)))
	}
	namespaces := map[string]object{}
	denied := map[string]bool{}
	for _, o := range objects {
		switch o.Kind {
		case "Namespace":
			namespaces[o.Metadata.Name] = o
			l := o.Metadata.Labels
			for _, mode := range []string{"enforce", "audit", "warn"} {
				if l["pod-security.kubernetes.io/"+mode] != "restricted" {
					add(o, "does not %s the restricted Pod Security Standard", mode)
				}
				if l["pod-security.kubernetes.io/"+mode+"-version"] == "" || l["pod-security.kubernetes.io/"+mode+"-version"] == "latest" {
					add(o, "does not pin the version it %ss", mode)
				}
			}
		case "NetworkPolicy":
			var p networkPolicy
			if err := o.Spec.Decode(&p); err != nil {
				add(o, "unreadable: %v", err)
				continue
			}
			if len(p.PodSelector.MatchLabels) == 0 && len(p.PodSelector.MatchExpressions) == 0 &&
				has(p.PolicyTypes, "Ingress") && has(p.PolicyTypes, "Egress") && len(p.Ingress) == 0 && len(p.Egress) == 0 {
				denied[o.Metadata.Namespace] = true
			}
		default:
			if t, ok, err := template(o); err != nil {
				add(o, "unreadable: %v", err)
			} else if ok {
				for _, b := range workload(t) {
					add(o, "%s", b)
				}
			}
		}
	}
	names := make([]string, 0, len(namespaces))
	for n := range namespaces {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !denied[n] {
			add(namespaces[n], "has no policy that denies all traffic in and out")
		}
	}
	return breaches
}

// template finds the pod template of a workload.
func template(o object) (podTemplate, bool, error) {
	var t podTemplate
	switch o.Kind {
	case "Pod":
		var s podSpec
		err := o.Spec.Decode(&s)
		return podTemplate{Metadata: o.Metadata, Spec: s}, true, err
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job":
		var s struct {
			Template podTemplate `yaml:"template"`
		}
		err := o.Spec.Decode(&s)
		t = s.Template
		if t.Metadata.Annotations == nil {
			t.Metadata.Annotations = o.Metadata.Annotations
		}
		return t, true, err
	case "CronJob":
		var s struct {
			JobTemplate struct {
				Spec struct {
					Template podTemplate `yaml:"template"`
				} `yaml:"spec"`
			} `yaml:"jobTemplate"`
		}
		err := o.Spec.Decode(&s)
		return s.JobTemplate.Spec.Template, true, err
	}
	return t, false, nil
}

func workload(t podTemplate) []string {
	var b []string
	s := t.Spec
	if s.SecurityContext.RunAsNonRoot == nil || !*s.SecurityContext.RunAsNonRoot {
		b = append(b, "the pod does not set runAsNonRoot")
	}
	if s.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		b = append(b, "the pod's seccomp profile is not RuntimeDefault")
	}
	if s.HostNetwork || s.HostPID || s.HostIPC {
		b = append(b, "the pod shares a host namespace")
	}
	for _, v := range s.Volumes {
		if v.HostPath != nil {
			b = append(b, fmt.Sprintf("volume %s is a host path", v.Name))
		}
	}
	if (s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken) && strings.TrimSpace(t.Metadata.Annotations[tokenReason]) == "" {
		b = append(b, "the pod mounts a service account token and does not say why ("+tokenReason+")")
	}
	for _, c := range append(append([]container{}, s.InitContainers...), s.Containers...) {
		if !strings.Contains(c.Image, "@sha256:") {
			b = append(b, fmt.Sprintf("container %s: image %s is not pinned by digest", c.Name, c.Image))
		}
		sc := c.SecurityContext
		if sc.Privileged {
			b = append(b, fmt.Sprintf("container %s is privileged", c.Name))
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			b = append(b, fmt.Sprintf("container %s allows privilege escalation", c.Name))
		}
		if !has(sc.Capabilities.Drop, "ALL") {
			b = append(b, fmt.Sprintf("container %s does not drop every capability", c.Name))
		}
		if len(sc.Capabilities.Add) > 0 {
			b = append(b, fmt.Sprintf("container %s adds capabilities %v", c.Name, sc.Capabilities.Add))
		}
		if (sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem) && strings.TrimSpace(t.Metadata.Annotations[writableRoot]) == "" {
			b = append(b, fmt.Sprintf("container %s writes its root file system and does not say why (%s)", c.Name, writableRoot))
		}
		for _, r := range []string{"cpu", "memory"} {
			if c.Resources.Requests[r] == "" {
				b = append(b, fmt.Sprintf("container %s requests no %s", c.Name, r))
			}
		}
		if c.Resources.Limits["memory"] == "" {
			b = append(b, fmt.Sprintf("container %s has no memory limit", c.Name))
		}
	}
	return b
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
