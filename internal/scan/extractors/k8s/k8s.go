// Package k8s extracts an architecture graph from Kubernetes manifests.
//
// Two things make this harder than Compose. Kubernetes splits one logical
// component across several objects — a Deployment, a Service, sometimes an
// Ingress — so the extractor has to decide which of them is the component and
// which are names for it. And a great many repositories do not commit plain
// manifests at all: they commit Helm charts, whose templates are not valid
// YAML. What this package does about the second case is report it precisely
// rather than fail on it; see helm.go.
package k8s

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/kamronarabi/structura/internal/classify"
	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/yamlpos"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier.
const Name = "k8s"

// DefaultNamespace is what an object with no namespace belongs to.
const DefaultNamespace = "default"

// Extractor reads Kubernetes manifests.
type Extractor struct{}

// New returns a Kubernetes extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

// composeNames are handled by the compose extractor; claiming them here would
// produce a second, wrong reading of the same file.
var composeNames = map[string]bool{
	"docker-compose.yml": true, "docker-compose.yaml": true,
	"compose.yml": true, "compose.yaml": true,
}

// Match implements scan.Extractor.
//
// Every YAML file in the repository is claimed, because there is no way to
// tell a manifest from any other YAML without opening it, and Match is not
// allowed to. Extract returns quietly when the contents turn out not to be
// Kubernetes, so the cost of a wrong guess here is one parse, not a
// diagnostic the user has to read.
func (*Extractor) Match(f scan.FileMeta) bool {
	if f.Ext != ".yaml" && f.Ext != ".yml" {
		return false
	}
	name := strings.ToLower(f.Name)
	if composeNames[name] {
		return false
	}
	for _, prefix := range []string{"docker-compose.", "compose."} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(_ context.Context, f *scan.File, emit scan.Emitter) error {
	if isHelmTemplate(f) {
		reportHelmTemplate(f, emit)
		return nil
	}
	// Plenty of committed kustomization.yaml files omit apiVersion and kind,
	// so recognizing them by name is the only reliable route.
	if classify.IsKustomization(f.Name) {
		reportKustomization(f, emit, nil)
		return nil
	}

	pos := yamlpos.New(f.Content)
	dec := yaml.NewDecoder(bytes.NewReader(f.Content))

	for index := 0; ; index++ {
		// Decoding happens in two passes. The first reads the document into
		// a generic node, which only fails on genuinely malformed YAML; the
		// second decodes into the typed manifest.
		//
		// The order matters. This extractor claims every YAML file in the
		// repository, because there is no way to tell a manifest from any
		// other YAML without opening it. Reporting a parse failure before
		// checking whether the document is even a Kubernetes object means
		// every Helm values.yaml with a field we typed as a string becomes
		// a scary "could not be parsed" line in the user's diagnostics. A
		// diagnostic list full of false alarms is worse than a shorter one,
		// because the user stops reading it.
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if isEOF(err) {
				break
			}
			if looksLikeManifest(f.Content) {
				emit.Diag(schema.Diagnostic{
					Severity: schema.SeverityWarn,
					Code:     "k8s_parse_failed",
					Path:     f.Path,
					Line:     pos.LineIn(index, "kind"),
					Message: fmt.Sprintf("document %d is not valid YAML (%v); any later documents in this file were not read",
						index+1, oneLine(err)),
				})
			}
			return nil
		}

		if !isKubernetesObject(&doc) {
			// Not a Kubernetes object. Most YAML in a repository is not.
			continue
		}

		var m manifest
		if err := doc.Decode(&m); err != nil {
			emit.Diag(schema.Diagnostic{
				Severity: schema.SeverityWarn,
				Code:     "k8s_parse_failed",
				Path:     f.Path,
				Line:     pos.LineIn(index, "kind"),
				Message:  fmt.Sprintf("%s could not be read (%v)", describe(&doc), oneLine(err)),
			})
			continue
		}
		if m.Kind == "" || m.APIVersion == "" || m.Metadata.Name == "" {
			continue
		}
		e.emitObject(f, pos, index, emit, &m)
	}
	return nil
}

