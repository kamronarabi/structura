// Package terraform extracts architecture from HCL configuration.
//
// The governing constraint is that static parsing of Terraform gives you
// structure, not semantics. What it reliably yields is real and valuable:
// which resources are declared, what types they are, which modules are
// called, and — most importantly — the reference expressions between them.
// When aws_lambda_function.api mentions aws_sqs_queue.jobs.url, that is a
// dependency stated in the file, not an inference.
//
// What static parsing does not yield is anything computed at apply time:
// values behind var.* without defaults, count and for_each expansion, remote
// state, provider lookups. Attempting those produces confident-sounding wrong
// answers, which is worse than a gap — a missing edge is visible to the user,
// while a fabricated one is not. So unresolved references become diagnostics
// rather than guesses.
package terraform

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/pkg/schema"
)

// Name is the extractor's stable identifier.
const Name = "terraform"

// Diagnostic codes this extractor emits. They are aggregated by the pipeline.
const (
	DiagUnresolvedReference = "terraform_unresolved_reference"
	DiagUnrecognizedType    = "terraform_unrecognized_resource"
	DiagUnexpanded          = "terraform_unexpanded"
	DiagJSONSyntax          = "terraform_json_syntax"
)

// Extractor reads Terraform configuration.
type Extractor struct{}

// New returns a Terraform extractor.
func New() *Extractor { return &Extractor{} }

// Name implements scan.Extractor.
func (*Extractor) Name() string { return Name }

// Match implements scan.Extractor.
func (*Extractor) Match(f scan.FileMeta) bool {
	if f.Ext == ".tf" {
		return true
	}
	// .tf.json has extension .json, so the full name has to be checked.
	return strings.HasSuffix(strings.ToLower(f.Name), ".tf.json")
}

// Extract implements scan.Extractor.
func (e *Extractor) Extract(_ context.Context, f *scan.File, emit scan.Emitter) error {
	if strings.HasSuffix(strings.ToLower(f.Name), ".tf.json") {
		// The JSON syntax is rare in hand-written configuration and is
		// usually machine-generated. Reporting it beats a silent gap.
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityInfo, Code: DiagJSONSyntax, Path: f.Path,
			Message: "Terraform JSON syntax is not parsed; resources declared in this file are missing from the graph",
		})
		return nil
	}

	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(f.Content, f.Name)
	if diags.HasErrors() {
		emit.Diag(schema.Diagnostic{
			Severity: schema.SeverityWarn, Code: "terraform_parse_failed", Path: f.Path,
			Line:    firstErrorLine(diags),
			Message: fmt.Sprintf("could not parse this Terraform file: %s", firstErrorMessage(diags)),
		})
		return nil
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}

	m := &module{
		file:      f,
		namespace: moduleNamespace(f.Dir),
		scope:     newScope(body),
		declared:  map[string]string{},
	}

	// Two passes. The first records every resource this file declares, so
	// that the second can tell a reference to a local resource — which is an
	// edge — from a reference to one in a sibling file, which is a hint.
	// An extractor may only emit an edge it can justify from the file in
	// front of it.
	m.collectDeclarations(body)
	m.emitAll(body, emit)
	return nil
}

// module is one .tf file's worth of context.
type module struct {
	file      *scan.File
	namespace string
	scope     *scope
	// declared maps a Terraform address ("aws_sqs_queue.jobs") to the node ID
	// it became, for resources declared in this file.
	declared map[string]string
}

// moduleNamespace names the directory a file belongs to. Terraform treats a
// directory as one module, so the directory is the natural scope for names.
// dirPath is the directory a file lives in, as a path a reader can act on.
func (m *module) dirPath() string {
	if m.file.Dir == "" {
		return "."
	}
	return m.file.Dir
}

func moduleNamespace(dir string) string {
	if dir == "" || dir == "." {
		return "root"
	}
	return dir
}

