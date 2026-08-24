---
title: Config schema
description: Every key in the config file, what it accepts, and where it applies.
sidebar:
  order: 2
---

```yaml
cloud:      { ... }   # required
refs:       { ... }   # optional
strategy:   { ... }   # optional
services:   { ... }   # required
```

## `cloud`

A tagged union. `provider` selects which fields apply and the rest are rejected.

| Key | Provider | |
|---|---|---|
| `provider` | | `aws` \| `gcp` \| `azure` \| `kubernetes` |
| `account` | aws | Checked against `sts:GetCallerIdentity`. Quote it |
| `region` | aws, gcp | |
| `project` | gcp | |
| `subscription` | azure | |
| `resource_group` | azure | |
| `app_config` | azure | App Configuration URL. Only needed if `${param:}` is used |
| `context` | kubernetes | Not implemented |
| `namespace` | kubernetes | Not implemented |

## `refs`

| Key | Default | |
|---|---|---|
| `resolve` | allow | `deny` refuses to read any reference value, at the cost of not being able to deploy Lambda targets that carry one |

## `strategy`

At the top level this is the default for every service; a service overrides it
field by field. Lists and maps are replaced whole, never merged.

| Key | Default | |
|---|---|---|
| `type` | `direct` | `direct` \| `blue-green` |
| `smoke` | none | Hooks run once against the staged side, after everything has staged and before any traffic moves. `[]` switches straight away |
| `labels` | `[blue, green]` | The two side names |
| `env` | none | `{ <label>: { KEY: value } }`. Every side must name the same variables |
| `keep_warm` | `false` | Leave the previous version running after the switch. Container Apps only; refused elsewhere |
| `bake_time` | `0` | How long before ECS terminates the old side. ECS only; also the rollback window |

## `services.<name>`

| Key | | |
|---|---|---|
| `version` | required | The image tag, or the version substituted into a package path |
| `type` | | Target type, for the short form |
| `targets` | | List of [targets](#targets). Omit for the short form |
| `env` | | `{ KEY: value }`, applied to every target. Values may be [references](../../configuration/references/) |
| `envFrom` | | List of `${param:...}` references to JSON objects to expand |
| `depends_on` | | Services that must finish before this one starts |
| `before` | | [Hooks](../../deploying/hooks/) that gate the whole release |
| `after` | | Hooks run once this service's targets have all succeeded |
| `strategy` | | Overrides the file's block, field by field |
| `cluster` | ecs | Inherited by ECS targets |
| `base` | ecs | Inherited by ECS targets. Default `<name>-base` |
| `container` | ecs | Inherited by ECS targets |
| `code` | lambda, function-app | Inherited by targets that ship a package |

### The short form

A service with exactly one target named after itself may put the target's fields
directly on the service and omit `targets`:

```yaml
services:
  site:
    version: abc1234
    type: ecs
    cluster: platform
```

## `targets[]`

| Key | | |
|---|---|---|
| `type` | required | `ecs` \| `lambda` \| `cloud-run` \| `container-app` \| `container-app-job` \| `function-app` \| `helm` |
| `name` | required | The resource name |
| `env` | | Adds to, and overrides, the service's `env` |
| `cluster` | ecs | Cluster name or ARN. Required for `ecs` |
| `base` | ecs | Task definition family Terraform owns. Default `<name>-base` |
| `container` | ecs | Which container carries the application image. Only needed when a task has several and none uses the conventional name |
| `test_url` | ecs | Where the test listener rule answers. Required for a blue-green ECS target |
| `code` | lambda, function-app | Where the deployment package lives |

### `code`

| Key | | |
|---|---|---|
| `bucket` + `key` | lambda | S3 location. `key` may contain `{{.version}}` |
| `url` | function-app | Full blob URL. May contain `{{.version}}` |

## Hooks

Each entry in `before`, `after` and `strategy.smoke` takes one of three forms:

```yaml
- hive schema:publish --commit {{.version}}       # a command line
- cmd: hive schema:publish --commit {{.version}}  # the same, written out
- uses: honeycomb                                 # a named action
  with: { dataset: purchase }
```

The options for each action are on the [Actions](../../deploying/actions/) page.

## Substitutions

| | Where | |
|---|---|---|
| `${env}` | reference values | The environment name |
| `${secret:NAME}` | `env` values | A secret, resolved by the platform where it can |
| `${param:PATH}` | `env` values, `envFrom` | A parameter store value |
| `{{.version}}` | hooks, `code.key`, `code.url` | The version being deployed |
| `{{.name}}` | hooks | The service name |
| `{{.env}}` | hooks | The environment name |
| `{{.label}}` | blue-green hooks | The side this release is going to |
| `{{.previous_label}}` | blue-green hooks | The side that was serving |
| `{{url "svc"}}` | hooks | The address that target keeps after the release |
| `{{url_public "svc"}}` | hooks, `strategy.smoke` | The same address, spelled out |
| `{{url_stage "svc"}}` | `strategy.smoke` | The staged side's address |
| `{{url_revision "svc"}}` | `strategy.smoke` | The staged revision's own address |
| `{{label "svc"}}` | `strategy.smoke` | Which side it staged on |
| `{{revision "svc"}}` | `strategy.smoke` | The revision that was staged |

Three addresses, told apart by what re-points them: a revision never, a side when
you stage, the public one when you switch. The staged two are refused in a hook
and bare `url` is refused in `strategy.smoke`, where it used to mean the side.

See [Templating](../../configuration/templating/).

## Refused before anything is deployed

Two groups, and the difference is worth knowing when a pipeline fails: the first
never touches a cloud, so it fails the same way on a laptop with no credentials
as it does in CI.

### While reading the config

Parsing is strict — an unknown key is an error — and the whole file is checked at
once, so one run reports every mistake in it rather than the first:

- An unknown key, or a field belonging to a different `cloud.provider` than the
  one declared
- A `type` that is not valid on the declared provider
- A `depends_on` naming a service that is not in the file, or naming itself
- A cycle between services, reported as the path around it
- `depends_on` between two blue-green services
- A service called `(release)`, which is what the staged release is reported as
- Two targets with the same type and name
- `base`, `cluster` or `container` outside `ecs`; `code` outside `lambda` and
  `function-app`; `code.bucket` on a `function-app`
- A missing `cluster` on `ecs`, or missing `code` on `lambda`
- `EVOLVE_DEPLOY_SIDE` set by hand — the tool writes it
- `smoke` on a service rather than on the file's `strategy` block
- `smoke`, `env`, `keep_warm` or `bake_time` under `type: direct`, which has no
  side for any of them to mean anything about
- `keep_warm` together with `bake_time`
- `labels` that is not exactly two distinct non-empty names
- Sides in `strategy.env` that do not all name the same variables, or a side
  that is not one of the labels
- A hook that is neither a command, a `cmd` nor a known `uses`, or one missing an
  option its action requires

### While planning, against the cloud

A capability the driver cannot honour is refused here rather than downgraded, so
these need credentials but still deploy nothing:

- A blue-green `ecs` target with no `test_url`
- `strategy.env` on an `ecs` target
- `keep_warm` outside Container Apps
- `bake_time` outside ECS
- `env` on a `function-app` target
- An image tag that does not exist, or a reference that resolves to nothing
- Traffic, listener rules or scale rules Terraform never declared
- A hook naming a template variable that does not exist, or an action whose API
  key is nowhere in the environment — checked here because an `after` hook runs
  on a release that has already succeeded