// isKubernetesObject reports whether a document declares the two fields every
// Kubernetes object has.
func isKubernetesObject(doc *yaml.Node) bool {
	n := doc
	for n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return false
	}
	var hasAPIVersion, hasKind bool
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "apiVersion":
			hasAPIVersion = n.Content[i+1].Value != ""
		case "kind":
			hasKind = n.Content[i+1].Value != ""
		}
	}
	return hasAPIVersion && hasKind
}

// looksLikeManifest is the same check against raw bytes, for the case where
// the YAML is too broken to produce a node at all.
func looksLikeManifest(content []byte) bool {
	return bytes.Contains(content, []byte("apiVersion:")) && bytes.Contains(content, []byte("kind:"))
}

// describe names a document for a diagnostic, using whatever it managed to
// declare.
func describe(doc *yaml.Node) string {
	n := doc
	for n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	kind, name := "", ""
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch n.Content[i].Value {
			case "kind":
				kind = n.Content[i+1].Value
			case "metadata":
				meta := n.Content[i+1]
				for j := 0; j+1 < len(meta.Content); j += 2 {
					if meta.Content[j].Value == "name" {
						name = meta.Content[j+1].Value
					}
				}
			}
		}
	}
	switch {
	case kind != "" && name != "":
		return fmt.Sprintf("%s %q", kind, name)
	case kind != "":
		return "a " + kind
	default:
		return "a Kubernetes object"
	}
}

// oneLine flattens a multi-line YAML error so a diagnostic stays one message.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

func (e *Extractor) emitObject(f *scan.File, pos *yamlpos.Locator, doc int, emit scan.Emitter, m *manifest) {
	switch m.Kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController",
		"Job", "CronJob", "Pod", "Rollout":
		e.emitWorkload(f, pos, doc, emit, m)
	case "Service":
		e.emitService(f, pos, doc, emit, m)
	case "Ingress":
		e.emitIngress(f, pos, doc, emit, m)
	case "ConfigMap":
		e.emitConfigMap(f, pos, doc, emit, m)
	}
}

func namespaceOf(m *manifest) string {
	if m.Metadata.Namespace != "" {
		return m.Metadata.Namespace
	}
	return DefaultNamespace
}

// emitNamespaceBoundary places a component inside the namespace that deploys
// it, and declares that namespace as a boundary if nothing has yet.
//
// Without this a Kubernetes repository has no hierarchy at all: Compose emits
// a boundary per project and Helm one per chart, but plain manifests produced
// a flat list of workloads with nothing enclosing them. A C4 view is built on
// exactly this nesting -- a system you can open to see its containers -- so a
// diagram drawn from a manifest repository had no level to zoom out to.
//
// A namespace is the right grouping to use. It is the boundary Kubernetes
// itself enforces for naming, DNS and access, which is the same line the
// resolver already refuses to match across.
func emitNamespaceBoundary(f *scan.File, emit scan.Emitter, memberID, ns string, line int) {
	boundaryID := schema.NewNodeID(schema.KindBoundary, scopeOf(ns), ns)
	if boundaryID == memberID {
		return
	}

	// Emitted per member; the builder merges the repeats into one node.
	emit.Node(schema.Node{
		ID:        boundaryID,
		Kind:      schema.KindBoundary,
		Layer:     schema.LayerContext,
		Name:      ns,
		Namespace: ns,
		Tech:      &schema.Tech{Runtime: "kubernetes"},
		Attrs:     schema.Attrs{"orchestrator": "kubernetes", "namespace": ns},
		Sources:   []schema.Source{{Extractor: Name, Path: f.Path, Line: line}},
		// The namespace is stated by the manifests themselves, even when it
		// is the implicit default.
		Confidence: schema.ConfDeclared,
	})

	emit.Edge(schema.Edge{
		From: boundaryID, To: memberID, Kind: schema.EdgeContains,
		Confidence: schema.ConfDeclared,
		Evidence: []schema.Evidence{{
			Extractor: Name, Path: f.Path, Line: line,
			Rule:   "k8s_namespace_member",
			Detail: fmt.Sprintf("deployed into namespace %q", ns),
		}},
	})
}