func (m *module) collectDeclarations(body *hclsyntax.Body) {
	for _, block := range body.Blocks {
		switch block.Type {
		case "resource":
			labels, ok := namedLabels(block, 2)
			if !ok {
				continue
			}
			resourceType, name := labels[0], labels[1]
			class, recognized, _ := classifyResource(resourceType)
			if !recognized {
				continue
			}
			m.declared[resourceType+"."+name] = m.nodeID(class.kind, resourceType, name)
		case "module":
			if labels, ok := namedLabels(block, 1); ok {
				m.declared["module."+labels[0]] =
					m.nodeID(moduleKind, "module", labels[0])
			}
		}
	}
}

func (m *module) nodeID(kind schema.NodeKind, resourceType, name string) string {
	return schema.NewNodeID(kind, m.namespace, resourceType+"."+name)
}

func (m *module) emitAll(body *hclsyntax.Body, emit scan.Emitter) {
	// Blocks are visited in source order, which HCL preserves, so the
	// emitted sequence does not depend on map iteration.
	for _, block := range body.Blocks {
		switch block.Type {
		case "resource":
			m.emitResource(block, emit, true)
		case "data":
			m.emitResource(block, emit, false)
		case "module":
			m.emitModule(block, emit)
		}
	}
}

func (m *module) emitResource(block *hclsyntax.Block, emit scan.Emitter, managed bool) {
	labels, ok := namedLabels(block, 2)
	if !ok {
		return
	}
	resourceType, name := labels[0], labels[1]
	line := block.TypeRange.Start.Line

	class, recognized, plumbing := classifyResource(resourceType)
	if !recognized {
		if !plumbing {
			// Reported in aggregate: the user learns which types were
			// skipped and how many, without a line per resource.
			emit.Diag(schema.Diagnostic{
				Severity: schema.SeverityInfo, Code: DiagUnrecognizedType,
				Path: m.dirPath(), Message: resourceType,
			})
		}
		return
	}

	address := resourceType + "." + name
	id := m.nodeID(class.kind, resourceType, name)
	src := schema.Source{Extractor: Name, Path: m.file.Path, Line: line}

	attrs := schema.Attrs{
		"resourceType": resourceType,
		"address":      address,
		"provider":     provider(resourceType),
	}
	if !managed {
		// A data source is infrastructure that exists but is managed
		// somewhere else. It is still part of the architecture, and saying
		// which is which matters for anyone reading the graph.
		attrs["managed"] = false
		attrs["address"] = "data." + address
	}
	// count and for_each are not expanded, so the instance count is unknown.
	// Recording the multiplicity is honest; guessing at a number is not.
	if _, ok := block.Body.Attributes["count"]; ok {
		attrs["multiplicity"] = "count"
		m.reportUnexpanded(emit, address, "count")
	}
	if _, ok := block.Body.Attributes["for_each"]; ok {
		attrs["multiplicity"] = "for_each"
		m.reportUnexpanded(emit, address, "for_each")
	}
	for key, attrName := range map[string]string{
		"name": "name", "bucket": "name", "function_name": "name",
		"identifier": "name", "engine": "engine", "engine_version": "engineVersion",
		"instance_class": "instanceClass", "runtime": "runtime", "handler": "handler",
		"fifo_queue": "fifo", "topic_arn": "topic",
	} {
		if v, ok := m.scope.literalAttr(block.Body, key); ok {
			attrs[attrName] = v
		}
	}

	confidence := schema.ConfDeclared
	if !managed {
		confidence = schema.ConfReference
	}
	emit.Node(schema.Node{
		ID:         id,
		Kind:       class.kind,
		Layer:      class.layer,
		Name:       displayName(attrs, name),
		Namespace:  m.namespace,
		Tech:       &schema.Tech{Runtime: provider(resourceType), Framework: class.tech},
		Attrs:      attrs,
		Sources:    []schema.Source{src},
		Confidence: confidence,
	})

	// The resource's address is how the rest of the configuration refers to
	// it, and its literal name is how a running service refers to it.
	aliasNames := []string{address}
	if literal, ok := attrs["name"].(string); ok && literal != "" {
		aliasNames = append(aliasNames, literal)
	}
	emit.Alias(resolve.Alias{
		Name: name, Namespace: m.namespace, DNS: aliasNames, TargetName: address,
		Source: schema.Evidence{
			Extractor: Name, Path: m.file.Path, Line: line,
			Rule:   "terraform_resource_address",
			Detail: fmt.Sprintf("%s is addressed as %s", resourceType, address),
		},
	})

	m.emitReferences(block, emit, id, address, line)
}

