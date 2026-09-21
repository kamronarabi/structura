# Expected edges

The resolver is the one part of this project with no mechanical oracle.

"Correct" for an extractor is checkable: this YAML declares a Deployment named
`api`, so there should be a node named `api`. "Correct" for a resolver means
*the edges a competent engineer would draw after reading the repository*, and
nothing in the repository states that.

So these files are written by hand, from each project's own documentation and
architecture diagrams, before looking at what Structura produced. That order
matters: a file written from the output measures nothing except that the
output is stable.

## The expectations are checked, not trusted

The paragraph above is a claim about a process, and a process claim is exactly
what its author cannot verify about themselves. The bias it invites is not
inventing edges — it is scoping: writing down the relationships the tool can
see and omitting the ones it cannot, which turns recall into a measurement of
the author's imagination.

That happened. online-boutique reported **recall 1.00** while missing seven
real relationships, because this file did not mention them. Every instrumented
service exports traces to the OpenTelemetry collector, declared in
`helm-chart/templates/*.yaml` behind a conditional. Structura reads those
files but does not render them, so it finds none of the seven — and the
expectation file, written by the same person who knew that, quietly left them
out. The collector sat in the graph as an isolated node with no explanation.

So where a repository ships application source, the same relationships are
derived from it mechanically and compared against this file
(`internal/resolve/oracle_test.go`). A twelve-factor service names its
dependencies in environment variables: configuration supplies them, the source
consumes them. Two independent statements of one fact, in different files and
different languages, and Structura reads only one of them. An edge the source
states and this file does not mention fails the test.

It is not a complete answer. sock-shop ships no application source, and
podinfo is one program rather than a set of services, so neither is checked
this way — the test says so out loud rather than implying coverage it does not
have. The `oracle:` block in each file configures it, and the aliases it
declares are small, checkable claims about the repository rather than
hand-written edges.

Each file has two lists:

- `edges` — relationships that should be found. Misses count against recall.
- `allowed` — relationships that are correct but not required, usually
  infrastructure plumbing outside the application architecture. Finding one
  costs nothing; missing one costs nothing.

Anything found that appears in neither list is a false positive and counts
against precision.

**Precision is gated, recall is tracked.** A missing edge is a visible gap:
the diagram looks sparse and the user notices. A wrong edge is invisible and
actively harmful — it reaches the MCP server, the model reasons on top of it,
and the Phase 3 profiler gives confident advice about a dependency that does
not exist.
