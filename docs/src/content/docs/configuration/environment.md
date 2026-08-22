---
title: Environment variables
description: Two ways to use the tool — image only, or environment under deploy control — and the four layers that produce what a target ends up with.
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

Declare `env`, and those variables are laid over the ones the target already
carries. Terraform declares the environment; the config refines it. So listing
one variable sets one variable, and the other thirty keep the values Terraform
gave them.

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

The trade is that **the config sets but never removes**. Deleting a variable
means deleting it where it is declared, which is Terraform — the next release
then carries that through.

## The four layers

The environment a target ends up with is four layers, each with the last word
over the one before it:

1. **What Terraform declares** — everything already on the running resource.
2. **`envFrom`** — a JSON object expanded into the environment.
3. **`env`** — the variables named in the config, service level then target level.
4. **`strategy.env`** — for a blue-green service, the values for the side being
   staged. Plus `EVOLVE_DEPLOY_SIDE`, which the tool writes itself.

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
can never mean reading a secret. Anything in `env` wins over it, and both are
laid over what the target already carries.

## Refusing to delete

A variable can still disappear — when Terraform stops declaring one that a
running target has. `apply` refuses to be the release that carries that through
unless it is confirmed. Secrets are among the things that go this way:

```console
$ evolve-deploy apply deploy/tst.yaml

Would delete 3 environment variable(s):
  - container-app/evolve-tst-purchase: API_EXTENSION_SECRET
  - container-app/evolve-tst-purchase: OTEL_EXPORTER_OTLP_HEADERS
  - container-app/evolve-tst-purchase: SENTRY_DSN

The deploy config only ever sets variables, so these are gone from what
Terraform declares. Put them back there if that was not intended.

error: refusing to delete 3 environment variable(s); pass --allow-env-removal if you meant it
```

Two ways out, and they are not equivalent:

- **You meant it.** Pass `--allow-env-removal` on that one run.
- **You did not.** Put them back in Terraform. The refusal is almost always
  telling you that a `terraform apply` did something you did not read.

`diff` shows the same list without failing, so a pipeline can see it coming.

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
