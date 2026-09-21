# Structura

**Map your architecture from the configs you already have.**

Structura scans a repository's infrastructure and dependency manifests — Docker
Compose, Kubernetes, Helm, Terraform, `.env`, `go.mod`, `package.json`,
`requirements.txt` — and builds a **Structura Architecture Graph (SAG)**: the services, datastores,
queues, and external systems in your stack, plus the inferred edges between them.

It then serves that graph to AI coding agents over the Model Context Protocol,
so a model can answer *"what does the checkout service depend on, and what
breaks if Postgres goes down?"* without reading a single source file.

```sh
structura scan                            # → .structura/graph.json
structura install-mcp --client claude-code
```

## Why a graph instead of just reading the code

Asking a model to infer architecture by reading source burns enormous context
for a low-fidelity answer, and it has to re-derive the same picture every
session. Structura does the extraction once, deterministically, and hands the
model a queryable index — overview, node lookup, path tracing, blast radius,
and the gaps in its own coverage — with each tool response held to a token
budget.

## Status

Phase 1 (the local wedge: `scan`, `mcp`, `install-mcp`) is under active
development. Nothing is released yet.

| Milestone | |
|---|---|
| M0 Foundation | ✅ |
| M1 Graph core | ✅ |
| M2 Extractor framework + Compose | ✅ |
| M3 Kubernetes + manifests | ✅ |
| M4 Terraform | ✅ |
| M5 Resolver | ✅ |
| M6 MCP server | ✅ |
| M7 Release | 🔜 |

`structura scan` works today against Docker Compose, Kubernetes, Helm,
Terraform, `.env` files, and language manifests, and infers the relationships
between what it finds. `structura mcp` serves the result to an editor, and
`structura install-mcp` wires it up. What is left is shipping binaries.

Resolver quality is measured against real repositories rather than asserted
(`make corpus && make corpus-test`):

| Repository | Precision | Recall |
|---|---|---|
| GoogleCloudPlatform/microservices-demo | 1.00 | 0.71 |
| stefanprodan/podinfo | 1.00 | 1.00 |
| Weaveworks Sock Shop | 1.00 | 0.07 |

Precision is gated in CI; recall is reported. A missing edge is a visible gap —
the diagram looks sparse and you notice. A wrong edge is invisible and actively
harmful, because a model reasons on top of it.

The expected edges are hand-written, so they are themselves checked against the
application source rather than trusted: a service names its dependencies in
environment variables, configuration supplies them, and the source consumes
them — two statements of one fact, and Structura reads only one of them. That
check is what corrected the microservices-demo figure. It had read 1.00 while
missing seven real edges, because the expectations had been scoped, without
anyone meaning to, to what the scanner could already see.

The two shortfalls are different kinds of gap, and only one of them is ours:

- **microservices-demo, 0.71.** Every instrumented service exports traces to an
  OpenTelemetry collector, declared in `helm-chart/templates/` behind a
  conditional. Structura reads those files but does not render Helm, so it
  finds none of the seven. Rendering would close it, and the scan already says
  so — `helm_unrendered` is in the diagnostics.
- **Sock Shop, 0.07.** This is the honest counterexample, kept in the corpus
  for that reason. The repository is a deployment repository: its manifests
  declare the workloads but almost no addresses between them, and it contains
  no application source at all, since each service lives in its own repository.
  So the relationships are not stated anywhere Structura is looking. Phase 2's
  source extractors would not close this one either — there is nothing here for
  them to read. Scanning the service repositories would.

## Every edge shows its work

Configuration files describe *what exists*; they rarely say *what calls what*.
No field in a Kubernetes manifest states that `api-gateway` calls
`user-service` — that has to be inferred by resolving names across artifacts.

So inference is explicit rather than hidden. Every edge carries a `confidence`
score and an `evidence[]` array naming the rule that fired and the file and line
it fired on:

```json
{
  "from": "service:k8s/prod/api-gateway",
  "to": "datastore:k8s/prod/session-cache",
  "kind": "persists_to",
  "confidence": 0.80,
  "evidence": [{
    "extractor": "k8s",
    "rule": "dns_exact",
    "path": "deploy/prod/api-gateway.yaml",
    "line": 4,
    "detail": "container \"api\" env REDIS_URL=redis://session-cache:6379/0 (resolves to session-cache)"
  }]
}
```

Shipping imperfect inference is fine. Shipping imperfect inference that
presents itself as certain is not — especially when a model is about to reason
on top of it.

## Credentials never reach the graph

`graph.json` gets committed to repos and pasted into LLM context, so anything
matching a credential pattern is masked before it touches disk:
`postgres://***:***@db:5432/orders`. This is a tested invariant with its own CI
gate, not a best effort.

## The graph format is versioned separately from the CLI

`graph.json` carries a `schemaVersion` that moves independently of the binary,
because the file gets committed and is then read by builds that are not the
one that wrote it. A minor bump is additive and only additive — new optional
fields, new node or edge kinds — so a reader built for an earlier minor still
understands everything it recognizes. Anything that removes a field, retypes
one, or changes what an existing kind means is a major bump. Those rules hold
at `0.x` too, where SemVer would permit otherwise.

A build reading a graph from a *newer* schema serves it as it is rather than
rescanning over it, since rewriting it would delete whatever that build cannot
represent and land the deletion in someone's history looking like an
architecture change. `pkg/schema/compat.go` is the full statement.

## Wiring it into an editor

```sh
structura install-mcp --client claude-code   # or cursor, windsurf, vscode
structura install-mcp --list                 # where each client's config lives
```

The config being edited belongs to the user and was not written by us, so the
existing file is parsed and merged rather than templated over: servers
configured elsewhere are preserved byte for byte and in their original order,
the original is copied to `<config>.structura.bak` before anything is written,
and re-running the command changes nothing. `--dry-run` prints the exact diff
first. A config that is not valid JSON — a trailing comma, a `//` comment — is
refused rather than replaced, because a file we cannot parse is a file whose
contents we cannot preserve.

## Development

Requires Go (see `go.mod`), plus `golangci-lint` and `goreleaser` for the full
check suite.

```sh
make            # tidy + lint + cgo-free + race tests + determinism
make test       # fast unit tests
make build      # → bin/structura
make demo       # scan a fixture repo and print the result
make eval       # ask a model 18 questions through the MCP tools (needs an API key)
make help       # all targets
```

It last scored **15/15 on `claude-opus-5`, at 4.2 tool calls per question** —
on the fifteen questions that existed at the time. Three have been added since,
and the tool surface has gained a tool and changed several response formats, so
that figure describes a version of the product that no longer exists. It is
reported rather than removed because a stale measurement someone can date is
more useful than none, but it is not a current result and the suite needs
re-running before the release.

`make eval` is the only check that is deliberately outside CI. It calls the
API, so it costs money and is nondeterministic, and a flaky gate gets disabled
until it stops being run at all. It is also the only thing here that tests the
product rather than the parts: a graph can be structurally perfect and still
useless if the tool surface makes a model take six calls to learn what one
should have told it. Run it per milestone and before each release.

Two invariants are enforced in CI and worth knowing before you contribute:

- **Nothing may pull in CGO.** `CGO_ENABLED=0` everywhere; a `runtime/cgo`
  dependency fails the build. Static binaries on five platforms is the
  distribution promise, and it is easy to break by accident.
- **Nothing outside `internal/cli` may write to stdout.** The MCP server speaks
  JSON-RPC over stdio; a stray `fmt.Println` corrupts the transport and surfaces
  as an opaque parse error in the user's IDE. A lint rule blocks it.

## License

[Apache 2.0](LICENSE).
