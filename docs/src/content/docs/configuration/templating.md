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

### In `strategy.smoke`, they are functions

A smoke test gates the whole release rather than one service, so it has no
single service to take a URL from. It names one:

```yaml
strategy:
  type: blue-green
  smoke:
    - uses: http
      with: { url: '{{url "site"}}/healthz', retry: 5 }
```

| | |
|---|---|
| `{{url "site"}}` | the staged side's address for that service |
| `{{label "site"}}` | which side it staged on |
| `{{revision "site"}}` | the revision that was staged |

Functions rather than fields, because a template field has to be an identifier
and `{{.catalog-commercetools.url}}` does not even parse.

A name that stages nothing in this release fails the plan rather than resolving
to an empty string — a gate pointed at nothing would pass.

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
`name`, `env`, `label`, `previous_label`, `url`, `revision`. A hook silently
receiving a different version than the one being deployed is not worth being
able to do.
