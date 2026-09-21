package terraform_test

import (
	"context"
	"strings"
	"testing"

	"github.com/kamronarabi/structura/internal/resolve"
	"github.com/kamronarabi/structura/internal/scan"
	"github.com/kamronarabi/structura/internal/scan/extractors/terraform"
	"github.com/kamronarabi/structura/pkg/schema"
)

type capture struct {
	nodes   []schema.Node
	edges   []schema.Edge
	hints   []resolve.Hint
	aliases []resolve.Alias
	diags   []schema.Diagnostic
}

func (c *capture) Node(n schema.Node)       { c.nodes = append(c.nodes, n) }
func (c *capture) Edge(e schema.Edge)       { c.edges = append(c.edges, e) }
func (c *capture) Hint(h resolve.Hint)      { c.hints = append(c.hints, h) }
func (c *capture) Alias(a resolve.Alias)    { c.aliases = append(c.aliases, a) }
func (c *capture) Diag(d schema.Diagnostic) { c.diags = append(c.diags, d) }

func (c *capture) byAddress(t *testing.T, address string) schema.Node {
	t.Helper()
	for _, n := range c.nodes {
		if n.Attrs["address"] == address {
			return n
		}
	}
	t.Fatalf("no node with address %q; got %d nodes", address, len(c.nodes))
	return schema.Node{}
}

func (c *capture) edgeBetween(from, to string) *schema.Edge {
	for i := range c.edges {
		if strings.HasSuffix(c.edges[i].From, from) && strings.HasSuffix(c.edges[i].To, to) {
			return &c.edges[i]
		}
	}
	return nil
}

func (c *capture) diagCodes() []string {
	var out []string
	for _, d := range c.diags {
		out = append(out, d.Code)
	}
	return out
}

func (c *capture) hasDiag(code string) bool {
	for _, d := range c.diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func extract(t *testing.T, path, content string) *capture {
	t.Helper()
	c := &capture{}
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}
	ext := ".tf"
	if strings.HasSuffix(name, ".json") {
		ext = ".json"
	}
	f := &scan.File{
		FileMeta: scan.FileMeta{Path: path, Dir: dir, Name: name, Ext: ext, Size: int64(len(content))},
		Content:  []byte(content),
	}
	if err := terraform.New().Extract(context.Background(), f, c); err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	return c
}

func TestMatch(t *testing.T) {
	e := terraform.New()
	if !e.Match(scan.FileMeta{Name: "main.tf", Ext: ".tf"}) {
		t.Error("Match(main.tf) = false")
	}
	if !e.Match(scan.FileMeta{Name: "generated.tf.json", Ext: ".json"}) {
		t.Error("Match(generated.tf.json) = false")
	}
	for _, f := range []scan.FileMeta{
		{Name: "terraform.tfstate", Ext: ".tfstate"},
		{Name: "vars.tfvars", Ext: ".tfvars"},
		{Name: "config.json", Ext: ".json"},
		{Name: "main.go", Ext: ".go"},
	} {
		if e.Match(f) {
			t.Errorf("Match(%q) = true, want false", f.Name)
		}
	}
}

