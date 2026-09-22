# Structura

**Map your architecture from the configs you already have.**

Structura reads the infrastructure and dependency manifests already in your
repository — Compose files, Kubernetes manifests, Helm charts, Terraform,
Dockerfiles, `.env` files, `go.mod`, `package.json` — and builds a graph of
the services, datastores, queues, and third-party systems in your stack, plus
the relationships between them.

It then serves that graph to AI coding agents over the Model Context Protocol,
so a model can answer *"what does checkout depend on, and what breaks if
Postgres goes down?"* without reading a single source file.

Nothing is generated that isn't already true in your repository. Every
relationship it infers carries the rule that produced it and the file and line
it came from.

> **Status: pre-release.** All the functionality below works; `v0.1.0` has not
> been tagged, so the only way to install it today is from source. See
> [Install](#install).

---

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [What it reads](#what-it-reads)
- [What you get](#what-you-get)
- [Using it with an AI agent](#using-it-with-an-ai-agent)
- [Every edge shows its work](#every-edge-shows-its-work)
- [Credentials never reach the graph](#credentials-never-reach-the-graph)
- [How accurate is it](#how-accurate-is-it)
- [What it does not do](#what-it-does-not-do)
- [Configuration](#configuration)
- [The graph format](#the-graph-format)
- [Development](#development)

---

## Install

### From source

Requires Go (see `go.mod`). Works on macOS, Linux, and Windows.

```sh
go install github.com/kamronarabi/structura/cmd/structura@main
```

Or clone and build:

```sh
git clone https://github.com/kamronarabi/structura
cd structura
make build          # → bin/structura
```

### From `v0.1.0` onward

One command, macOS and Linux, no package manager required:

```sh
curl -fsSL https://raw.githubusercontent.com/kamronarabi/structura/main/install.sh | sh
```

It works out which build you need, checks the download against the published
checksums before installing anything, and puts the binary in `/usr/local/bin`
if that is writable or `~/.local/bin` if not. It never runs `sudo` on your
behalf. To pin a version or choose the directory yourself:

```sh
STRUCTURA_VERSION=v0.1.0 STRUCTURA_INSTALL_DIR=~/bin   sh -c "$(curl -fsSL https://raw.githubusercontent.com/kamronarabi/structura/main/install.sh)"
```

If you would rather not pipe a script into a shell — a reasonable position —
every release also publishes signed, static binaries with checksums and an
SBOM:

| Platform | |
|---|---|
| macOS | `brew install kamronarabi/tap/structura` |
| Debian / Ubuntu | `.deb` from the releases page |
| Fedora / RHEL | `.rpm` |
| Alpine | `.apk` |
| Anything else | `.tar.gz` — a single binary, nothing else to install |

Builds exist for macOS (Intel, Apple silicon, and a universal binary), Linux
(x86-64, arm64, armv7), and Windows (x86-64, arm64).

There is nothing to configure, no daemon, and no runtime to install. The
binary is built with cgo disabled, which on Linux means it is fully statically
linked and runs on any distribution including Alpine; on macOS it still links
the system libraries every Go binary does.

## Quick start

```sh
cd your-repo
structura scan --print
```

That writes `.structura/graph.json` and prints what it found. Then wire it
into your editor:

```sh
structura install-mcp --client claude-code
```

Restart the editor and ask it about your architecture.

Useful flags:

```sh
structura scan --print              # summarize the result in the terminal
structura scan -o -                 # write the graph to stdout instead of a file
structura scan --debug-dump         # show raw extractor output, before inference
structura scan -q                   # write the graph, print nothing
structura install-mcp --list        # show where each editor keeps its config
structura install-mcp --dry-run     # print the exact config diff, change nothing
```

`.structura/graph.json` is deterministic — two scans of an unchanged tree
produce byte-identical output — so it is safe to commit and meaningful to
diff.

## What it reads

| Source | Files | What it contributes |
|---|---|---|
| Docker Compose | `docker-compose.yml`, `compose.yaml`, and `.prod`/`.override` variants | services, `depends_on`, volumes, environment |
| Kubernetes | any other `.yaml` / `.yml` | Deployments, StatefulSets, Services, Ingresses, ConfigMaps, namespaces, label selectors |
| Helm | `Chart.yaml`, `values.yaml`, `values-*.yaml` | chart metadata, subchart dependencies, and the workloads an umbrella chart declares |
| Terraform | `*.tf`, `*.tf.json` | resources, modules, variables, and reference edges between them |
| Dockerfile | `Dockerfile`, `Containerfile`, `*.dockerfile` | base image, runtime, exposed ports |
| Environment | `.env`, `.env.*`, `env.example` | connection strings and service addresses |
| Language manifests | `go.mod`, `package.json`, `requirements*.txt`, `pyproject.toml` | language, framework, and the managed services a dependency implies |

Kustomize overlays and Helm templates are detected but not rendered. The scan
says so in its diagnostics rather than pretending the result is complete — see
[What it does not do](#what-it-does-not-do).

## What you get

```
$ structura scan --print
.structura/graph.json: 9 nodes, 12 edges, 3 diagnostics (4 files scanned, 4 parsed, 6ms)

Components
  service         api-gateway                ghcr.io/acme/api-gateway:1.2.0
  service         checkout                   ghcr.io/acme/checkout:2.0.1
  datastore       orders-db                  postgres:16-alpine
  datastore       session-cache              redis:7-alpine
  external        analytics.example.com
  external        api.stripe.com
  external        warehouse.legacy.internal
  cloud_resource  public
  boundary        prod

Relationships
  api-gateway  calls        checkout               (0.80)
  api-gateway  persists_to  session-cache          (0.80)
  api-gateway  calls        analytics.example.com  (0.70)
  checkout     persists_to  orders-db              (0.80)
  checkout     calls        api.stripe.com         (0.80)
  public       exposes      api-gateway            (0.95)
  public       exposes      checkout               (0.95)

Diagnostics
  info  unresolved_reference         deploy/prod/api-gateway.yaml:4
        "api-gateway.prod" refers to "search-service", which matches no component in this repository
```

Components are typed rather than uniform: `postgres:16-alpine` is recognized
as a datastore, `api.stripe.com` as a third-party system you call. That is
what makes the result read like an architecture instead of a list of boxes.

The number beside each relationship is how much the evidence supports it.
Nothing is hidden behind a threshold — a weak inference is shown *and marked
weak*.

## Using it with an AI agent

```sh
structura install-mcp --client claude-code   # or cursor, windsurf, vscode
```

The editor config being edited belongs to you and was not written by us, so
the existing file is parsed and merged rather than templated over. Servers you
configured elsewhere are preserved byte for byte and in their original order,
the original is copied to `<config>.structura.bak` first, and re-running the
command changes nothing. A config that is not valid JSON — a trailing comma, a
`//` comment — is refused rather than replaced, because a file we cannot parse
is a file whose contents we cannot preserve.

The model then gets six tools:

| Tool | Answers |
|---|---|
| `structura_overview` | What is in this repository, and is it a system or an inventory? |
| `structura_list_nodes` | Which components match this kind, layer, or namespace? |
| `structura_describe_node` | What does this component connect to, in both directions? |
| `structura_trace_path` | How does A reach B, and how well evidenced is each route? |
| `structura_impact_of` | What breaks if this goes down? |
| `structura_diagnostics` | What could the scan not read? |

Plus an `architecture-graph` resource for the whole graph.

Every response is held to a token budget, and a truncated one says how much it
left out rather than trailing off. The server rescans automatically when a
manifest changes underneath it.

### Why a graph instead of just reading the code

Asking a model to infer architecture by reading source burns enormous context
for a low-fidelity answer, and it re-derives the same picture every session.
Structura does the extraction once, deterministically, and hands the model a
queryable index — including an honest account of its own blind spots.

## Every edge shows its work

Configuration files describe *what exists*; they rarely say *what calls what*.
No field in a Kubernetes manifest states that `api-gateway` calls
`checkout` — that has to be inferred by resolving names across artifacts.

So inference is explicit rather than hidden. Every edge carries a `confidence`
score and an `evidence[]` array naming the rule that fired and the file and
line it fired on:

```json
{
  "id": "e:70ea54e717f8",
  "from": "container:@prod/api-gateway",
  "to": "container:@prod/session-cache",
  "kind": "persists_to",
  "protocol": "redis",
  "confidence": 0.8,
  "evidence": [
    {
      "extractor": "k8s",
      "path": "deploy/prod/api-gateway.yaml",
      "line": 4,
      "rule": "dns_exact",
      "detail": "container \"api\" env REDIS_URL=redis://session-cache:6379/0 (resolves to session-cache)"
    }
  ]
}
```

An identifier says what a component is called and where it lives —
`container:@prod/api-gateway` — and nothing about which file or extractor
found it, so two readings of one component share an identifier instead of
becoming two boxes.

Shipping imperfect inference is fine. Shipping imperfect inference that
presents itself as certain is not — especially when a model is about to reason
on top of it.

Read the score as a class, not a measurement: `1.00` is declared outright,
`0.95` resolved from a direct reference, `0.80` matched on a hostname, `0.70`
reached through an indirection, `0.40` a naming convention and nothing else.
The numbers rank evidence; they are not observed frequencies. The evidence
array has the specifics.

## Credentials never reach the graph

`graph.json` gets committed and pasted into model context, so anything
matching a credential pattern is masked before it is written:
`postgres://***:***@db:5432/orders`. The same masking applies to every other
output path, including `--debug-dump`.

This is a tested invariant with its own CI gate, not a best effort.

## How accurate is it

Quality is measured against real repositories rather than asserted
(`make corpus && make corpus-test`). Seven are pinned by commit; three have
hand-written expectation files and are scored:

| Repository | Precision | Recall |
|---|---|---|
| GoogleCloudPlatform/microservices-demo | 1.00 | 1.00 |
| stefanprodan/podinfo | 1.00 | 1.00 |
| Weaveworks Sock Shop | 1.00 | 0.07 |

**Precision is gated in CI; recall is reported.** A missing edge is a visible
gap — the diagram looks sparse and you notice. A wrong edge is invisible and
actively harmful, because a model reasons on top of it.

The expectations are hand-written, so they are themselves checked against the
application source rather than trusted: a service names its dependencies in
environment variables, configuration supplies them, and the source consumes
them — two statements of one fact, and Structura reads only one of them. The
check reads the other, and every relationship it derives has to be accounted
for.

The remaining four repositories have no architecture to get right — a
grab-bag of Kubernetes examples, a hundred unrelated Compose stacks, a
Terraform module library, and `bitnami/charts`, which yields 241 nodes and
eight real relationships from 4,122 files. Scoring them would measure nothing,
so instead each has the shape of its result pinned: how much was found, of
what kinds, by which rules, and what could not be read. That is stability, not
correctness, and the distinction is deliberate.

### Does the graph actually help a model

The corpus above measures the graph. This measures the product: eighteen
architecture questions asked through the MCP tools, scored on whether the
answer contains the facts a correct answer needs, and on how many tool calls
it took to get there.

```
score       18/18
tool calls  70 total, 3.9 per question
```

Against `claude-opus-5` on 2026-09-22 (`make eval`). A graph can be
structurally perfect and still useless if the tool surface makes a model take
six calls to learn what one should have told it, so the call count is part of
the result rather than a footnote to it.

## What it does not do

Worth knowing before you try it:

- **Helm templates and Kustomize overlays are not rendered.** Charts are read
  through `Chart.yaml` and `values.yaml`, which is enough for an umbrella
  chart that names its workloads there, but wiring expressed only inside
  `templates/` is not seen. Rendering was prototyped and measured: on the
  corpus it recovered no relationships that plain manifests did not already
  provide, at roughly 3.5× the binary size, and 24 of 25 bitnami charts could
  not render offline at all.
- **It does not read your application source.** If a service hardcodes an
  address in code rather than declaring it in config, Structura will not see
  it. Sock Shop's 0.07 recall above is exactly this: a deployment repository
  whose manifests declare workloads and almost no addresses between them.
  Reading source is Phase 2.
- **Terraform is read statically.** Resources, modules, variables, and
  references between them — not plan output, not remote state.
- **One repository at a time.** A service whose dependencies live in sibling
  repositories is only as visible as this repository's config makes it.

The scan reports all of this in its diagnostics. Silence about a gap reads as
completeness, which is worse than the gap.

## Configuration

Optional. `.structura.yaml` at the repository root:

```yaml
# Treat these directories as independent projects rather than one system.
# Without this, a monorepo's stacks are merged into a single architecture.
projects:
  - services/billing
  - services/storefront

# Declare what configuration does not express, or remove what inference got
# wrong. `note` is carried into the graph as the evidence for the edge, and
# is the field a future reader actually needs.
relationships:
  - from: api
    to: cache
    kind: persists_to
    protocol: redis
    note: redis client built in api/internal/cache.go

  - from: api
    to: api.stripe.com
    remove: true
    note: the SDK is vendored but the integration was retired

# Fold components that inference read as separate things into one. The first
# name survives.
components:
  - same: [checkout, checkout-service]
    note: the Deployment and the codebase are the same component
```

A declared relationship enters the graph at full confidence with the config
file as its evidence, so a reader can always tell what was inferred from what
was asserted. `kind` defaults to `depends_on`, the weakest claim that is still
a relationship. A rule that no longer matches anything is reported as a
diagnostic rather than silently ignored — a correction that has quietly
stopped applying is worse than no correction.

## The graph format

`graph.json` carries a `schemaVersion` that moves independently of the CLI,
because the file gets committed and is then read by builds that are not the
one that wrote it. A minor bump is additive and only additive — new optional
fields, new node or edge kinds — so a reader built for an earlier minor still
understands everything it recognizes. Anything that removes a field, retypes
one, or changes what an existing kind means is a major bump.

The format is at `1.0.0`, which is a commitment rather than a milestone: the
fields and identifier grammar described there will not change shape again
without a `2.0.0`. It reached 1.0 before the CLI did because the file is the
part other things depend on — it gets committed, diffed, and read by builds
that are not the one that wrote it — and a format that keeps moving is not one
anybody can build on.

A build reading a graph from a *newer* schema serves it as it is rather than
rescanning over it, since rewriting would delete whatever that build cannot
represent and land the deletion in someone's history looking like an
architecture change. `pkg/schema/compat.go` is the full statement.

## Development

Requires Go (see `go.mod`), plus `golangci-lint` and `goreleaser` for the full
check suite.

```sh
make            # tidy + lint + cgo-free + race tests + determinism
make test       # fast unit tests
make build      # → bin/structura
make demo       # scan a fixture repo and print the result
make corpus     # clone the pinned real-world repositories
make corpus-test# score precision and recall against them
make eval       # ask a model 18 questions through the MCP tools (needs an API key)
make help       # all targets
```

Two invariants are enforced in CI and worth knowing before you contribute:

- **Nothing may pull in CGO.** `CGO_ENABLED=0` everywhere; a `runtime/cgo`
  dependency fails the build. Static binaries on every platform is the
  distribution promise, and it is easy to break by accident.
- **Nothing outside `internal/cli` may write to stdout.** The MCP server
  speaks JSON-RPC over stdio; a stray `fmt.Println` corrupts the transport and
  surfaces as an opaque parse error in the user's editor. A lint rule blocks
  it.

`make eval` is deliberately outside CI: it calls a model API, so it costs
money and is nondeterministic, and a flaky gate gets disabled until it stops
being run at all. It is also the only check that tests the product rather than
the parts — a graph can be structurally perfect and still useless if the tool
surface makes a model take six calls to learn what one should have told it.
Run it per milestone and before each release.

## License

[Apache 2.0](LICENSE).
