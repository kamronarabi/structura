# containerized-app

A repository with no orchestrator. Nothing here declares a cluster, a Compose
project, or a chart: CI builds each Dockerfile and a platform runs the images.
That is a very common shape, and before Structura read Dockerfiles it produced
one node for the one package.json and reported the rest of the tree as unread.

What each directory is here to pin:

- `api` — a Dockerfile beside a go.mod. Two files, one component: the module
  name wins, and the base image and port come from the Dockerfile.
- `web` — a Dockerfile with no manifest at all, and the multi-stage shape that
  hides the language: the bundle is built in a Node stage and served by nginx.
- `worker` — `Dockerfile` beside `Dockerfile.debug`, a variant of one service.
- `orders-db` and `cache` — a Dockerfile that starts FROM the product it ships.
- `tools/migrator/docker` — a directory named for its role, which names no
  component; the name comes from the directory above it.

The edges come from ENV values, which is the only place this repository says
what talks to what.