func TestResourceClassification(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_lambda_function" "api"   { function_name = "api" }
resource "aws_db_instance" "orders"    { identifier = "orders" }
resource "aws_sqs_queue" "jobs"        { name = "jobs" }
resource "aws_s3_bucket" "uploads"     { bucket = "uploads" }
resource "aws_lb" "public"             { name = "public" }
resource "aws_vpc" "main"              { cidr_block = "10.0.0.0/16" }
resource "aws_dynamodb_table" "sess"   { name = "sessions" }
resource "aws_sns_topic" "events"      { name = "events" }
`)
	want := map[string]schema.NodeKind{
		"aws_lambda_function.api": schema.KindService,
		"aws_db_instance.orders":  schema.KindDatastore,
		"aws_sqs_queue.jobs":      schema.KindQueue,
		"aws_s3_bucket.uploads":   schema.KindDatastore,
		"aws_lb.public":           schema.KindCloudResource,
		"aws_vpc.main":            schema.KindBoundary,
		"aws_dynamodb_table.sess": schema.KindDatastore,
		"aws_sns_topic.events":    schema.KindQueue,
	}
	for address, kind := range want {
		if got := c.byAddress(t, address).Kind; got != kind {
			t.Errorf("%s is %q, want %q", address, got, kind)
		}
	}
}

// Real Terraform is dominated by plumbing. A large module can declare a
// hundred IAM and networking resources for six things a reader would draw.
func TestPlumbingIsSkippedSilently(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_iam_role" "lambda"                       { name = "r" }
resource "aws_iam_role_policy_attachment" "basic"      { role = "r" }
resource "aws_security_group" "db"                     { name = "sg" }
resource "aws_subnet" "private"                        { cidr_block = "10.0.1.0/24" }
resource "aws_route_table" "private"                   { vpc_id = "x" }
resource "aws_cloudwatch_log_group" "lambda"           { name = "/aws/lambda/x" }
resource "aws_db_subnet_group" "main"                  { name = "g" }
resource "aws_s3_bucket_versioning" "uploads"          { bucket = "b" }
`)
	if len(c.nodes) != 0 {
		t.Errorf("plumbing produced %d nodes, want 0", len(c.nodes))
	}
	// Silently: reporting these would be reporting nearly the whole file.
	if len(c.diags) != 0 {
		t.Errorf("plumbing produced diagnostics: %v", c.diagCodes())
	}
}

func TestUnrecognizedTypesAreReported(t *testing.T) {
	c := extract(t, "main.tf", `resource "aws_quicksight_dashboard" "d" { name = "d" }`)
	if len(c.nodes) != 0 {
		t.Errorf("an unrecognized type produced %d nodes", len(c.nodes))
	}
	// Unlike plumbing, an unrecognized type is a gap the user should know
	// about, so it is reported and the message names the type.
	if !c.hasDiag(terraform.DiagUnrecognizedType) {
		t.Errorf("an unrecognized type was dropped silently; diags: %v", c.diagCodes())
	}
	if c.diags[0].Message != "aws_quicksight_dashboard" {
		t.Errorf("diagnostic message = %q, want the type name for aggregation", c.diags[0].Message)
	}
}

// A reference between two resources is a dependency the author wrote down,
// and needs nothing evaluated to be trustworthy.
func TestReferencesBecomeEdges(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_sqs_queue" "jobs"  { name = "jobs" }
resource "aws_s3_bucket" "files" { bucket = "files" }
resource "aws_db_instance" "db"  { identifier = "db" }

resource "aws_lambda_function" "api" {
  function_name = "api"
  environment {
    variables = {
      QUEUE_URL   = aws_sqs_queue.jobs.url
      BUCKET      = aws_s3_bucket.files.id
      DB_ENDPOINT = aws_db_instance.db.address
    }
  }
}
`)
	tests := []struct {
		to   string
		kind schema.EdgeKind
	}{
		{"aws_sqs_queue.jobs", schema.EdgePublishesTo},
		{"aws_s3_bucket.files", schema.EdgePersistsTo},
		{"aws_db_instance.db", schema.EdgePersistsTo},
	}
	for _, tt := range tests {
		e := c.edgeBetween("aws_lambda_function.api", tt.to)
		if e == nil {
			t.Errorf("no edge to %s", tt.to)
			continue
		}
		if e.Kind != tt.kind {
			t.Errorf("edge to %s is %q, want %q", tt.to, e.Kind, tt.kind)
		}
		if e.Confidence != schema.ConfReference {
			t.Errorf("edge to %s has confidence %v, want %v", tt.to, e.Confidence, schema.ConfReference)
		}
		if len(e.Evidence) == 0 || e.Evidence[0].Line == 0 {
			t.Errorf("edge to %s has no located evidence", tt.to)
		}
	}
}

// Terraform treats a whole directory as one module, so a reference to a
// resource in a sibling file is normal. An extractor may only emit an edge it
// can justify from the file in front of it.
func TestCrossFileReferencesBecomeHints(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_lambda_function" "api" {
  function_name = "api"
  environment {
    variables = { DB = aws_db_instance.declared_elsewhere.address }
  }
}
`)
	if len(c.edges) != 0 {
		t.Errorf("emitted %d edges to a resource this file never declared", len(c.edges))
	}
	if len(c.hints) != 1 {
		t.Fatalf("got %d hints, want 1", len(c.hints))
	}
	h := c.hints[0]
	if h.Kind != resolve.HintTraversal {
		t.Errorf("hint kind = %q, want traversal", h.Kind)
	}
	if h.Tokens[0] != "aws_db_instance.declared_elsewhere" {
		t.Errorf("hint token = %v", h.Tokens)
	}
	if h.SuggestedEdge != schema.EdgePersistsTo {
		t.Errorf("suggested edge = %q, want persists_to", h.SuggestedEdge)
	}
}

