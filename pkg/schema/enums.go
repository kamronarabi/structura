package schema

import "fmt"

// NodeKind classifies what a node represents.
type NodeKind string

const (
	// KindService is anything that runs and serves requests: a container, a
	// Lambda, a Compose service, a Deployment.
	KindService NodeKind = "service"
	// KindDatastore is a database or cache: Postgres, Redis, DynamoDB, S3.
	KindDatastore NodeKind = "datastore"
	// KindQueue is an asynchronous transport: SQS, Kafka, SNS, EventBridge.
	KindQueue NodeKind = "queue"
	// KindExternal is a third-party system reached over the network, such as
	// api.stripe.com. These are synthesized by the resolver from references
	// that match no node in the repo, and are what populate the C4 context
	// diagram's outer ring.
	KindExternal NodeKind = "external"
	// KindCloudResource is infrastructure that is neither compute nor
	// storage: a load balancer, a VPC, an IAM role, a CDN.
	KindCloudResource NodeKind = "cloud_resource"
	// KindPackage is a library dependency declared in a manifest.
	KindPackage NodeKind = "package"
	// KindBoundary is a grouping: a namespace, a cluster, an AWS account, a
	// Compose project. Boundaries contain other nodes via "contains" edges.
	KindBoundary NodeKind = "boundary"
)

var nodeKinds = map[NodeKind]bool{
	KindService: true, KindDatastore: true, KindQueue: true,
	KindExternal: true, KindCloudResource: true, KindPackage: true,
	KindBoundary: true,
}

// Valid reports whether k is a known node kind.
func (k NodeKind) Valid() bool { return nodeKinds[k] }

// Layer is the C4 level a node belongs to.
type Layer string

const (
	// LayerContext is the outermost C4 level: the system and the external
	// systems it talks to.
	LayerContext Layer = "context"
	// LayerContainer is the deployable-unit level: services, datastores,
	// queues. Phase 1 emits mostly this.
	LayerContainer Layer = "container"
	// LayerComponent is the inside-a-service level. Phase 1 never emits it;
	// the Phase 2 AST extractors fill it in.
	LayerComponent Layer = "component"
)

var layers = map[Layer]bool{LayerContext: true, LayerContainer: true, LayerComponent: true}

// Valid reports whether l is a known layer.
func (l Layer) Valid() bool { return layers[l] }

// EdgeKind classifies a relationship.
type EdgeKind string

const (
	// EdgeCalls is a synchronous request: HTTP, gRPC, RPC.
	EdgeCalls EdgeKind = "calls"
	// EdgePersistsTo is a read or write against a datastore.
	EdgePersistsTo EdgeKind = "persists_to"
	// EdgePublishesTo is a write to a queue or topic.
	EdgePublishesTo EdgeKind = "publishes_to"
	// EdgeSubscribesTo is a read from a queue or topic.
	EdgeSubscribesTo EdgeKind = "subscribes_to"
	// EdgeDependsOn is a declared ordering or build-time dependency that
	// implies no particular runtime protocol.
	EdgeDependsOn EdgeKind = "depends_on"
	// EdgeExposes is an ingress relationship: something routes traffic to a
	// service from outside.
	EdgeExposes EdgeKind = "exposes"
	// EdgeContains is structural nesting: a boundary holding its members.
	EdgeContains EdgeKind = "contains"
)

var edgeKinds = map[EdgeKind]bool{
	EdgeCalls: true, EdgePersistsTo: true, EdgePublishesTo: true,
	EdgeSubscribesTo: true, EdgeDependsOn: true, EdgeExposes: true,
	EdgeContains: true,
}

// Valid reports whether k is a known edge kind.
func (k EdgeKind) Valid() bool { return edgeKinds[k] }

// Severity grades a diagnostic.
type Severity string

const (
	// SeverityInfo notes something worth knowing that cost no fidelity.
	SeverityInfo Severity = "info"
	// SeverityWarn means the graph is probably incomplete here.
	SeverityWarn Severity = "warn"
	// SeverityError means a file that should have parsed did not.
	SeverityError Severity = "error"
)

var severities = map[Severity]bool{SeverityInfo: true, SeverityWarn: true, SeverityError: true}

// Valid reports whether s is a known severity.
func (s Severity) Valid() bool { return severities[s] }

