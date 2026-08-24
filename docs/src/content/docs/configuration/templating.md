---
title: Templating
description: ${env} in references, {{.version}} in hooks and package paths, and overriding versions from CI with --set.
sidebar:
  order: 5
---

There are two substitution syntaxes and they run at different times on different
things. Knowing which is which saves an afternoon.

| Syntax | Where | What it is |
|---|---|---|
| `${...}` | reference values in `env` and `envFrom` | the reference syntax — `${secret:}`, `${param:}`, `${env}` |
| `{{...}}` | hooks, `code.key`, `code.url` | Go templates, rendered with what the tool knows about the release |

## `${env}` — the environment name

Expands to the environment name, which by default is the config filename without
its extension. `deploy/tst.yaml` is `tst`.

```yaml
envFrom:
  - ${param:/evolve/${env}/purchase/setup}
```

Override it with `--env` when the filename is not the name you want:

```sh
evolve-deploy apply deploy/azure-tst.yaml --env tst
```

That changes the substitution only, never which file is read. The path is still
the only thing that decides which environment is touched.

## `{{.version}}` and friends — Go templates

Available in hooks and in package paths:

| | |
|---|---|
| `{{.version}}` | the version being deployed for this service |
| `{{.name}}` | the service name |
| `{{.env}}` | the environment name |
| `{{.label}}` | blue-green only: the side this release is going to |
| `{{.previous_label}}` | blue-green only: the side that was serving |

```yaml
services:
  purchase:
    version: abc1234
    after:
      - hive schema:publish --service {{.name}} --commit {{.version}}
    targets:
      - type: lambda
        name: purchase-events
        code:
          bucket: artifacts
          key: purchase-sha-{{.version}}.zip
```

On a `direct` service the two side variables are **absent rather than empty**,
so a hook naming one fails loudly instead of publishing to `tst-`.

Every hook is rendered while planning, so a typo in a variable name cannot fail
a release that already succeeded.

### The three addresses, and which one a hook wants

A release has three addresses, and they differ in what re-points them:

| | pinned to | moves when | Container Apps |
|---|---|---|---|
| `{{url_revision "site"}}` | one revision | never | `site--abc123.<domain>` |
| `{{url_stage "site"}}` | the label | you stage | `site---green.<domain>` |
| `{{url_public "site"}}` | the traffic weights | you switch | `site.<domain>` |

`url` is the short spelling of `url_public`.

In a **hook**, `url` is the one you want. A hook that registers something — a
federated subgraph, a webhook, a callback — writes an address down for something
else to read later, and later is after the labels have swapped again:

```yaml
services:
  discover:
    version: abc1234
    after:
      - >
        hive schema:publish backend/discover/schema.generated.graphql
        --service discover --commit {{.version}}
        --url {{url "discover"}}/graphql
```

It resolves to what Terraform declared — a Container App ingress hostname, a
Cloud Run service URI — read while planning, so it costs no call of its own.
`url_stage` and `url_revision` are refused in a hook: both name something this
release replaces, so a registration made from one looks right the day it is
written and points at the wrong revision from the next release on.

A function rather than a field, for all three, because a template field has to be
an identifier and `{{.catalog-commercetools.url}}` does not even parse. Each takes
a service name or a target name. A service with two targets that both have an
address cannot be named by its own name, because that name cannot mean either of
them; a job or a function beside a service does not count towards that, so the
ordinary app-plus-job service is still nameable.

A target with no address **fails the plan** rather than resolving to an empty
string. `url_revision` is the one genuinely absent on two platforms out of three:
a Cloud Run revision is reachable through a tag and not otherwise, and an ECS
task set has a listener rule in front of it rather than an address.

### In `strategy.smoke`, the staged side is the point

A smoke test gates the whole release rather than one service, so it has no single
service to take a URL from. It names one, and it names the **staged** side —
because the service's own address is still serving the version this release is
replacing, and a gate pointed there passes on the old code:

```yaml
strategy:
  type: blue-green
  smoke:
    - uses: http
      with: { url: '{{url_stage "site"}}/healthz', retry: 5 }
```

| | |
|---|---|
| `{{url_stage "site"}}` | the staged side's address |
| `{{url_revision "site"}}` | the staged revision's own address |
| `{{url_public "site"}}` | the address still serving the version being replaced |
| `{{label "site"}}` | which side it staged on |
| `{{revision "site"}}` | the revision that was staged |

A name that stages nothing in this release fails the plan rather than resolving
to an empty string — a gate pointed at nothing would pass.

Bare `url` is refused here, and only here. It used to mean the staged side and
now means the public address; resolved silently it would keep rendering, keep
passing, and start testing the version being replaced. So a smoke block has to
say which one it means. `url_public` written out in full is allowed — that is a
choice rather than a leftover.

## `--set` — a version from the pipeline

Override a version without editing the file. This is how CI deploys a test
build:

```sh
evolve-deploy apply deploy/tst.yaml --set site=${GITHUB_SHA:0:7}
```

Repeatable, and it composes with `--only`:

```sh
evolve-deploy apply deploy/tst.yaml \
  --only site,purchase \
  --set site=abc1234 \
  --set purchase=abc1234
```

Nothing has to be committed for a test deploy. Production, where the file *is*
the record, normally uses the committed version.

## `--var` — anything else the pipeline knows

A pipeline knows things a deploy config cannot: which generation of a federated
graph this release writes to, a build number, a change ticket. `--var` passes
them to hooks as `{{.name}}`:

```sh
evolve-deploy apply deploy/prd.yaml --var ticket=CHG-4471
```

```yaml
after:
  - notify-change --ref {{.ticket}} --version {{.version}}
```

Repeatable. Deliberately generic — the tool learns nothing about whatever the
value means.

The names the tool uses itself are reserved and cannot be shadowed: `version`,
`name`, `env`, `label`, `previous_label`, `url`, `url_stage`, `url_revision`,
`url_public`, `revision`. A hook silently receiving a different version than the
one being deployed is not worth being able to do.
