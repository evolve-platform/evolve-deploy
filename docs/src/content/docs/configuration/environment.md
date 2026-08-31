---
title: Environment variables
description: Two ways to use the tool — image only, or environment under deploy control — the three layers that produce what a target ends up with, and exactly what each cloud does with them.
sidebar:
  order: 3
---

There are two ways to use this tool, and the difference is one key.

## Image only

Leave `env` and `envFrom` out, and the tool sets the image tag and touches
nothing else. Every environment variable stays exactly as Terraform left it.

This needs no mode of its own, because a config that sets nothing merges nothing.
The whole file:

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

Start here. Move to the other mode when you actually want a variable to change
on a deploy rather than on a `terraform apply`.

:::caution[`lambda` is the exception]
A Lambda's variables are rewritten on every deploy whether or not the config
declares any. See [AWS Lambda](#aws-lambda) below.
:::

## Environment under deploy control

Declare `env` — or `envFrom` — and the config becomes the whole environment. A
variable it does not name is removed. Listing one variable therefore means the
target ends up with one variable, not with that one plus whatever else was
already there.

```yaml
services:
  purchase:
    version: 27ec167
    env:
      LOG_LEVEL:         info
      CTP_API_URL:       https://api.europe-west1.gcp.commercetools.com
      CTP_CLIENT_SECRET: ${secret:purchase-server-token-commercetools}
    targets:
      - { type: container-app, name: evolve-tst-purchase }
```

This is the whole reason the config owns it rather than sharing it. On Azure a
container's `env` is on Terraform's `ignore_changes`, so Terraform writes one at
create and can never correct it afterwards. A tool that only ever laid its own
variables on top could not remove one either — so a variable set once outlived
every release, and went on outranking whatever was meant to replace it. Nothing
in the system could say what the environment *is*.

Configuration that used to arrive that way belongs in a parameter store the
service reads for itself — App Configuration, Parameter Store, Secret Manager —
leaving the config here to carry only what a store cannot tell a process: where
the store is, and the identity to read it with.

Cloud Run and ECS work the same way, though neither forced it: a Cloud Run
service is Terraform's to correct, and an ECS base task definition is registered
whole so a variable dropped there did reach the next release. They follow anyway,
because a list of variables that means the environment on one cloud and a patch
over an unseen one on another is not something a reader of a deploy file can be
asked to keep track of.

## The three layers

The environment a target ends up with is three layers, each with the last word
over the one before it:

1. **`envFrom`** — a JSON object expanded into the environment.
2. **`env`** — the variables named in the config, service level then target level.
3. **`strategy.env`** — for a blue-green service, the values for the side being
   staged. Plus `EVOLVE_DEPLOY_SIDE`, which the tool writes itself.

What the resource already carries is not a layer. It is replaced.

```yaml
services:
  discover:
    version: abc1234
    envFrom:
      - ${param:/evolve/${env}/discover/setup}   # layer 1
    env:
      LOG_LEVEL: debug                            # layer 2, wins over envFrom
    targets:
      - type: container-app
        name: evolve-tst-discover
        env:
          LOG_LEVEL: info                         # layer 2 too, and more specific
```

Layers 1 and 2 are flattened into one map before any driver sees them, so whether
a variable came from a bulk object, from the service or from the target makes no
difference to what is written. Layer 3 is handed over separately, because it is
applied differently — see [Per-side variables are not part of
this](#per-side-variables-are-not-part-of-this).

### `envFrom`

`envFrom` expands a JSON object into the environment — what Terraform already
writes with `jsonencode(local.env_vars)`:

```hcl
resource "azurerm_app_configuration_key" "discover_setup" {
  key   = "/evolve/tst/discover/setup"
  value = jsonencode(local.discover_env)
}
```

It must point at a **parameter store, never a secret store**, so bulk expansion
can never mean reading a secret. Anything in `env` wins over it, and together
they are the environment.

Everything it produces is a literal by the time a driver sees it: the object is
read while planning and its members are strings, not references. Listing two of
them is allowed, and the later one wins key by key.

## Removals

A variable the config stops naming is a variable the next release removes, and
the plan says so before anything is written:

```console
$ evolve-deploy diff deploy/tst.yaml

container-app/evolve-tst-purchase
  image  reg/purchase:34b990c -> reg/purchase:a1b2c3d
  - API_EXTENSION_SECRET
  - CTP_PROJECT_KEY
  + APP_CONFIG_ENDPOINT
```

There is no flag to confirm it. A removal here is something a person wrote in the
config and a reviewer read in the diff, unlike the old model where it meant
Terraform had quietly stopped declaring something — that was worth interrupting a
release for, and this is not.

Moving a service onto a parameter store is the large case: the first release drops
every variable the store now answers for, all at once, and lists them.

## How the merge works, per cloud

Everything above is the contract. This part is what each driver does with it,
which is what you want when you are reading a diff and wondering where a value
came from.

### The shape every container driver shares

No driver merges two lists of variables. Each one does the same two things:

1. It reads a **base**: a complete description of the resource — every container,
   its probes, its cpu, its sidecars.
2. On exactly **one** container in that base it replaces the image tag, the
   environment (only when the config declares one) and the entry point (only when
   the config declares one). Everything else in the base goes back untouched.

The declared environment therefore either **replaces** that container's or is not
applied at all. There is no third behaviour. What differs per cloud is which base
is read, what the write produces, and how a reference is written into it.

| Target | Base read from | The write produces | A reference becomes |
|---|---|---|---|
| `container-app` | the app's own template | a new revision | `secretRef` |
| `container-app-job` | the job's own template | the job, merge-patched | `secretRef` |
| `cloud-run` | the service's `template` | a new revision | `secretKeyRef` |
| `cloud-run-job` | the job as read, etag and all | the job, written whole | `secretKeyRef` |
| `ecs` | the `<name>-base` task definition family | a new revision of the `<name>` family | an entry in `secrets` |
| `lambda` | the function's current variables | the whole variables map, replaced | nothing — the tool reads it |
| `function-app` | — | only the tool's own app settings | — |

Which container is "the one" is [`container`](../clouds/) when the target names
it, and otherwise the conventionally named one. **Sidecars are never touched** —
not their image, not their environment. A reverse proxy's `proxy_env_vars` and an
OpenTelemetry collector's configuration stay Terraform's on every cloud, in both
modes.

### Azure Container Apps

The base is the **app's own template**, not the revision that is serving.
Terraform owns everything in the container except the tag and the environment, so
a probe, a cpu bump or a sidecar image it declares has to reach the next release.
Building on the serving revision instead would stage the running values back over
every `terraform apply`, and Terraform would never own what it declares.

A `${secret:…}` becomes a `secretRef` naming a secret **Terraform declared on the
app**. A name that is not declared fails the plan, with the declared names
listed — a revision that cannot start is a worse outcome than a refusal. There is
no equivalent for `${param:…}`, so the tool reads that from App Configuration and
writes the value as a literal.

The write is a merge patch carrying only the template, which is necessity rather
than tidiness: a read never returns secret values, so writing the resource back
whole would blank every one of them.

`container-app-job` behaves identically, minus everything about sides.

This is also the cloud where image-only mode really does mean *nothing else
moves*. Because `env` is on `ignore_changes`, Terraform cannot correct a variable
after create, so what the next revision carries is what the last one carried.

### GCP Cloud Run

The base is the service's `template`, for the same reason as on Azure, and the
write carries only the `template` field mask — ingress, IAM and the traffic split
are not part of it.

A `${secret:…}` becomes a `secretKeyRef` that Cloud Run resolves when the
revision starts, so no secret value passes through this tool. A `${param:…}`
resolves against Secret Manager, which Cloud Run cannot be pointed at for a plain
value, so the tool reads it and writes a literal. Two reference kinds, one handed
over and one read — see [References and secrets](../references/).

`cloud-run-job` is the one write that is not a patch: `UpdateJob` takes no field
mask, so the whole resource goes back. What keeps Terraform's parallelism,
retries, timeout and service account is that the job being written is the job
that was just read, with one container changed. An etag rides along, so a job
that moved in between fails the write rather than being overwritten.

Unlike Azure, Terraform here *can* correct a Cloud Run environment. Declaring
`env` in the deploy config means taking that away from it. The tool follows the
same rule anyway, so that a list of variables means one thing on every cloud.

### AWS ECS

ECS is the one place where the base is a **different object from the thing that
is running**. Image, environment, cpu and healthcheck live in one immutable
`container_definitions` blob, so field-level `ignore_changes` is impossible. Each
owner gets its own family instead: Terraform registers the shape into
`<name>-base`, nothing points at it, and `evolve-deploy` reads the latest ACTIVE
revision of that family and registers the result as `<name>`.

Two consequences that surprise people:

- In image-only mode the environment comes from the **base family**, not from the
  task definition currently running. A variable Terraform adds to the base
  reaches the service at the next deploy without the deploy config mentioning it,
  and the plan says `base <family>:<n> changed` when that is why there is
  something to do.
- Under deploy control the base's variables are dropped wholesale — both lists at
  once.

Both lists, because ECS splits an environment in two: literals go in
`environment`, references go in `secrets` as a `valueFrom`, and a name may be in
only one of them. Replacing rather than merging is what makes that safe — a
variable that turns from a literal into a `${secret:…}` cannot leave a stale twin
behind in the list it came from.

ECS is also the only target where **both** reference kinds are handed to the
platform: `valueFrom` accepts a Secrets Manager ARN and an SSM parameter name
alike, so nothing is ever read by the tool.

### AWS Lambda

Lambda has no reference mechanism at all — its environment variables are literal
strings — so every `${secret:…}` and `${param:…}` is read while planning and
written as a value. Under `refs.resolve: deny` that is refused instead, and a
Lambda carrying references cannot be deployed. This is the whole reason that
setting exists.

The tool also writes `EVOLVE_DEPLOY_VERSION` itself, because nothing else on a
function records which package it is running.

:::caution[Image-only mode does not apply here]
Every deploy sends the complete variables map, built from the config plus
`EVOLVE_DEPLOY_VERSION`. A `lambda` target with no `env` declared therefore ends
up with `EVOLVE_DEPLOY_VERSION` and nothing else, and the plan lists everything
Terraform set as a removal. Declare a Lambda's environment in the deploy config,
or put it in a store the function reads for itself.
:::

### Azure Function Apps

No environment at all. App settings on a function app hold platform wiring —
`AzureWebJobsStorage`, the deployment connection string, Application Insights —
alongside application config, and owning that map would mean reproducing secrets
the platform manages. Declaring `env` on a `function-app` target is a plan-time
error rather than a silent no-op:

```
function-app/evolve-tst-purchase-events: env is not supported on function-app
targets, because their app settings also hold platform wiring
(AzureWebJobsStorage, the deployment connection string). Run the app on Container
Apps if you want its environment managed here
```

The tool still writes its own two keys — `EVOLVE_DEPLOY_VERSION` and, on the
classic plans, `WEBSITE_RUN_FROM_PACKAGE` — and touches nothing else in the map.

### What the diff compares against

The `+` and `-` lines come from the same two environments the write is built
from, and from nothing remembered between runs:

- **Direct deploys** compare the base against what the write would produce.
- **Blue-green deploys** compare against the **serving** revision instead. With
  two live revisions the resource's template is whichever was created last, which
  after a failed deploy is the attempt that was abandoned — comparing against that
  would report the retry as already up to date.
- **A reference is compared by what it points at**, never by its value. The tool
  cannot see what a `secretRef` holds, and a rotated secret is not a change it
  should claim to have made. Moving `CTP_SECRET` from `${secret:a}` to
  `${secret:b}` shows up; rotating `a` does not.
- `EVOLVE_DEPLOY_SIDE` and every variable named by `strategy.env` are left out.
  They differ by side by definition, so comparing them would report a change on
  every run — and a change on every run is a deploy on every run, flipping the
  sides forever with no version ever changing.
- On `lambda` the comparison is over literal values, because that is all a Lambda
  environment holds.

An environment difference is enough on its own to make a deploy happen. A run
where only a variable moved reports `environment changed` as its reason, so a
release that writes something without the version moving says why.

### Per-side variables are not part of this

`strategy.env` and `EVOLVE_DEPLOY_SIDE` are written **last**, over whatever the
staged revision would otherwise carry, and they are **additive**: they do not
replace an environment, and they work in image-only mode too, where the rest of
it came off the resource untouched.

Before the staged side's values go in, every variable named by *any* side is
removed first. The containers being staged were copied from a template the other
side last wrote, so a variable this side does not set would otherwise arrive
carrying the other side's value. Config validation requires both sides to name
the same variables, so in practice this removes exactly what it puts back —
differently.

That works on Container Apps and Cloud Run. On ECS `strategy.env` is refused at
plan time and `EVOLVE_DEPLOY_SIDE` is not written: ECS owns both target groups
and swaps them itself, so its sides are roles within one release rather than two
standing environments for a service to address. See [Addressing the staged
side](../../blue-green/staged-side/) and [Blue-green per
cloud](../../blue-green/clouds/).

## Once `env` is in the config, extend `ignore_changes`

Terraform must stop trying to own what the deploy now owns:

```hcl
lifecycle {
  ignore_changes = [
    template[0].container[0].image,
    template[0].container[0].env,    # add this once env moves into the config
  ]
}
```

Otherwise the two write over each other on alternate applies. See [What
Terraform must do](../../infrastructure/terraform/).

On ECS there is nothing to add: the two families already separate the owners, and
Terraform goes on owning `<name>-base` whole.

## Not managed

`function-app` targets manage no environment at all — see [Clouds and
targets](../clouds/#function-app). Declaring `env` on one is an error.
