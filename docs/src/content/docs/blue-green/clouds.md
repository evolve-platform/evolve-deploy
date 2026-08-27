---
title: Per cloud
description: The choreography is the same everywhere. Who owns the traffic is not, and that changes what a rollback is.
sidebar:
  order: 3
---

The choreography is the same everywhere — stage, gate, switch — and so is the
config. What differs is **who owns the traffic**, and that changes what a
rollback is. It is worth knowing before you pick a cloud to be brave on.

| | Container Apps | Cloud Run | ECS |
|---|---|---|---|
| who moves the traffic | the tool | the tool | ECS |
| the sides | labels, alternating | tags, alternating | roles in one release |
| the staged address | `<app>---<label>.<domain>` | from the API | `test_url`, written down |
| after the switch | previous stopped | previous untagged, then retired | previous terminated |
| cost of the idle side | zero, or `keep_warm` | zero | zero, or `bake_time` |
| the idle side's address | `<app>---<label>.<domain>` | none — recorded, not addressable | none |
| `traffic --to <label>` | yes | yes | no — no side to name |
| `rollback` | any time | any time | until the release finishes |
| `strategy.env` per side | yes | yes | no |

## Azure Container Apps

The reference implementation. The tool owns `ingress.traffic`, the sides are
labels it writes, and everything in these pages works here.

Terraform sets `revision_mode = "Multiple"` and bootstraps the traffic block,
then lets go of it. See [What Terraform must
do](../../infrastructure/terraform/).

Container App **Jobs** carry no traffic, so they ride along: their templates are
written at the switch, never before it.

This is also the only platform where `keep_warm` means anything.

## GCP Cloud Run

The same model with fewer preconditions. There is no revision mode to switch on
— several revisions with a split are always allowed — so Terraform only has to
bootstrap the traffic block with a tag on the side that serves. The tagged URL
comes back from the API rather than being assembled.

Three differences worth knowing:

**The switch takes the tag off the side it retires.** A tag is an address:
`blue---service.run.app` answers whether or not any traffic is split that way, so
a revision that keeps its tag is never retired and goes on holding whatever
`min_instance_count` the template carries. Dropping the tag is what lets it
scale to zero. The revision a rollback needs is recorded in an
`evolve-deploy/rollback` annotation on the service instead — it cannot live on
the revision, because revisions are immutable and the one to name is the one that
stopped serving a moment ago.

The cost of that: **the idle side has no address of its own.** There is no URL to
curl the previous version on before deciding to go back to it. `rollback` still
reaches it in one write — it re-tags the recorded revision and moves the traffic
— as a cold start, which it always was.

**`keep_warm` is refused here.** Keeping a revision warm is
`scaling.min_instance_count`, which Terraform owns. On the template it applies
per revision; at service level it is divided over the revisions that have
traffic. Either way an idle side gets none of it, and the switch now actively
takes away the address that was keeping it up.

Cloud Run **jobs** carry no traffic, so they ride along the same way Container
App Jobs do: their definitions are written at the switch, never before it.

:::caution[Nothing expires]
Cloud Run keeps every revision it has ever been given. A service deployed twice a
day accumulates them until it meets a quota, and since the switch stopped tagging
the side it retires, an outside script cannot tell the rollback target from the
rest. Use [`evolve-deploy prune`](../../reference/cli/), which reads the traffic block
and the annotation before it removes anything.
:::

## AWS ECS

**ECS is the other family.** ECS has a blue/green engine of its own and it is
better than one this tool could build: it owns the target groups, the listener
rules and the shifting, and it can roll back on a CloudWatch alarm.

So the tool does not drive the rollout. It **declares** it and then answers the
gate — a `PAUSE` lifecycle hook at `POST_TEST_TRAFFIC_SHIFT`, which is test
traffic fully on the new side and production traffic still entirely on the old
one. That is exactly where a smoke test belongs.

No Lambda and no appspec: the exit code of a shell command still decides.

```yaml
strategy:
  type: blue-green
  bake_time: 10m        # ECS only: the window before the old side is terminated
  smoke: [ 'curl -fsS {{url_stage "site"}}/healthz' ]

services:
  site:
    version: 27ec167
    type: ecs
    cluster: platform
    test_url: https://site-test.internal.example
```

Three consequences, all from ECS owning the sides rather than the tool:

### `test_url` is required

The staged side is reached through the test listener rule, and **a rule is not
an address** — it may match on a host, a port or a header, and only the first
two are reachable by URL at all. So it is written down on the target, and a
blue-green ECS target without one is refused rather than staged with nothing to
point a gate at.

### `strategy.env` per side is refused

[Per-side environment](../staged-side/) exists so green calls green, which needs
the two sides to alternate and keep their names. ECS swaps its own target
groups, so the sides here are roles in one release rather than two standing
environments.

### `rollback` works on a window, not on a side

`traffic --to` does not apply — there is no side to name. But `rollback` does,
and it takes the other shape: for as long as the deployment has not finished,
the previous version is still running and ECS can put the traffic back on it.

That covers the `bake_time` window after a switch, and a release whose pipeline
died while paused at the smoke gate.

Once ECS has finished, `CLEAN_UP` has terminated the old tasks and going back is
a deploy of the previous version — which the command says, with the line to run,
rather than failing.

**So `bake_time` is not just an accounting setting: it is how long `rollback`
keeps working.** Zero means the switch is the end of it.

### Riding along

A Lambda has no listener rule and nothing to shift, so it rides along with the
service it shares an image with, and is written at the switch:

```yaml
services:
  purchase:
    version: abc1234
    cluster: platform
    targets:
      - type: ecs
        name: purchase
        test_url: https://purchase-test.internal.evolve-prd.example
      - type: lambda
        name: purchase-events
        code: { bucket: labdigital-evolve-artifacts, key: "purchase-sha-{{.version}}.zip" }
```

## Kubernetes

Not implemented. Asking for `blue-green` on a `helm` target is an explicit
refusal while planning, never a silent direct update.
