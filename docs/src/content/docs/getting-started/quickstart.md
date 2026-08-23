---
title: Your first deploy
description: Write a config file, read the plan, and roll it out.
sidebar:
  order: 4
---

This walks from nothing to a deployed version. It assumes you already have
something running that Terraform created — a Container App, a Cloud Run service,
an ECS service — because this tool deploys to infrastructure, it does not create
it.

## 1. Tell your IaC to let go of the image

One line, once, per resource. Without it the next `terraform apply` rolls the
image back to whatever the module declared:

```hcl
# azure_container_app
lifecycle { ignore_changes = [template[0].container[0].image] }
```

The equivalents for every platform are in [What Terraform must
do](../../infrastructure/terraform/). Do this first — it is the only change to
your infrastructure the tool needs, and forgetting it produces a deploy that
works and then quietly reverts.

## 2. Write the config

One file per environment, in the repository that holds the code. The filename
is the environment name, so call it after the environment:

```yaml
# deploy/tst.yaml
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

That is a complete, valid file. `version` is the image tag; `targets` names the
resource to set it on. Nothing here mentions environment variables, so none are
touched — every variable stays exactly as Terraform left it.

:::tip[There is a shorter form]
A service with exactly one target named after itself does not need the `targets`
block at all:

```yaml
services:
  purchase:
    version: 27ec167
    type: container-app
```

The target is then `container-app/purchase`. Use the long form when the name
differs or when there is more than one target.
:::

## 3. Read the plan

`diff` is read-only. It resolves everything, checks the image exists, reads the
live state and prints what `apply` would do:

```console
$ evolve-deploy diff deploy/tst.yaml

purchase  27ec167
  container-app/evolve-tst-purchase                  c2a1950 -> 27ec167

1 target in 1 service
```

If the config is wrong, this is where you find out. A tag that was never pushed,
a resource that does not exist, a subscription you cannot reach — all of it
fails here, having changed nothing.

If everything already matches, it says so and there is nothing to apply.

## 4. Roll it out

```console
$ evolve-deploy apply deploy/tst.yaml

purchase  27ec167
  container-app/evolve-tst-purchase                  c2a1950 -> 27ec167

  container-app/evolve-tst-purchase                  27ec167 in 1m21s

done in 1m28s
```

The tool waits until the target is actually healthy — a Container Apps revision
becoming the ready one, a Cloud Run ready condition, ECS `services-stable` —
rather than returning as soon as the API accepted the write.

Run it again and it does nothing: the cloud now matches the file.

## 5. Add the rest of the services

Services roll out concurrently, so a release takes about as long as its slowest
service rather than the sum of them:

```yaml
services:
  purchase:
    version: 27ec167
    type: container-app

  discover:
    version: 27ec167
    type: container-app

  site:
    version: 27ec167
    type: container-app
```

## Where to go from here

Pick whichever of these is your next actual problem:

| You want to | Read |
|---|---|
| Deploy a test build from CI without committing | [Templating](../../configuration/templating/) — `--set` |
| Manage environment variables here too | [Environment variables](../../configuration/environment/) |
| Reference a secret without putting it in git | [References and secrets](../../configuration/references/) |
| One image, several deployables (a service plus its jobs) | [The config file](../../configuration/config-file/) |
| Deploy the frontend only after the backend | [Ordering](../../deploying/ordering/) |
| Run a schema check before, publish after | [Hooks](../../deploying/hooks/) |
| Smoke test a version before anyone reaches it | [Blue-green](../../blue-green/overview/) |
| Wire this into GitHub Actions | [GitHub Actions](../../ci/github-actions/) |