// emitWorkload turns a controller into the component it runs.
func (e *Extractor) emitWorkload(f *scan.File, pos *yamlpos.Locator, doc int, emit scan.Emitter, m *manifest) {
	ns := namespaceOf(m)
	line := pos.LineIn(doc, "metadata", "name")
	src := schema.Source{Extractor: Name, Path: f.Path, Line: line}

	pods := m.pods()
	var containers []container
	for _, p := range pods {
		containers = append(containers, p.allContainers()...)
	}

	// A workload manifest that declares no pod template at all is a
	// strategic-merge patch, not a component: Kustomize overlays are full of
	// files that name a Deployment purely to override its replica count.
	// Emitting a node for one would invent a service with no image, once per
	// environment, which is exactly the kind of confident-looking wrong
	// answer the whole design is trying to avoid.
	if len(pods) == 0 && m.Kind != "Pod" {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityInfo,
			Code:     "k8s_patch_fragment",
			Path:     f.Path,
			Line:     line,
			Message: fmt.Sprintf(
				"%s %q declares no pod template, so it was read as a patch rather than a component; its overrides are not reflected in the graph",
				m.Kind, m.Metadata.Name),
		})
		return
	}

	kind, tech := workloadKind(containers)
	id := schema.NewNodeID(kind, scopeOf(ns), m.Metadata.Name)

	attrs := schema.Attrs{"workload": m.Kind}
	if images := containerImages(containers); len(images) > 0 {
		attrs["images"] = images
		attrs["image"] = images[0]
	}
	if ports := containerPorts(containers); len(ports) > 0 {
		attrs["ports"] = ports
	}
	if m.Spec.Replicas != nil {
		attrs["replicas"] = *m.Spec.Replicas
	}
	if m.Spec.Schedule != "" {
		attrs["schedule"] = m.Spec.Schedule
	}
	// The pod labels are what a Service selector is matched against, so they
	// have to reach the resolver. They travel on the node rather than
	// through a separate channel because they are a property of the
	// workload, not a relationship.
	if labels := m.podLabels(); len(labels) > 0 {
		attrs["labels"] = sortedStringMap(labels)
	}
	if len(pods) > 0 && pods[0].ServiceAccountName != "" {
		attrs["serviceAccount"] = pods[0].ServiceAccountName
	}

	emit.Node(schema.Node{
		ID:         id,
		Kind:       kind,
		Layer:      schema.LayerContainer,
		Name:       m.Metadata.Name,
		Namespace:  ns,
		Tech:       &schema.Tech{Runtime: "kubernetes", Framework: tech},
		Attrs:      attrs,
		Sources:    []schema.Source{src},
		Confidence: schema.ConfDeclared,
	})

	emitNamespaceBoundary(f, emit, id, ns, line)

	// A StatefulSet's governing service is a name for this workload, stated
	// outright rather than by selector.
	if m.Spec.ServiceName != "" {
		emit.Alias(resolve.Alias{
			Name: m.Spec.ServiceName, Namespace: ns,
			DNS:        resolve.DNSNames(m.Spec.ServiceName, ns),
			TargetName: m.Metadata.Name,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: pos.LineIn(doc, "spec", "serviceName"),
				Rule:   "statefulset_governing_service",
				Detail: fmt.Sprintf("StatefulSet %q is addressed as %q", m.Metadata.Name, m.Spec.ServiceName),
			},
		})
	}

	e.emitContainerReferences(f, pos, doc, emit, id, ns, containers)
	e.emitVolumeReferences(f, emit, id, ns, pods)
}

