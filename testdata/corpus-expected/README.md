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

That happened. online-boutique's file did not mention that the frontend reads
`PACKAGING_SERVICE_URL`, and nothing else here would have noticed.

The check turned up seven more the file had not mentioned: every instrumented
service reads `COLLECTOR_SERVICE_ADDR`. Those went under `edges` on the
strength of it, taking recall to 0.71 and blaming unrendered Helm — and that
was wrong in a way worth recording. `helm-chart/values.yaml` sets
`opentelemetryCollector.create: false`, the manifests comment the variables
out, and the collector node exists only because the scan reads an opt-in
Kustomize component. Rendering the chart produces none of the seven. The
application *can* export traces; no deployment described in the repository
receives them.

Which is the lesson: a relationship derived from source says what the code is
able to do, not what any deployment here does. The check is right to demand an
answer and wrong to assume one.

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
