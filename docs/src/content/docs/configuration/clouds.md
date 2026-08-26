---
title: Clouds and targets
description: The cloud block is a tagged union — provider selects which fields apply, and the rest are rejected.
sidebar:
  order: 2
---

The `cloud` block says which cloud and where in it. It is a **tagged union**:
`provider` selects which fields apply and the rest are rejected, so the file has
the same shape whichever cloud it targets, and a leftover field from a copied
config is an error rather than something silently ignored.

| `provider` | Target `type`s | Rest of the block |
|---|---|---|
| `aws` | `ecs`, `lambda` | `account` + `region` |
| `gcp` | `cloud-run`, `cloud-run-job` | `project` + `region` |
| `azure` | `container-app`, `container-app-job`, `function-app` | `subscription` + `resource_group`, optionally `app_config` |
| `kubernetes` | `helm` | `context` + `namespace` |

:::caution[Kubernetes is not implemented]
The `helm` target type is declared but has no driver behind it. Planning against
it reports an explicit "not implemented" rather than doing nothing quietly.
:::

## AWS

```yaml
cloud:
  provider: aws
  account: "513712104672"
  region: eu-west-1
```

`account` is a **guard, not an address**. The account is implicit in the
credentials, so the tool compares this value against `sts:GetCallerIdentity` and
refuses on a mismatch. Everything else in the file is reviewable; without this,
where it points would not be.

Quote it. YAML will otherwise read a 12-digit account number as an integer and
drop a leading zero.

### `ecs`

```yaml
services:
  purchase:
    version: abc1234
    type: ecs
    cluster: platform
```

ECS keeps image, environment, cpu and probes in one immutable
`container_definitions` blob, so field-level `ignore_changes` is impossible.
Instead each owner gets its own task definition family: Terraform registers the
shape into `<name>-base`, nothing points at it, and `evolve-deploy` derives the
running family from it. `base` defaults to `<name>-base` and can be set
explicitly. See [What Terraform must do](../../infrastructure/terraform/).

### `lambda`

A Lambda has no registry to read a tag from, so the package location is spelled
out. `{{.version}}` is substituted:

```yaml
targets:
  - type: lambda
    name: purchase-events
    code:
      bucket: labdigital-evolve-artifacts
      key: purchase-sha-{{.version}}.zip
```

Lambda is also the one place where [references](../references/) are read by the
tool rather than handed to the platform: its environment variables are literal
strings with no reference mechanism at all.

## GCP

```yaml
cloud:
  provider: gcp
  project: evolve-tst
  region: europe-west4
```

### `cloud-run`

```yaml
services:
  purchase:
    version: abc1234
    type: cloud-run
```

Updates carry only the `template` field mask, so ingress, IAM, traffic split and
everything else on the service are untouched.

### `cloud-run-job`

```yaml
services:
  discover:
    version: abc1234
    targets:
      - { type: cloud-run,     name: evolve-tst-discover }
      - { type: cloud-run-job, name: evolve-tst-discover-products }
      - { type: cloud-run-job, name: evolve-tst-discover-categories }
```

A job is the same image as the service beside it, started with different
arguments. Those arguments live in the task template and are Terraform's; only
the tag and the environment are written here.

`UpdateJob` takes no field mask, so unlike a service the whole resource goes
back over the wire. What keeps Terraform's settings — parallelism, retries,
timeout, the service account — is that it is the job that was just read, with
one container changed and nothing else touched. An etag rides along, so a job
that moved in between fails the write rather than being overwritten.

Jobs carry no traffic, so in a blue-green release they ride along — their
templates are written at the switch, never before it.

There is no readiness to wait for: a job runs when it is triggered, so a broken
image is discovered at the next run rather than at deploy time.

## Azure

```yaml
cloud:
  provider: azure
  subscription: 00000000-0000-0000-0000-000000000000
  resource_group: evolve-tst
  app_config: https://evolve-tst.azconfig.io    # only if you use ${param:...}
```

Updates are a merge patch carrying only the template, which on Azure is
necessity rather than tidiness: a full write-back would blank every secret,
because a read never returns their values.

### `container-app` and `container-app-job`

```yaml
services:
  discover:
    version: abc1234
    targets:
      - { type: container-app,     name: evolve-tst-discover }
      - { type: container-app-job, name: evolve-tst-discover-products }
```

The reference implementation for [blue-green](../../blue-green/overview/):
the tool owns `ingress.traffic`, the sides are labels it writes, and every
blue-green feature works here.

Jobs carry no traffic, so in a blue-green release they ride along — their
templates are written at the switch, never before it.

### `function-app`

```yaml
services:
  purchase:
    version: abc1234
    code:
      url: https://artifacts.blob.core.windows.net/functions/purchase/purchase-sha-{{.version}}.zip
    targets:
      - { type: function-app, name: evolve-tst-purchase-events }
```

Deploying one is a **one deploy**, the only technology Flex Consumption
supports: the package is fetched from blob storage and posted to the app's
publish endpoint. Because nothing in the app then records which package it is
running, the tool writes an `EVOLVE_DEPLOY_VERSION` app setting — the same
marker Lambda needs, for the same reason.

:::caution[Function apps manage no environment at all]
Their app settings hold platform wiring — `AzureWebJobsStorage`, the deployment
connection string, Application Insights — alongside application config, and
owning that map would mean reproducing secrets the platform manages. Declaring
`env` on a `function-app` target is an error rather than a silent no-op.
:::

Whoever runs this needs **Storage Blob Data Reader** on the artifacts account:
the package is fetched over the data plane, not through ARM, so the roles that
let you manage the storage account are not enough to read from it.

## Maturity

Not every driver has the same amount of road behind it. This is worth knowing
before you pick one to be brave on.

| | Status |
|---|---|
| Azure Container Apps and Jobs | Exercised against a real subscription: reading, planning, the diff both ways, skipping when nothing changed, the full write, concurrency, sidecars untouched, `--set` |
| Azure Function Apps | Half proven — planning works against real apps, but nothing has been deployed to one yet |
| AWS (ECS, Lambda) | Built, never run against a real account |
| GCP (Cloud Run) | Built, never run against a real account |
| Kubernetes / Helm | Not built; refuses explicitly |