// workloadKind decides what a workload is from the images it runs.
func workloadKind(containers []container) (kind schema.NodeKind, tech string) {
	// A recognized datastore or queue image anywhere in the pod decides it:
	// sidecars are near-universal, and a pod with a postgres container in it
	// is a database however many log shippers ride alongside.
	best := schema.KindService
	bestTech := ""
	for _, c := range containers {
		k, t, recognized := classify.ImageKind(c.Image)
		if !recognized {
			continue
		}
		if k == schema.KindDatastore || k == schema.KindQueue {
			return k, t
		}
		if best == schema.KindService && k != schema.KindService {
			best, bestTech = k, t
		}
	}
	return best, bestTech
}

// emitService records a Service as a name for a workload rather than as a
// component. See resolve.Alias for why.
func (e *Extractor) emitService(f *scan.File, pos *yamlpos.Locator, doc int, emit scan.Emitter, m *manifest) {
	ns := namespaceOf(m)
	line := pos.LineIn(doc, "metadata", "name")

	alias := resolve.Alias{
		Name:      m.Metadata.Name,
		Namespace: ns,
		DNS:       resolve.DNSNames(m.Metadata.Name, ns),
		Ports:     servicePorts(m.Spec.Ports),
		Selector:  m.Spec.Selector.Labels(),
		Source: schema.Evidence{
			Extractor: Name, Path: f.Path, Line: line,
			Rule:   "k8s_service_selector",
			Detail: fmt.Sprintf("Service %q in namespace %q selects %s", m.Metadata.Name, ns, formatLabels(m.Spec.Selector.Labels())),
		},
	}

	// An ExternalName Service is a CNAME to something outside the cluster —
	// a managed database, a SaaS endpoint. That is a real architectural
	// fact and belongs on the context diagram.
	if m.Spec.Type == "ExternalName" && m.Spec.ExternalName != "" {
		alias.External = m.Spec.ExternalName
		alias.Source.Rule = "k8s_service_external_name"
		alias.Source.Detail = fmt.Sprintf("Service %q is a CNAME to %q outside the cluster",
			m.Metadata.Name, m.Spec.ExternalName)
	} else if len(alias.Selector) == 0 {
		// A selectorless Service is backed by manually managed Endpoints.
		// There is nothing in the repository to attach it to, so say so
		// rather than leaving a silent gap.
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityInfo,
			Code:     "k8s_service_without_selector",
			Path:     f.Path,
			Line:     line,
			Message: fmt.Sprintf("Service %q has no selector; its backend is defined by Endpoints that are not in this repository",
				m.Metadata.Name),
		})
	}
	emit.Alias(alias)
}

// emitIngress records the route from outside the cluster to a Service. The
// Ingress itself is infrastructure, so it becomes a node: it is where rate
// limiting, TLS termination, and routing live, which is exactly what the
// Phase 3 bottleneck profiler looks for.
func (e *Extractor) emitIngress(f *scan.File, pos *yamlpos.Locator, doc int, emit scan.Emitter, m *manifest) {
	ns := namespaceOf(m)
	line := pos.LineIn(doc, "metadata", "name")
	id := schema.NewNodeID(schema.KindCloudResource, scopeOf(ns), m.Metadata.Name)

	attrs := schema.Attrs{"workload": "Ingress"}
	if m.Spec.IngressClassName != "" {
		attrs["ingressClass"] = m.Spec.IngressClassName
	}
	if hosts := ingressHosts(m); len(hosts) > 0 {
		attrs["hosts"] = hosts
	}
	if len(m.Spec.TLS) > 0 {
		attrs["tls"] = true
	}

	emit.Node(schema.Node{
		ID:         id,
		Kind:       schema.KindCloudResource,
		Layer:      schema.LayerContainer,
		Name:       m.Metadata.Name,
		Namespace:  ns,
		Tech:       &schema.Tech{Runtime: "kubernetes", Framework: "ingress"},
		Attrs:      attrs,
		Sources:    []schema.Source{{Extractor: Name, Path: f.Path, Line: line}},
		Confidence: schema.ConfDeclared,
	})

	emitNamespaceBoundary(f, emit, id, ns, line)

	for _, target := range ingressBackends(m) {
		emit.Hint(resolve.Hint{
			FromNode:      id,
			Kind:          resolve.HintSelector,
			Raw:           target.service,
			Tokens:        resolve.DNSNames(target.service, ns),
			Port:          target.port,
			Protocol:      "http",
			SuggestedEdge: schema.EdgeExposes,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: line,
				Rule:   "k8s_ingress_backend",
				Detail: fmt.Sprintf("Ingress %q routes %s to Service %q", m.Metadata.Name, target.where, target.service),
			},
		})
	}
}

