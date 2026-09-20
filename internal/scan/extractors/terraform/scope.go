package terraform

import (
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

// scope holds what can be resolved without running Terraform: variable
// defaults and local values, each evaluated exactly one level deep.
//
// One level is a deliberate stopping point. Resolving further means
// evaluating expressions over values that do not exist until apply time, and
// every step past the first multiplies the chance of producing a confident
// wrong answer. A variable with a default is a fact the file states; a
// variable without one is a question only the deployment can answer.
type scope struct {
	ctx *hcl.EvalContext
}

// maxLocalPasses bounds the fixed-point iteration over local values. Real
// configurations nest a handful deep; the cap only matters for a cyclic
// definition, which Terraform would reject anyway.
const maxLocalPasses = 8

func newScope(body *hclsyntax.Body) *scope {
	variables := map[string]cty.Value{}
	for _, block := range body.Blocks {
		if block.Type != "variable" || len(block.Labels) != 1 {
			continue
		}
		attr, ok := block.Body.Attributes["default"]
		if !ok {
			// A variable with no default is supplied at apply time. There is
			// nothing here to resolve it to.
			continue
		}
		if v, diags := attr.Expr.Value(nil); !diags.HasErrors() {
			variables[block.Labels[0]] = v
		}
	}

	// Locals are resolved to a fixed point rather than in a single pass.
	// A local built from another local is ordinary Terraform
	// ("${local.name_prefix}-jobs"), and refusing to follow it would report
	// a value the file fully determines as unknowable.
	//
	// This is still static evaluation: locals form a dependency graph over
	// values already in the file, and nothing here reaches for anything that
	// only exists at apply time. The iteration cap bounds a cyclic
	// definition, which Terraform itself would reject.
	locals := map[string]cty.Value{}
	for pass := 0; pass < maxLocalPasses; pass++ {
		ctx := &hcl.EvalContext{Variables: map[string]cty.Value{
			"var":   objectOf(variables),
			"local": objectOf(locals),
		}}
		resolved := 0
		for _, block := range body.Blocks {
			if block.Type != "locals" {
				continue
			}
			for _, name := range sortedAttrNames(block.Body.Attributes) {
				if _, done := locals[name]; done {
					continue
				}
				if v, diags := block.Body.Attributes[name].Expr.Value(ctx); !diags.HasErrors() {
					locals[name] = v
					resolved++
				}
			}
		}
		if resolved == 0 {
			break
		}
	}

	return &scope{ctx: &hcl.EvalContext{Variables: map[string]cty.Value{
		"var":   objectOf(variables),
		"local": objectOf(locals),
	}}}
}

func objectOf(m map[string]cty.Value) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(m)
}

// literalAttr evaluates one attribute to a string, reporting false when it
// depends on anything not statically known.
func (s *scope) literalAttr(body *hclsyntax.Body, name string) (string, bool) {
	attr, ok := body.Attributes[name]
	if !ok {
		return "", false
	}
	v, diags := attr.Expr.Value(s.ctx)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() {
		return "", false
	}
	str, err := convert.Convert(v, cty.String)
	if err != nil || str.IsNull() || !str.IsKnown() {
		return "", false
	}
	out := str.AsString()
	if out == "" {
		return "", false
	}
	return out, true
}

// reference is one traversal found inside a block.
type reference struct {
	// address is the Terraform address referred to, such as
	// "aws_sqs_queue.jobs" or "module.network".
	address string
	// expression is the traversal as written, for the evidence record.
	expression string
	line       int
	// unresolved marks a reference to a value that only exists at apply
	// time, which is reported rather than guessed at.
	unresolved bool
}

// referenceAttrSuffixes mark an attribute whose value locates another
// component.
//
// The list is narrow on purpose. An earlier version matched any attribute
// containing "name" or "id", which in Terraform is very nearly all of them:
// running it over one real module produced over a hundred and seventy
// diagnostics, every one of them describing the normal behavior of a
// reusable module. A diagnostic list that long is not read at all, so it is
// worse than none. Only names that unambiguously point at another component
// qualify.
var referenceAttrSuffixes = []string{
	"_arn", "_url", "_uri", "_endpoint", "_host", "_hostname", "_address",
	"_bucket", "_queue", "_topic", "_table", "_stream", "_broker",
	"_connection_string", "_dsn",
}

