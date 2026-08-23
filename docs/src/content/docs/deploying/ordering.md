---
title: Ordering with depends_on
description: Services roll out concurrently by default. depends_on says which way round, and deliberately does two things less than you might expect.
sidebar:
  order: 2
---

Services roll out concurrently, which is right until one of them reads from
another. `depends_on` says which way round:

```yaml
services:
  discover:
    version: abc1234
    type: container-app
    name: evolve-tst-discover

  purchase:
    version: abc1234
    type: container-app

  site:
    version: abc1234
    depends_on: [discover, purchase]
    type: container-app
    name: evolve-tst-site
```

`site` now starts only once both backends have finished. Everything else still
runs at once — this is an ordering constraint, not a queue, so each service
waits for exactly what it named and nothing else.

## Why bother

Deploying a frontend and its backend at the same time makes a window where the
new frontend talks to the old backend. That window is real, it is usually short,
and it is entirely invisible in the output. `depends_on` closes it for the cases
where it matters.

## Two things it deliberately does not do

**It does not pull a service into a release.** A dependency that is not part of
this run — filtered out by `--only`, or already at its version — is simply
satisfied. That is what makes it usable in CI, where the pipeline deploys only
what it rebuilt:

```sh
evolve-deploy apply deploy/tst.yaml --only site --set site=abc1234
```

`site` depends on `discover`, `discover` is not in this run, and the run
proceeds. Anything else would mean a one-service deploy quietly becoming a
whole-environment one.

**It does not deploy a service whose dependency failed.** That one is reported as
*not deployed* rather than counted as a failure of its own, because nothing was
written for it:

```console
  site                               skipped, discover did not deploy
```

and again at the end, counted separately from the failures:

```console
1 service(s) were not deployed at all:
  - site, waiting on discover
```

## Checked while reading the config

A `depends_on` naming a service that is not in the file, and a cycle between
services, both fail while reading the config — before anything is deployed, and
before any credentials are even used.

## Not with blue-green

`depends_on` between two blue-green services is refused. Staging carries no
traffic, and a staged side reaches its own side by label URL whatever the weights
say, so there is nothing left to order — the whole release stages, then gates,
then switches, as one thing.

A blue-green service may still depend on a `direct` one, and the other way
round.
