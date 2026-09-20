package resolve

import "github.com/kamronarabi/structura/pkg/schema"

// Alias is a name that routes to a component rather than being one.
//
// A Kubernetes Service is the motivating case. It is not a component in any
// architectural sense — it is a stable DNS name and a label selector pointing
// at whatever workload happens to match. Modeling it as its own node would
// double the node count and turn every dependency into a two-hop path:
// "checkout calls postgres-svc which exposes postgres" rather than "checkout
// persists to postgres". What a reader wants, and what a model should answer
// with, is the second one.
//
// So a Service contributes identities to an existing node instead of adding
// one. The extractor cannot do the attachment itself — the workload is
// usually declared in a different file, and matching a selector against pod
// labels needs the whole node set — so it emits this and the resolver joins
// it up.
type Alias struct {
	// Name is the alias itself, as other components would write it.
	Name      string
	Namespace string

	// DNS lists every form the alias is reachable by, most qualified first.
	DNS []string

	// Ports are the ports the alias exposes.
	Ports []int

	// Selector is the label set an object must carry to be the target.
	// Empty means the alias names its target directly; see TargetName.
	Selector map[string]string

	// TargetName names the target outright, for the cases where a selector
	// is not involved: a StatefulSet's governing service, or an
	// ExternalName Service.
	TargetName string

	// External is set when the alias resolves outside the cluster, as with
	// an ExternalName Service pointing at a managed database.
	External string

	Source schema.Evidence
}

// Matches reports whether a set of labels satisfies the alias's selector.
//
// Kubernetes selector semantics: every key in the selector must be present
// with the same value, and extra labels on the target are irrelevant. An
// empty selector deliberately matches nothing here — in Kubernetes it would
// match every pod in the namespace, which as an architecture edge would mean
// connecting one Service to everything.
func (a Alias) Matches(labels map[string]string) bool {
	if len(a.Selector) == 0 || len(labels) == 0 {
		return false
	}
	for k, want := range a.Selector {
		if got, ok := labels[k]; !ok || got != want {
			return false
		}
	}
	return true
}

// DNSNames builds the forms a Kubernetes service name is reachable by, from
// most qualified to least. All of them are emitted because which one a
// repository writes down varies by file: a manifest in the same namespace
// uses the bare name, one crossing namespaces uses "name.namespace", and
// anything generated tends to use the fully qualified form.
func DNSNames(name, namespace string) []string {
	if name == "" {
		return nil
	}
	if namespace == "" {
		namespace = "default"
	}
	return []string{
		name + "." + namespace + ".svc.cluster.local",
		name + "." + namespace + ".svc",
		name + "." + namespace,
		name,
	}
}