// emitConfigMap records references hiding in configuration data.
//
// The values belong to whichever pods mount the ConfigMap, which this
// extractor cannot know. They are emitted against a synthetic identifier that
// the resolver joins to the pods that referenced it, at the lower confidence
// an extra level of indirection deserves.
func (e *Extractor) emitConfigMap(f *scan.File, pos *yamlpos.Locator, doc int, emit scan.Emitter, m *manifest) {
	ns := namespaceOf(m)
	line := pos.LineIn(doc, "metadata", "name")

	data := m.stringData()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		ref, ok := resolve.ParseValue(k, data[k])
		if !ok {
			continue
		}
		redacted, _ := resolve.RedactValue(k, data[k])
		emit.Hint(resolve.Hint{
			FromNode:      ConfigMapRef(ns, m.Metadata.Name),
			Kind:          resolve.HintConfigValue,
			Raw:           redacted,
			Tokens:        ref.Tokens,
			Port:          ref.Port,
			Protocol:      ref.Protocol,
			SuggestedEdge: ref.Edge,
			Source: schema.Evidence{
				Extractor: Name, Path: f.Path, Line: line,
				Rule:   "k8s_configmap_value",
				Detail: fmt.Sprintf("ConfigMap %q key %s=%s", m.Metadata.Name, k, redacted),
			},
		})
	}
}

// ConfigMapRef is the synthetic identifier a ConfigMap's values are attached
// to before the resolver knows which pods consume them. It is never a node ID
// and never reaches the graph.
func ConfigMapRef(namespace, name string) string {
	return "configmap:" + namespace + "/" + name
}

func (e *Extractor) emitContainerReferences(
	f *scan.File, pos *yamlpos.Locator, doc int, emit scan.Emitter,
	id, ns string, containers []container,
) {
	line := pos.LineIn(doc, "metadata", "name")

	for _, c := range containers {
		for _, env := range c.Env {
			if env.ValueFrom != nil {
				// The value lives in a ConfigMap or Secret. A Secret's
				// contents are not in the repository and must not be; a
				// ConfigMap's are, and the resolver joins them up.
				if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil {
					emitConfigMapUse(f, emit, id, ns, ref.Name, line,
						fmt.Sprintf("env %s comes from ConfigMap %q key %q", env.Name, ref.Name, ref.Key))
				}
				continue
			}
			ref, ok := resolve.ParseValue(env.Name, env.Value)
			if !ok {
				continue
			}
			redacted, _ := resolve.RedactValue(env.Name, env.Value)
			emit.Hint(resolve.Hint{
				FromNode:      id,
				Kind:          ref.Kind,
				Raw:           redacted,
				Tokens:        ref.Tokens,
				Port:          ref.Port,
				Protocol:      ref.Protocol,
				SuggestedEdge: ref.Edge,
				Source: schema.Evidence{
					Extractor: Name, Path: f.Path, Line: line,
					Rule:   "k8s_env_reference",
					Detail: fmt.Sprintf("container %q env %s=%s", c.Name, env.Name, redacted),
				},
			})
		}
		for _, from := range c.EnvFrom {
			if from.ConfigMapRef != nil {
				emitConfigMapUse(f, emit, id, ns, from.ConfigMapRef.Name, line,
					fmt.Sprintf("container %q takes its environment from ConfigMap %q", c.Name, from.ConfigMapRef.Name))
			}
		}
	}
}