// moduleKind is what a `module` block becomes.
//
// It was a boundary at the context layer, and both halves were wrong. A
// boundary is a grouping that holds other nodes through contains edges, and a
// module call never gets any: the resources it instantiates are declared in
// another directory and are extracted under their own namespace, so the
// module node groups nothing that this graph can see. Nor can it -- a module
// reused by eight callers would have to sit inside eight containers at once,
// and containment has to be a tree for a reader to navigate it.
//
// The cost was not cosmetic. Boundaries are excluded from the connected count
// and from resolution targets, so a repository built out of module calls --
// which is what a Terraform module library is -- reported no relationships at
// all and could not be referred to. terraform-aws-vpc came out as 48 nodes,
// every one of them a context-layer boundary, with 19 real dependency edges
// between them and a cohesion of zero.
//
// A module call is an instantiated unit of infrastructure whose composition
// is not visible from here, which is what cloud_resource means, and it sits
// inside the system rather than around it, which is the container layer.
// Whether its definition is in this repository at all is recorded in attrs,
// where it is a fact about the node rather than a claim about its shape.
const moduleKind = schema.KindCloudResource

func (m *module) emitModule(block *hclsyntax.Block, emit scan.Emitter) {
	labels, ok := namedLabels(block, 1)
	if !ok {
		return
	}
	name := labels[0]
	line := block.TypeRange.Start.Line
	id := m.nodeID(moduleKind, "module", name)

	attrs := schema.Attrs{"terraformModule": name}
	if source, ok := m.scope.literalAttr(block.Body, "source"); ok {
		attrs["source"] = source
		// A registry or git source is code that is not in this repository,
		// so whatever it declares cannot appear in the graph.
		if !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") {
			attrs["external"] = true
			emit.Diag(schema.Diagnostic{
				Severity: schema.SeverityInfo, Code: "terraform_external_module",
				Path: m.file.Path, Line: line,
				Message: fmt.Sprintf("module %q comes from %q, which is outside this repository; the resources it declares are not in the graph",
					name, source),
			})
		}
	}

	emit.Node(schema.Node{
		ID: id, Kind: moduleKind, Layer: schema.LayerContainer,
		Name: name, Namespace: m.namespace,
		Tech:  &schema.Tech{Runtime: "terraform", Framework: "module"},
		Attrs: attrs, Sources: []schema.Source{{Extractor: Name, Path: m.file.Path, Line: line}},
		Confidence: schema.ConfDeclared,
	})

	m.emitReferences(block, emit, id, "module."+name, line)
}