// referenceAttrExact covers the same names when they stand alone.
var referenceAttrExact = map[string]bool{
	"arn": true, "url": true, "uri": true, "endpoint": true, "host": true,
	"address": true, "bucket": true, "queue": true, "topic": true,
	"table": true, "connection_string": true, "dsn": true,
}

// references walks every expression in a block, in a fixed order, and returns
// the traversals worth acting on.
func (s *scope) references(block *hclsyntax.Block) []reference {
	var out []reference
	s.walkBody(block.Body, &out)

	// HCL preserves source order for blocks but not for attributes, which
	// live in a map. Sorting makes the emitted sequence reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].line != out[j].line {
			return out[i].line < out[j].line
		}
		return out[i].address < out[j].address
	})
	return out
}

func (s *scope) walkBody(body *hclsyntax.Body, out *[]reference) {
	if body == nil {
		return
	}
	for _, name := range sortedAttrNames(body.Attributes) {
		attr := body.Attributes[name]
		for _, traversal := range attr.Expr.Variables() {
			if ref, ok := s.classify(traversal, name); ok {
				*out = append(*out, ref)
			}
		}
	}
	for _, nested := range body.Blocks {
		s.walkBody(nested.Body, out)
	}
}

// classify turns a traversal into a reference, or reports that it is not one.
func (s *scope) classify(traversal hcl.Traversal, attrName string) (reference, bool) {
	root := traversal.RootName()
	line := traversal.SourceRange().Start.Line
	expression := traversalString(traversal)

	switch root {
	case "var", "local":
		// A value, not a component. It matters only when it sits in an
		// attribute that locates something and cannot be resolved, which is
		// the case the plan calls an unresolved reference.
		if _, resolvable := s.resolve(traversal); resolvable {
			return reference{}, false
		}
		if !isReferenceAttr(attrName) {
			return reference{}, false
		}
		return reference{
			address: expression, expression: expression, line: line, unresolved: true,
		}, true

	case "each", "count", "self", "path", "terraform", "provider":
		// Meta-arguments and interpolation context, never components.
		return reference{}, false

	case "data":
		// data.<type>.<name>
		if len(traversal) < 3 {
			return reference{}, false
		}
		typeName, ok1 := traverseName(traversal[1])
		name, ok2 := traverseName(traversal[2])
		if !ok1 || !ok2 {
			return reference{}, false
		}
		return reference{address: typeName + "." + name, expression: expression, line: line}, true

	case "module":
		if len(traversal) < 2 {
			return reference{}, false
		}
		name, ok := traverseName(traversal[1])
		if !ok {
			return reference{}, false
		}
		return reference{address: "module." + name, expression: expression, line: line}, true

	default:
		// A resource address is "<provider>_<type>.<name>", so the root must
		// contain an underscore and be followed by a name.
		if !strings.Contains(root, "_") || len(traversal) < 2 {
			return reference{}, false
		}
		name, ok := traverseName(traversal[1])
		if !ok {
			return reference{}, false
		}
		return reference{address: root + "." + name, expression: expression, line: line}, true
	}
}

// resolve reports whether a var or local traversal evaluates to a known value.
func (s *scope) resolve(traversal hcl.Traversal) (cty.Value, bool) {
	v, diags := traversal.TraverseAbs(s.ctx)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() {
		return cty.NilVal, false
	}
	return v, true
}

func traverseName(step hcl.Traverser) (string, bool) {
	switch t := step.(type) {
	case hcl.TraverseAttr:
		return t.Name, true
	case hcl.TraverseIndex:
		if t.Key.Type() == cty.String {
			return t.Key.AsString(), true
		}
		return "", false
	default:
		return "", false
	}
}

func traversalString(traversal hcl.Traversal) string {
	var b strings.Builder
	b.WriteString(traversal.RootName())
	for _, step := range traversal[1:] {
		name, ok := traverseName(step)
		if !ok {
			b.WriteString("[*]")
			continue
		}
		b.WriteString(".")
		b.WriteString(name)
	}
	return b.String()
}

func isReferenceAttr(name string) bool {
	lower := strings.ToLower(name)
	if referenceAttrExact[lower] {
		return true
	}
	for _, suffix := range referenceAttrSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func sortedAttrNames(attrs hclsyntax.Attributes) []string {
	out := make([]string, 0, len(attrs))
	for name := range attrs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