// Confidence values for the evidence sources the resolver knows about.
// They are constants rather than magic numbers at each call site so that
// recalibrating the resolver is a single-file change.
const (
	// ConfDeclared is an explicit statement in a config file, such as a
	// Compose depends_on. There is nothing to infer.
	ConfDeclared = 1.00
	// ConfReference is a direct expression reference, such as a Terraform
	// traversal from one resource to another, or a Kubernetes label-selector
	// chain. Deterministic, but resolved by us rather than stated outright.
	ConfReference = 0.95
	// ConfHostMatch is a hostname in an env var or connection string that
	// matches a known node's name.
	ConfHostMatch = 0.80
	// ConfIndirect is a match that arrived through a level of indirection,
	// such as a ConfigMap value mounted into a pod.
	ConfIndirect = 0.70
	// ConfWeak is a naming-convention or shared-image match with no other
	// corroboration. Emitted, but marked for what it is.
	ConfWeak = 0.40
)

// Validate reports the first structural problem with a node, or nil.
func (n Node) Validate() error {
	if err := ValidateNodeID(n.ID); err != nil {
		return err
	}
	if !n.Kind.Valid() {
		return fmt.Errorf("node %s: unknown kind %q", n.ID, n.Kind)
	}
	if !n.Layer.Valid() {
		return fmt.Errorf("node %s: unknown layer %q", n.ID, n.Layer)
	}
	if n.Name == "" {
		return fmt.Errorf("node %s: empty name", n.ID)
	}
	if n.Confidence < 0 || n.Confidence > 1 {
		return fmt.Errorf("node %s: confidence %v out of range [0,1]", n.ID, n.Confidence)
	}
	for _, s := range n.Sources {
		if err := validateRelPath(s.Path); err != nil {
			return fmt.Errorf("node %s: source: %w", n.ID, err)
		}
	}
	return nil
}

// Validate reports the first structural problem with an edge, or nil.
func (e Edge) Validate() error {
	if e.ID == "" {
		return fmt.Errorf("edge %s->%s: empty id", e.From, e.To)
	}
	if err := ValidateNodeID(e.From); err != nil {
		return fmt.Errorf("edge %s: from: %w", e.ID, err)
	}
	if err := ValidateNodeID(e.To); err != nil {
		return fmt.Errorf("edge %s: to: %w", e.ID, err)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("edge %s: unknown kind %q", e.ID, e.Kind)
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return fmt.Errorf("edge %s: confidence %v out of range [0,1]", e.ID, e.Confidence)
	}
	// An edge with no evidence is an assertion with no basis. The whole
	// point of the confidence model is that every claim is traceable.
	if len(e.Evidence) == 0 {
		return fmt.Errorf("edge %s: no evidence", e.ID)
	}
	for _, ev := range e.Evidence {
		if ev.Rule == "" {
			return fmt.Errorf("edge %s: evidence with empty rule", e.ID)
		}
		if err := validateRelPath(ev.Path); err != nil {
			return fmt.Errorf("edge %s: evidence: %w", e.ID, err)
		}
	}
	return nil
}

// Validate reports the first structural problem with a diagnostic, or nil.
func (d Diagnostic) Validate() error {
	if !d.Severity.Valid() {
		return fmt.Errorf("diagnostic %q: unknown severity %q", d.Code, d.Severity)
	}
	if d.Code == "" {
		return fmt.Errorf("diagnostic: empty code")
	}
	if d.Message == "" {
		return fmt.Errorf("diagnostic %q: empty message", d.Code)
	}
	return validateRelPath(d.Path)
}

// The ordered value lists below are the single source of truth for what each
// enum may contain. They back the published JSON Schema, and the MCP server
// uses them to validate filter arguments before touching the graph, so a new
// kind never has to be added in more than one place.

// NodeKinds returns every valid node kind, in a stable order.
func NodeKinds() []NodeKind {
	return []NodeKind{
		KindService, KindDatastore, KindQueue, KindExternal,
		KindCloudResource, KindPackage, KindBoundary,
	}
}

// Layers returns every valid layer, outermost first.
func Layers() []Layer {
	return []Layer{LayerContext, LayerContainer, LayerComponent}
}

// EdgeKinds returns every valid edge kind, in a stable order.
func EdgeKinds() []EdgeKind {
	return []EdgeKind{
		EdgeCalls, EdgePersistsTo, EdgePublishesTo, EdgeSubscribesTo,
		EdgeDependsOn, EdgeExposes, EdgeContains,
	}
}

// Severities returns every valid diagnostic severity, least severe first.
func Severities() []Severity {
	return []Severity{SeverityInfo, SeverityWarn, SeverityError}
}
