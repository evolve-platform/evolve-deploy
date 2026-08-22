---
title: How a release works
description: Stage the new version on the side carrying no traffic, gate it, switch in one write. A release is one release.
sidebar:
  order: 1
---

By default a new version starts serving as soon as it is ready. That covers the
crudest failure — a container that never starts never gets traffic — and nothing
else: a container that starts but is broken serves everyone, there is no moment
to check anything, and going back means another pipeline run.

`strategy: blue-green` stages the new version on the side that carries no
traffic, runs a command against it, and switches in one write.

```yaml
strategy:
  type: blue-green
  smoke:
    - uses: http
      with: { url: '{{url "site"}}/healthz', retry: 5, delay: 2s }

services:
  site:
    version: 27ec167
    type: container-app

  # No gate for this one: it stages and switches straight away.
  catalog-commercetools:
    version: 27ec167
    type: container-app
    strategy:
      smoke: []

  # And this one is updated straight, the way everything is by default.
  purchase:
    version: 27ec167
    type: container-app
    strategy:
      type: direct
```

## The strategy block

| Field | | Default |
|---|---|---|
| `type` | `direct` \| `blue-green` | `direct` |
| `smoke` | hooks run against the staged side | empty = switch straight away |
| `labels` | the two side names | `[blue, green]` |
| `env` | [environment per side](../staged-side/) | none |
| `keep_warm` | leave the previous version running after the switch | `false`, Container Apps only |
| `bake_time` | how long before ECS terminates the old side | `0`, ECS only |

The file's block is the default and a service overrides it **field by field**.
Lists and maps are replaced whole, never merged — `smoke: []` has to be sayable
without restating everything else.

## A release is one release

Every staged service is staged first, then every smoke step runs, then all of
the traffic moves. Not stage-smoke-switch per service.

```
  stage everything  ─┐
                     ├─ smoke, once ─── switch everything, one write per app
  stage everything  ─┘
```

Which means **a service with nothing to change is staged as well**, at the
version it already runs. The side is a property of the environment, and a side
missing an app is not a stack you can point a smoke test at. (A revision can
carry only one label, so the serving revision cannot lend the idle one its own.)

Two things follow:

- [`depends_on`](../../deploying/ordering/) between two blue-green services is
  refused. Staging carries no traffic, and a staged side reaches its own side by
  label URL whatever the weights say, so there is nothing left to order.
- **If nothing in the release has a real change, nothing is staged at all.** A
  second `apply` is still a no-op.

## Active is the label with 100% of the traffic

There is no state and no marker. The tool reads the traffic block and the side
with all of it is the one serving.

If no label has all of it, **it refuses**. A split means someone is in there by
hand or a previous run died, and there is then no active side to deploy away
from. That check runs while planning, so one odd split stops the whole release
before anything is written.

The same holds across the environment: the apps have to agree on which side is
idle, because "green" must mean the same thing everywhere for the staged side to
be a stack. A release where they disagree is refused, naming the command that
aligns them:

```sh
evolve-deploy traffic deploy/prd.yaml --to blue
```

## The idle side is switched off

The previous version keeps its label at 0% and that is the rollback target — but
after the switch it is switched off.

A Container Apps revision that is not deactivated holds on to its own scale
rules, so with `minReplicas >= 1` the side nobody is using goes on costing money
for as long as the pair stands. A version nobody is using should not cost
anything.

Rolling back then starts it again, which is a container start rather than the
one write it would otherwise be. `keep_warm: true` buys that write back:

```yaml
strategy:
  type: blue-green
  keep_warm: true      # prd: an outage is measured in money, a cold start is not free
```

Set it per file or per service, which is the axis production and test actually
differ on. Either way this is one line rewritten after every successful deploy,
even when it already held, so a run that died halfway is tidied by the next
release — there is no retention setting and no cleanup command.

:::caution[Two versions, one database]
Version N and N-1 are live at the same time against the same database while a
release is being staged and checked. That is the familiar expand/contract
discipline and the tool can see nothing about it.

With `keep_warm` they also both hold replicas between releases, which doubles
the compute floor for that app.
:::

## There is no gradual traffic shifting

No 5% for a minute, then 20%. Both platforms can express it and this
deliberately does not use it.

To hold at 5% and then decide something, you need to know the error rate at 5%
— and there is no metrics client here. The smoke test checks the new version
deliberately, against a URL, before anyone reaches it. That is a better gate
than 5% of traffic and an unread graph.

## Not everywhere

Implemented for Azure Container Apps, GCP Cloud Run and AWS ECS. Kubernetes has
no implementation, and asking for it there is an explicit "not implemented"
while planning — never a silent direct update.

The choreography is the same everywhere and so is the config. What differs is
who owns the traffic, and that changes what a rollback is: see [Per
cloud](../clouds/).