func (e *Extractor) emitVolumeReferences(
	f *scan.File, emit scan.Emitter, id, ns string, pods []podSpec,
) {
	for _, p := range pods {
		for _, v := range p.Volumes {
			if v.ConfigMap != nil {
				emitConfigMapUse(f, emit, id, ns, v.ConfigMap.Name, 0,
					fmt.Sprintf("volume %q mounts ConfigMap %q", v.Name, v.ConfigMap.Name))
			}
		}
	}
}

// emitConfigMapUse records that a workload consumes a ConfigMap, so the
// resolver can transfer that ConfigMap's references onto it.
func emitConfigMapUse(f *scan.File, emit scan.Emitter, id, ns, configMap string, line int, detail string) {
	emit.Hint(resolve.Hint{
		FromNode:      id,
		Kind:          resolve.HintConfigValue,
		Raw:           ConfigMapRef(ns, configMap),
		Tokens:        nil, // resolved by joining to the ConfigMap, not by name matching
		SuggestedEdge: schema.EdgeDependsOn,
		Source: schema.Evidence{
			Extractor: Name, Path: f.Path, Line: line,
			Rule:   "k8s_configmap_use",
			Detail: detail,
		},
	})
}

type ingressTarget struct {
	service string
	port    int
	where   string
}

func ingressBackends(m *manifest) []ingressTarget {
	var out []ingressTarget
	add := func(b backend, where string) {
		name := b.Name()
		if name == "" {
			return
		}
		port := 0
		if b.Service != nil {
			port = b.Service.Port.Number
		}
		out = append(out, ingressTarget{service: name, port: port, where: where})
	}
	if m.Spec.DefaultBackend != nil {
		add(*m.Spec.DefaultBackend, "the default route")
	}
	for _, rule := range m.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			where := p.Path
			if rule.Host != "" {
				where = rule.Host + p.Path
			}
			if where == "" {
				where = "/"
			}
			add(p.Backend, where)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].service != out[j].service {
			return out[i].service < out[j].service
		}
		return out[i].where < out[j].where
	})
	return out
}

func ingressHosts(m *manifest) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range m.Spec.Rules {
		if r.Host != "" && !seen[r.Host] {
			seen[r.Host] = true
			out = append(out, r.Host)
		}
	}
	for _, t := range m.Spec.TLS {
		for _, h := range t.Hosts {
			if h != "" && !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

func containerImages(containers []container) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range containers {
		if c.Image != "" && !seen[c.Image] {
			seen[c.Image] = true
			out = append(out, c.Image)
		}
	}
	sort.Strings(out)
	return out
}

func containerPorts(containers []container) []int {
	seen := map[int]bool{}
	var out []int
	for _, c := range containers {
		for _, p := range c.Ports {
			if p.ContainerPort > 0 && !seen[p.ContainerPort] {
				seen[p.ContainerPort] = true
				out = append(out, p.ContainerPort)
			}
		}
	}
	sort.Ints(out)
	return out
}

func servicePorts(ports []servicePort) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range ports {
		if p.Port > 0 && !seen[p.Port] {
			seen[p.Port] = true
			out = append(out, p.Port)
		}
	}
	sort.Ints(out)
	return out
}

func sortedStringMap(in map[string]string) map[string]string {
	// Map keys are sorted at marshal time; the copy exists so that the graph
	// does not alias the decoder's map.
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "nothing (no selector)"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + labels[k]
	}
	return strings.Join(parts, ",")
}

func isEOF(err error) bool {
	return err != nil && (err.Error() == "EOF" || strings.Contains(err.Error(), "EOF"))
}

// scopeOf turns a Kubernetes namespace into an identity scope.
//
// "default" is dropped, because it is how Kubernetes spells "nobody said". A
// manifest with no namespace field lands there, and treating that as a scope
// made the same service in a chart and in a manifest two components. A
// repository that really does run things in default loses nothing: the
// namespace is still on the node, and two projects both using it are still
// kept apart by the project qualification.
func scopeOf(namespace string) string {
	if namespace == "default" {
		return ""
	}
	return namespace
}
