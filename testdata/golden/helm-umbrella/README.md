# helm-umbrella

A repository whose only deployment description is one Helm chart that deploys
several differently-named services.

Every other Helm fixture here sits beside plain manifests that already answer
the question, so nothing tested what this shape produces on its own. It
produced one boundary node and nothing else: `looksLikeChartValues` tests for
the single-service keys (`image`, `replicaCount`, `service`), and an umbrella
chart has none of them at the top level.

The values file deliberately includes blocks that must *not* become
components: `images` is chart-wide settings, `persistence` names a volume, and
`networkPolicies` is a feature switch.