func TestVariableDefaultsResolve(t *testing.T) {
	c := extract(t, "main.tf", `
variable "env" {
  type    = string
  default = "prod"
}

resource "aws_sqs_queue" "jobs" {
  name = "orders-${var.env}"
}
`)
	if got := c.byAddress(t, "aws_sqs_queue.jobs").Name; got != "orders-prod" {
		t.Errorf("name = %q, want the default substituted", got)
	}
}

// A local built from another local is ordinary Terraform. Refusing to follow
// it would report a value the file fully determines as unknowable.
func TestLocalsResolveThroughEachOther(t *testing.T) {
	c := extract(t, "main.tf", `
variable "region" {
  default = "us-east-1"
}

locals {
  prefix = "orders-${var.region}"
  queue  = "${local.prefix}-jobs"
  deep   = "${local.queue}-dlq"
}

resource "aws_sqs_queue" "jobs" { name = local.queue }
resource "aws_sqs_queue" "dlq"  { name = local.deep }
`)
	if got := c.byAddress(t, "aws_sqs_queue.jobs").Name; got != "orders-us-east-1-jobs" {
		t.Errorf("name = %q, want the chained locals resolved", got)
	}
	if got := c.byAddress(t, "aws_sqs_queue.dlq").Name; got != "orders-us-east-1-jobs-dlq" {
		t.Errorf("name = %q, want three levels of locals resolved", got)
	}
	if c.hasDiag(terraform.DiagUnresolvedReference) {
		t.Errorf("resolvable locals were reported as unknowable: %v", c.diagCodes())
	}
}

func TestCyclicLocalsTerminate(t *testing.T) {
	// Terraform would reject this, but the parser must not hang on it.
	c := extract(t, "main.tf", `
locals {
  a = "${local.b}-x"
  b = "${local.a}-y"
}

resource "aws_sqs_queue" "q" { name = local.a }
`)
	if len(c.nodes) != 1 {
		t.Errorf("got %d nodes, want the queue to still be emitted", len(c.nodes))
	}
}

// A variable with no default is supplied at apply time; there is nothing here
// to resolve it to, and guessing would produce a confident wrong answer.
func TestUnresolvableValuesAreReportedNotGuessed(t *testing.T) {
	c := extract(t, "main.tf", `
variable "search_endpoint" {
  type = string
}

resource "aws_lambda_function" "api" {
  function_name = "api"
  environment {
    variables = { SEARCH_HOST = var.search_endpoint }
  }
}
`)
	if len(c.edges) != 0 || len(c.hints) != 0 {
		t.Errorf("an apply-time value produced %d edges and %d hints; it should produce neither",
			len(c.edges), len(c.hints))
	}
}

