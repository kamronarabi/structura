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
