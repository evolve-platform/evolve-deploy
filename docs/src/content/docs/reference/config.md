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
| `{{url "svc"}}` | `strategy.smoke` | The staged side's address for that service |
| `{{label "svc"}}` | `strategy.smoke` | Which side it staged on |
| `{{revision "svc"}}` | `strategy.smoke` | The revision that was staged |

See [Templating](../../configuration/templating/).

## Checked while reading the config

These fail before any credentials are used and before anything is deployed:

- A `depends_on` naming a service that is not in the file
- A cycle between services
- `depends_on` between two blue-green services
- Sides in `strategy.env` that do not all name the same variables
- `env` declared on a `function-app` target
- `keep_warm` outside Container Apps, `bake_time` outside ECS
- `strategy.env` on an ECS target
- A blue-green ECS target with no `test_url`
- Fields belonging to a different `cloud.provider` than the one declared