// An earlier version matched any attribute containing "name" or "id", which
// in Terraform is nearly all of them: one real module produced over a hundred
// and seventy diagnostics describing normal module behavior.
func TestOrdinaryVariablesDoNotProduceDiagnostics(t *testing.T) {
	c := extract(t, "main.tf", `
variable "tags"        { type = map(string) }
variable "vpc_id"      { type = string }
variable "subnet_ids"  { type = list(string) }
variable "name"        { type = string }
variable "instance_class" { type = string }

resource "aws_db_instance" "db" {
  identifier     = var.name
  instance_class = var.instance_class
  vpc_id         = var.vpc_id
  subnet_ids     = var.subnet_ids
  tags           = var.tags
}
`)
	if c.hasDiag(terraform.DiagUnresolvedReference) {
		t.Errorf("ordinary module parameters were reported as gaps: %v", c.diagCodes())
	}
}

func TestDataSourcesAreMarkedUnmanaged(t *testing.T) {
	c := extract(t, "main.tf", `data "aws_s3_bucket" "shared" { bucket = "acme-shared" }`)
	n := c.byAddress(t, "data.aws_s3_bucket.shared")
	if n.Kind != schema.KindDatastore {
		t.Errorf("kind = %q, want datastore", n.Kind)
	}
	// It exists but is managed somewhere else, and saying which is which
	// matters to anyone reading the graph.
	if n.Attrs["managed"] != false {
		t.Errorf("managed = %v, want false", n.Attrs["managed"])
	}
}

func TestCountAndForEachAreNotExpanded(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_db_instance" "a" {
  identifier = "a"
  count      = 3
}
resource "aws_dynamodb_table" "b" {
  name     = "b"
  for_each = toset(["x", "y"])
}
`)
	if got := c.byAddress(t, "aws_db_instance.a").Attrs["multiplicity"]; got != "count" {
		t.Errorf("multiplicity = %v, want count", got)
	}
	if got := c.byAddress(t, "aws_dynamodb_table.b").Attrs["multiplicity"]; got != "for_each" {
		t.Errorf("multiplicity = %v, want for_each", got)
	}
	// Recording the multiplicity is honest; inventing an instance count is
	// not, so exactly one node is emitted per resource.
	if len(c.nodes) != 2 {
		t.Errorf("got %d nodes, want 2", len(c.nodes))
	}
	if !c.hasDiag(terraform.DiagUnexpanded) {
		t.Error("unexpanded resources were not reported")
	}
}

func TestExternalModulesAreReported(t *testing.T) {
	c := extract(t, "main.tf", `
module "network" {
  source = "terraform-aws-modules/vpc/aws"
  name   = "main"
}

