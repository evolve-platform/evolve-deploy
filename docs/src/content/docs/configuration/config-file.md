---
title: The config file
description: Services, targets, the short form, and what a deployment lockfile looks like.
sidebar:
  order: 1
---

One file per environment, one cloud per file. The filename is the environment
name by default: `deploy/tst.yaml` is the `tst` environment.

```yaml
cloud:
  provider: azure
  subscription: bbbf237a-8c9e-492a-b6a3-9b0bd4869690
  resource_group: evolve-tst

services:
  purchase:
    version: 27ec167
    targets:
      - { type: container-app, name: evolve-tst-purchase }
```

There are four top-level keys, and only two of them are usually present:

| Key | | |
|---|---|---|
| `cloud` | which cloud, and where in it | required |
| `services` | what to deploy | required |
| `strategy` | the file-wide default release strategy | optional, default `direct` |
| `refs` | policy for resolving references | optional |

## Services

A **service** is one unit of release: a single version, one or more deployables.

```yaml
services:
  catalog-commercetools:
    version: 27ec167
    targets:
      - { type: container-app, name: evolve-tst-catalog-commercetools }
      - { type: container-app-job, name: evolve-tst-catalog-ct-products }
      - { type: container-app-job, name: evolve-tst-catalog-ct-categories }
```

The version lives on the service, not on the target, and that is the point: one
image, five deployables, one version written once. A sync job can never drift
away from the service it shares an image with.

`env`, `envFrom`, `before` and `after` are all service-level for the same
reason. Hooks run once per service, not once per target — publishing a schema
five times for a service with four jobs is not what anyone wants.

## Targets

A **target** is a single deployable resource: the thing the image actually gets
set on.

```yaml
targets:
  - type: container-app
    name: evolve-tst-catalog-commercetools
  - type: container-app-job
    name: evolve-tst-catalog-ct-products
    env:
      SERVICE_ENTRYPOINT: sync-products/index.mjs
```

A target may add to or override the service's `env`. Everything else it needs —
which cluster, which package, which container — is either inherited from the
service or spelled out here.

### The short form

A service with exactly one target, named after the service, can skip the
`targets` block:

```yaml
services:
  site:
    version: abc1234
    type: ecs
    cluster: platform
```

That is exactly equivalent to:

```yaml
services:
  site:
    version: abc1234
    targets:
      - { type: ecs, name: site, cluster: platform }
```

### Fields inherited by targets

Set these on the service and every target that understands them picks them up.
Each is inherited only by the target types it applies to, so a service with both
an ECS service and a Lambda can set `cluster` once without the Lambda acquiring
a field that means nothing to it.

| Field | Applies to | |
|---|---|---|
| `cluster` | `ecs` | the ECS cluster name or ARN |
| `base` | `ecs` | the task definition family Terraform owns, default `<name>-base` |
| `container` | `ecs` | which container carries the application image |
| `code` | `lambda`, `function-app` | where the deployment package lives |

### The entry point

`command` replaces the container's entry point. It is how several targets that
share one image say which of them they are — the sync jobs beside a subgraph are
one build started several ways:

```yaml
targets:
  - { type: cloud-run,     name: discover }
  - { type: cloud-run-job, name: discover-products,
      command: ["node", "src/cli.ts", "sync", "products"] }
```

It sits on the target and never on the service, because a service whose targets
all took the same command would be describing one job several times over.

Write the whole command line. The declaration is the entry point the way `env`
is the environment, so any arguments the container carried separately are
dropped with it — Cloud Run appends args to command, and a leftover pair would
start the container on a line nobody wrote.

Leave it out and the entry point already on the target is carried through, which
is what happens where Terraform owns it. Worth knowing which of the two you are
relying on: a target that neither declares here nor gets one from Terraform runs
the image's own `CMD`, and for an image shared by a service and its jobs that is
the server.

Only `cloud-run` and `cloud-run-job` write it today. On the other types it is
refused rather than accepted and ignored.

### Picking the container

Most tasks have one container and there is nothing to decide. When a task has
several — a reverse proxy, an OpenTelemetry collector — the tool takes the one
named after the target, and sidecars are never touched.

If none of them uses that conventional name, it refuses rather than guessing,
and `container:` on the target is the answer:

```yaml
targets:
  - type: ecs
    name: purchase
    cluster: platform
    container: app
```

## A fuller example

```yaml
cloud:
  provider: azure
  subscription: 00000000-0000-0000-0000-000000000000
  resource_group: evolve-tst
  app_config: https://evolve-tst.azconfig.io

services:
  purchase:
    version: abc1234
    type: container-app
    env:
      LOG_LEVEL: info
      CTP_CLIENT_SECRET: ${secret:ctp-client-secret}
    before:
      - hive schema:check --service purchase --commit {{.version}}
    after:
      - hive schema:publish --service purchase --commit {{.version}}

  # One image, five deployables: the service plus its sync jobs.
  discover:
    version: abc1234
    envFrom:
      - ${param:/evolve/${env}/discover/setup}
    targets:
      - { type: container-app,     name: evolve-tst-discover }
      - { type: container-app-job, name: evolve-tst-discover-products }
      - { type: container-app-job, name: evolve-tst-discover-categories }

  site:
    version: def5678
    type: container-app
    depends_on: [purchase, discover]
```

The complete list of every key is in the [config schema
reference](../../reference/config/).
