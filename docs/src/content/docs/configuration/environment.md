---
title: Environment variables
description: Two ways to use the tool — image only, or environment under deploy control — and the three layers that produce what a target ends up with.
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
      - ${param:/evolve/${env}/discover/setup}   # layer 2
    env:
      LOG_LEVEL: debug                            # layer 3, wins over envFrom
    targets:
      - type: container-app
        name: evolve-tst-discover
        env:
          LOG_LEVEL: info                         # layer 3 too, and more specific
```

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

## Not managed

`function-app` targets manage no environment at all — see [Clouds and
targets](../clouds/#function-app). Declaring `env` on one is an error.