module "local_thing" {
  source = "./modules/thing"
}
`)
	var external, local *schema.Node
	for i := range c.nodes {
		switch c.nodes[i].Name {
		case "network":
			external = &c.nodes[i]
		case "local_thing":
			local = &c.nodes[i]
		}
	}
	if external == nil || local == nil {
		t.Fatalf("expected both module nodes, got %d", len(c.nodes))
	}
	if external.Attrs["external"] != true {
		t.Errorf("a registry module was not marked external")
	}
	if _, marked := local.Attrs["external"]; marked {
		t.Errorf("a local module was marked external")
	}
	if !c.hasDiag("terraform_external_module") {
		t.Error("a module whose code is outside the repository was not reported")
	}
}

func TestMalformedHCLIsReported(t *testing.T) {
	c := extract(t, "main.tf", `resource "aws_sqs_queue" "jobs" { name = `)
	if !c.hasDiag("terraform_parse_failed") {
		t.Errorf("malformed HCL produced no diagnostic: %v", c.diagCodes())
	}
	if len(c.nodes) != 0 {
		t.Error("malformed HCL produced nodes")
	}
}

func TestJSONSyntaxIsReportedNotParsed(t *testing.T) {
	c := extract(t, "generated.tf.json", `{"resource": {"aws_sqs_queue": {"jobs": {}}}}`)
	if !c.hasDiag(terraform.DiagJSONSyntax) {
		t.Error("Terraform JSON syntax was skipped without saying so")
	}
}

func TestOutputIsOrderIndependent(t *testing.T) {
	// HCL preserves block order but attributes live in a map, so without
	// explicit sorting the emitted sequence would vary between runs.
	src := `
resource "aws_sqs_queue" "jobs"  { name = "jobs" }
resource "aws_s3_bucket" "files" { bucket = "files" }
resource "aws_lambda_function" "api" {
  function_name = "api"
  environment {
    variables = {
      Z_QUEUE  = aws_sqs_queue.jobs.url
      A_BUCKET = aws_s3_bucket.files.id
    }
  }
}
`
	first := extract(t, "main.tf", src)
	baseline := edgeOrder(first)
	for range 25 {
		if got := edgeOrder(extract(t, "main.tf", src)); got != baseline {
			t.Fatalf("edge order varied between runs:\n %s\n %s", baseline, got)
		}
	}
}

func edgeOrder(c *capture) string {
	parts := make([]string, len(c.edges))
	for i, e := range c.edges {
		parts[i] = e.From + "->" + e.To
	}
	return strings.Join(parts, ",")
}

// HCL comes from files in a repository being scanned; it must never panic.
func FuzzExtract(f *testing.F) {
	f.Add(`resource "aws_sqs_queue" "jobs" { name = "jobs" }`)
	f.Add(`locals { a = "${local.a}" }`)
	f.Add(`variable "x" { default = 1 }`)
	f.Add("resource {")
	// Empty labels parse but are not valid Terraform; they once reached the
	// graph as a node with no name.
	f.Add(`resource "aws_sqs_queue" "" {}`)
	f.Add(`module "" { source = "./x" }`)
	f.Add("")
	f.Add(strings.Repeat(`resource "a_b" "c" {}`+"\n", 50))

	f.Fuzz(func(t *testing.T, content string) {
		c := &capture{}
		file := &scan.File{
			FileMeta: scan.FileMeta{Path: "main.tf", Name: "main.tf", Ext: ".tf", Size: int64(len(content))},
			Content:  []byte(content),
		}
		if err := terraform.New().Extract(context.Background(), file, c); err != nil {
			t.Fatalf("Extract() = %v", err)
		}
		for _, n := range c.nodes {
			if err := n.Validate(); err != nil {
				t.Fatalf("emitted an invalid node: %v", err)
			}
		}
		for _, e := range c.edges {
			if err := e.Validate(); err != nil {
				t.Fatalf("emitted an invalid edge: %v", err)
			}
		}
	})
}

// Terraform requires every block label to be an identifier. HCL parses an
// empty one anyway, and a resource named "" used to become a node with no
// name, which the schema rejects downstream. Skipping the block matches how
// a block with the wrong number of labels is already handled.
func TestEmptyBlockLabelsAreSkipped(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"resource with empty name", `resource "aws_sqs_queue" "" {}`},
		{"resource with empty type", `resource "" "jobs" {}`},
		{"data with empty name", `data "aws_sqs_queue" "" {}`},
		{"module with empty name", `module "" { source = "./vpc" }`},
		{"variable with empty name", `variable "" { default = "x" }`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := extract(t, "main.tf", tt.src)
			if len(c.nodes) != 0 {
				t.Errorf("emitted %d node(s) for a block with an empty label: %+v", len(c.nodes), c.nodes)
			}
			for _, n := range c.nodes {
				if err := n.Validate(); err != nil {
					t.Errorf("emitted an invalid node: %v", err)
				}
			}
		})
	}
}

// A valid resource declared alongside a malformed one still lands in the
// graph: one bad block does not abort the file.
func TestEmptyLabelDoesNotSuppressValidSiblings(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_sqs_queue" "" {}
resource "aws_sqs_queue" "jobs" { name = "jobs" }
`)
	if len(c.nodes) != 1 {
		t.Fatalf("got %d nodes, want 1: %+v", len(c.nodes), c.nodes)
	}
	if c.nodes[0].Name != "jobs" {
		t.Errorf("node name = %q, want %q", c.nodes[0].Name, "jobs")
	}
}