// emitReferences walks every expression in a block and turns the traversals
// it finds into edges or hints.
//
// This is where most of Terraform's architectural value lives. A reference
// from one resource to another is a dependency the author wrote down, and it
// does not depend on anything being evaluated.
func (m *module) emitReferences(block *hclsyntax.Block, emit scan.Emitter, fromID, fromAddress string, blockLine int) {
	seen := map[string]bool{}

	for _, ref := range m.scope.references(block) {
		if ref.address == fromAddress || seen[ref.address] {
			continue
		}
		seen[ref.address] = true

		line := ref.line
		if line == 0 {
			line = blockLine
		}

		switch {
		case ref.unresolved:
			// A value that only exists at apply time. Saying so is the
			// point: a graph with an honest gap beats one with an invented
			// edge, because the user can see the gap and the model cannot
			// see the invention.
			// Keyed by directory rather than by file so the pipeline
			// collapses these into one entry per module. That a module is
			// parameterized is a single fact about it; repeating it per
			// expression drowns every other diagnostic.
			emit.Diag(schema.Diagnostic{
				Severity: schema.SeverityInfo, Code: DiagUnresolvedReference,
				Path: m.dirPath(), Message: ref.address,
			})

		case m.declared[ref.address] != "":
			// Both ends are in this file, so the edge is justified without
			// looking anywhere else.
			from, to := fromID, m.declared[ref.address]
			kind := edgeKindFor(ref.address)
			rule := "terraform_reference"
			if kind == schema.EdgeContains {
				// Containment runs from the container, and a reference
				// always runs the other way: `vpc_id = aws_vpc.main.id` is
				// the member naming its boundary, never the boundary
				// enumerating its members. Written as-is it produced edges
				// asserting that a Lambda contains a VPC and a task
				// contains its ECS cluster -- backwards, and stated at the
				// confidence of a declared fact.
				from, to = to, from
				rule = "terraform_containment"
			}
			emit.Edge(schema.Edge{
				From: from, To: to,
				Kind:       kind,
				Confidence: schema.ConfReference,
				Evidence: []schema.Evidence{{
					Extractor: Name, Path: m.file.Path, Line: line,
					Rule:   rule,
					Detail: fmt.Sprintf("%s references %s", fromAddress, ref.expression),
				}},
			})

		default:
			// Terraform treats a whole directory as one module, so a
			// reference to a resource declared in a sibling file is normal
			// and extremely common. The resolver joins them up.
			emit.Hint(resolve.Hint{
				FromNode:      fromID,
				Kind:          resolve.HintTraversal,
				Raw:           ref.expression,
				Tokens:        []string{ref.address},
				SuggestedEdge: edgeKindFor(ref.address),
				Source: schema.Evidence{
					Extractor: Name, Path: m.file.Path, Line: line,
					Rule:   "terraform_reference",
					Detail: fmt.Sprintf("%s references %s", fromAddress, ref.expression),
				},
			})
		}
	}
}

func (m *module) reportUnexpanded(emit scan.Emitter, address, meta string) {
	emit.Diag(schema.Diagnostic{
		Severity: schema.SeverityInfo, Code: DiagUnexpanded,
		Path:    m.dirPath(),
		Message: fmt.Sprintf("%s uses %s", address, meta),
	})
}

// edgeKindFor infers the relationship from what is being referenced. The
// target's type is the only thing available statically, and it is usually
// enough: referencing a queue means publishing to it, referencing a database
// means persisting to it.
func edgeKindFor(address string) schema.EdgeKind {
	resourceType, _, _ := strings.Cut(address, ".")
	resourceType = strings.TrimPrefix(resourceType, "data.")

	class, recognized, _ := classifyResource(resourceType)
	if !recognized {
		return schema.EdgeDependsOn
	}
	switch class.kind {
	case schema.KindDatastore:
		return schema.EdgePersistsTo
	case schema.KindQueue:
		return schema.EdgePublishesTo
	case schema.KindService:
		return schema.EdgeCalls
	case schema.KindBoundary:
		return schema.EdgeContains
	default:
		return schema.EdgeDependsOn
	}
}

func displayName(attrs schema.Attrs, fallback string) string {
	if name, ok := attrs["name"].(string); ok && name != "" && !strings.Contains(name, "${") {
		return name
	}
	return fallback
}

func firstErrorMessage(diags hcl.Diagnostics) string {
	for _, d := range diags {
		if d.Severity == hcl.DiagError {
			if d.Detail != "" {
				return d.Summary + ": " + d.Detail
			}
			return d.Summary
		}
	}
	return "unknown error"
}

func firstErrorLine(diags hcl.Diagnostics) int {
	for _, d := range diags {
		if d.Severity == hcl.DiagError && d.Subject != nil {
			return d.Subject.Start.Line
		}
	}
	return 0
}

// namedLabels returns a block's labels when there are exactly n of them and
// none is empty. Terraform requires every label to be an identifier, so
// `resource "aws_sqs_queue" "" {}` is as malformed as a block with the wrong
// number of labels — but it parses, and an empty label would otherwise reach
// the graph as a node with no name, which Validate rejects.
func namedLabels(block *hclsyntax.Block, n int) ([]string, bool) {
	if len(block.Labels) != n {
		return nil, false
	}
	for _, l := range block.Labels {
		if l == "" {
			return nil, false
		}
	}
	return block.Labels, true
}
