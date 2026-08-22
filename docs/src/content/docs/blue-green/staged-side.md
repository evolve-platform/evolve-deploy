---
title: Addressing the staged side
description: A smoke test against the whole staged stack, not one container. Why the side goes in the environment rather than in a header.
sidebar:
  order: 2
---

A smoke test against one staged container is worth something. A smoke test
against the whole staged *stack* is worth much more — and for that, a service has
to reach **its own side's** downstream rather than whatever is live.

## Why not a header

Tagging the request is the wrong way round. A header only survives if the router,
the reverse proxy and every sidecar forward it, and one that drops it produces a
smoke test that quietly checks the old version and passes.

The side is a property of the deployment, so it goes in the environment.

## `strategy.env`

Each side gets its own values:

```yaml
services:
  graphql-gateway:
    version: 27ec167
    type: container-app
    strategy:
      env:
        blue:
          HIVE_CDN_ENDPOINT: https://cdn.graphql-hive.com/artifacts/v1/<tst-blue>
          HIVE_CDN_KEY: ${secret:hive-cdn-blue}
        green:
          HIVE_CDN_ENDPOINT: https://cdn.graphql-hive.com/artifacts/v1/<tst-green>
          HIVE_CDN_KEY: ${secret:hive-cdn-green}
```

The green router then reads the green graph, which names the green subgraph URLs,
and the storefront staged on green calls the green router — with nothing to
propagate and nobody to cooperate.

Or more simply, for a plain service-to-service URL:

```yaml
strategy:
  type: blue-green
  env:
    blue:
      ROUTER_URL: https://router---blue.evolve-prd.example
    green:
      ROUTER_URL: https://router---green.evolve-prd.example
```

## This is not the same as declaring `env`

Declaring [`env`](../../configuration/environment/) on a service means the
deploy owns that variable outright. `strategy.env` writes the variables named
here over whatever the staged revision inherited and leaves the rest to
Terraform — exactly like `EVOLVE_DEPLOY_SIDE` itself.

[References](../../configuration/references/) work as they do anywhere else, so
a per-side secret is named rather than carried.

## Two properties worth knowing

**The values are excluded from the environment diff.** They differ by side by
definition, so comparing the staged side's against the serving side's would
report a change on every run — which would deploy on every run and flip the
sides forever with no version ever changing. The plan prints which variables the
side sets instead.

**Every side must name the same variables.** The staged containers are copied
from the serving revision, so a variable only one side sets does not arrive
unset — it arrives carrying the *other side's* value. That is refused while
reading the config rather than resolved by picking a behaviour for it.

## `EVOLVE_DEPLOY_SIDE`

Every blue-green target gets `EVOLVE_DEPLOY_SIDE=blue|green` in its environment,
written by the tool.

A request cannot carry the side — a header only arrives if every hop forwards it
— but a service can resolve its own downstream by its own side with nothing to
propagate. It is written and never compared: the side alternates every release,
so diffing it would report a change on every run.

## Naming a service from a smoke step

`smoke` gates the release rather than a service, so it runs once and has no
single service to take a URL from. It names one:

```yaml
strategy:
  type: blue-green
  smoke:
    - uses: http
      with: { url: '{{url "purchase"}}/healthz', retry: 5, delay: 2s }
    - uses: http
      with: { url: '{{url "discover"}}/healthz', retry: 5, delay: 2s }
    # The one worth more than the sum of those: a request through the staged
    # side, end to end.
    - npm run smoke -- --base-url {{url "site"}}
```

| | |
|---|---|
| `{{url "site"}}` | the staged side's address for that service |
| `{{label "site"}}` | which side it staged on |
| `{{revision "site"}}` | the revision that was staged |

Functions rather than fields, because a template field has to be an identifier
and `{{.catalog-commercetools.url}}` does not even parse.

A name that stages nothing in this release **fails the plan** rather than
resolving to an empty string. A gate pointed at nothing would pass.

## Per-service hook variables

Inside `before` and `after`, which do run per service, the side is available as
a field — named after its role in the release, so it means the same thing in
both:

| | |
|---|---|
| `{{.label}}` | the side this release is going to |
| `{{.previous_label}}` | the other one: what was serving, and what a rollback returns to |

```yaml
before:
  - hive schema:check   --service purchase --target prd-{{.previous_label}}
after:
  - hive schema:publish --service purchase --target prd-{{.label}}
```

Check against what is serving now; publish to the side this is going to.

On a `direct` service these are **absent rather than empty**, so a hook naming
one fails loudly instead of publishing to `tst-`.