// A reference always runs from the member to its boundary -- a resource names
// the VPC or cluster it sits in, never the other way round -- so containment
// derived from one has to be written in the opposite direction. Emitted as
// read, it produced edges claiming a Lambda contains a VPC.
func TestContainmentRunsFromTheBoundary(t *testing.T) {
	c := extract(t, "main.tf", `
resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
}

resource "aws_ecs_cluster" "app" {
  name = "app"
}

resource "aws_lambda_function" "api" {
  function_name = "api"
  vpc_config {
    subnet_ids = [aws_vpc.main.id]
  }
}

resource "aws_ecs_service" "worker" {
  name    = "worker"
  cluster = aws_ecs_cluster.app.id
}
`)

	for _, tc := range []struct{ boundary, member string }{
		{"aws_vpc.main", "aws_lambda_function.api"},
		{"aws_ecs_cluster.app", "aws_ecs_service.worker"},
	} {
		e := c.edgeBetween(tc.boundary, tc.member)
		if e == nil {
			t.Fatalf("no edge from %s to %s; containment runs from the container", tc.boundary, tc.member)
		}
		if e.Kind != schema.EdgeContains {
			t.Errorf("%s -> %s is %s, want contains", tc.boundary, tc.member, e.Kind)
		}
		if back := c.edgeBetween(tc.member, tc.boundary); back != nil && back.Kind == schema.EdgeContains {
			t.Errorf("%s claims to contain %s", tc.member, tc.boundary)
		}
		// The reference is still what justifies the edge, so the evidence
		// has to name it even though the edge points the other way.
		if len(e.Evidence) == 0 || !strings.Contains(e.Evidence[0].Detail, tc.member) {
			t.Errorf("evidence %+v does not name the reference it came from", e.Evidence)
		}
	}
}

// A module call groups nothing this graph can see: the resources it
// instantiates live in another directory under their own namespace, and a
// module reused by several callers could not sit inside all of them at once.
// Calling it a boundary excluded it from the connected count and from
// resolution, which is how a repository of module calls reported no
// relationships at all.
func TestAModuleCallIsAContainerNotABoundary(t *testing.T) {
	c := extract(t, "main.tf", `
module "vpc" {
  source = "../.."
}

module "security_group" {
  source = "terraform-aws-modules/security-group/aws"
  vpc_id = module.vpc.vpc_id
}
`)

	for _, name := range []string{"vpc", "security_group"} {
		var found *schema.Node
		for i := range c.nodes {
			if c.nodes[i].Name == name {
				found = &c.nodes[i]
			}
		}
		if found == nil {
			t.Fatalf("no node for module %q", name)
		}
		if found.Kind == schema.KindBoundary {
			t.Errorf("module %q is a boundary, but contains nothing", name)
		}
		if found.Layer != schema.LayerContainer {
			t.Errorf("module %q is at layer %s, want container", name, found.Layer)
		}
		if !strings.HasPrefix(found.ID, string(schema.KindCloudResource)+":") {
			t.Errorf("module %q has id %s, which disagrees with its kind", name, found.ID)
		}
	}

	// The reference between two module calls still has to resolve, which it
	// can only do if collectDeclarations builds the same ID emitModule does.
	if e := c.edgeBetween("module.security_group", "module.vpc"); e == nil {
		t.Error("no edge from security_group to vpc; the module id changed in one place only")
	}
}
