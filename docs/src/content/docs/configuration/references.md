---
title: References and secrets
description: The config never contains a secret. How ${secret:} and ${param:} resolve, and what they mean per cloud.
sidebar:
  order: 4
---

Values in `env` are either literals or references.

**The config never contains a secret.**

```yaml
env:
  LOG_LEVEL:         info                            # literal
  REDIS_URL:         ${param:/platform/redis-url}    # configuration store
  CTP_CLIENT_SECRET: ${secret:purchase-ctp-secret}   # secret store
```

## Handed over, or read

A reference is handed to the platform where the platform can resolve one, and
read by the tool only where it cannot. The difference matters: where the platform
resolves it, the value never passes through CI at all.

| Target | `${secret:}` | `${param:}` |
|---|---|---|
| `ecs` | `valueFrom` | `valueFrom` |
| `cloud-run`, `cloud-run-job` | `secretKeyRef` | read from Secret Manager |
| `container-app`, `container-app-job` | `secretRef` | read from App Configuration |
| `lambda` | read by the tool | read by the tool |
| `function-app` | not managed | not managed |

Lambda is the exception because its environment variables are literal strings
with no reference mechanism at all. There is nowhere to hand a reference to.

### Refusing to resolve at all

A project that will not accept any value passing through CI can say so — at the
cost of not being able to deploy Lambda targets that carry references:

```yaml
refs:
  resolve: deny
```

## What `${secret:}` names differs per cloud, and it has to

This is the part that surprises people moving a config between clouds.

**On AWS** it is a Secrets Manager ARN or an SSM parameter name, which a task
definition carries directly.

**On Azure** a secret must be *declared on the resource* — with a Key Vault URL
and the identity allowed to read it — and then referred to by name. Declaring it
is Terraform's job, so `${secret:ctp-secret}` means "the secret named
`ctp-secret` that Terraform declared on this app". A name that is not declared
**fails the plan** rather than producing a revision that cannot start.

```hcl
resource "azurerm_container_app" "purchase" {
  # ...
  secret {
    name                = "ctp-client-secret"
    key_vault_secret_id = azurerm_key_vault_secret.ctp.versionless_id
    identity            = azurerm_user_assigned_identity.purchase.id
  }
}
```

```yaml
env:
  CTP_CLIENT_SECRET: ${secret:ctp-client-secret}   # the name declared above
```

**On GCP** it is a Secret Manager secret name, resolved by Cloud Run itself
through `secretKeyRef`. During planning, evolve-deploy resolves the selected
secret version to its numeric ID using metadata only. An omitted version means
`latest` at deployment time. Use `${secret:NAME/versions/7}` for a specific version;
old revisions keep their numeric pins when instances restart.

## `${env}` in a reference

`${env}` expands to the environment name, so a reference can be written once for
every environment:

```yaml
envFrom:
  - ${param:/evolve/${env}/purchase/setup}
```

In `deploy/tst.yaml` that reads `/evolve/tst/purchase/setup`; in
`deploy/prd.yaml`, `/evolve/prd/purchase/setup`. One file shape, every
environment. See [Templating](../templating/).

## Where `${param:}` reads from

| Cloud | Store | Configured by |
|---|---|---|
| AWS | SSM Parameter Store | nothing — the region in `cloud` |
| GCP | Secret Manager | nothing — the project in `cloud` |
| Azure | App Configuration | `cloud.app_config` |

On Azure the `app_config` URL is only needed if a `${param:...}` reference is
actually used. A config with none does not need it.

## Parameter Manager snapshots on GCP

```yaml
env:
  PARAMETER_MANAGER_PROJECT: my-project
  PARAMETER_MANAGER_PARAMETER: purchase
  PARAMETER_MANAGER_VERSION: ${parameter-version:purchase}
```

`${parameter-version:NAME}` resolves Parameter Manager's `latest` selector once
per plan and writes the concrete version ID as a literal. The same selector is
shared across targets, including jobs. Each new plan resolves it again. A missing
or disabled selected version fails planning. Explicit pins use
`${parameter-version:purchase/versions/config-a}`. Full
`projects/PROJECT/locations/LOCATION/parameters/NAME/versions/VERSION` paths are
also accepted; bare names use `cloud.project` and the `global` location.

The app must pass the injected version to its Parameter Manager loader. This keeps
a restarted blue/green revision on the configuration it was deployed with. Its
parameter document must also use numeric nested secret references, and Terraform
must retain both parameter and secret versions. Existing revisions that still read
`latest` are not changed retroactively.

The lookup reads metadata, so it works with `refs.resolve: deny`. The deploy
identity needs `parametermanager.parameterVersions.get`; pinning native secrets
needs `secretmanager.versions.get`. Runtime payload permissions remain separate.
Other clouds refuse this reference kind. Put it in `env` or `strategy.env`;
`envFrom` remains a map of literal values read through `${param:...}`.

A traffic rollback reuses the old revision's pins. It does not restore job
templates. Replaying an old image with a floating selector chooses current
configuration; select the saved concrete version explicitly for a replay.

## Everything resolves before anything is written

Every reference in the whole file is looked up during the plan. A mistyped
parameter name, a secret that Terraform never declared, a store you cannot
reach — all of it fails with nothing deployed, rather than halfway through a
release.
