---
title: GitHub Actions
description: setup-evolve-deploy installs the binary and puts it on PATH. Then authenticate to your cloud with OIDC and deploy.
sidebar:
  order: 1
---

There is an action that installs the binary, verifies it and puts it on `PATH`:
[`evolve-platform/setup-evolve-deploy`](https://github.com/evolve-platform/setup-evolve-deploy).

```yaml
- name: Setup evolve-deploy
  uses: evolve-platform/setup-evolve-deploy@v1
  with:
    version: v0.5.0
    token: ${{ secrets.GITHUB_TOKEN }}

- name: Deploy
  run: evolve-deploy apply deploy/tst.yaml --only site --set site=${GITHUB_SHA:0:7}
```

## Inputs

| Input | Required | Default | |
|---|---|---|---|
| `token` | yes | | Token with read access to the release repository |
| `version` | no | `latest` | Release tag, e.g. `v0.5.0` |
| `install-dir` | no | `${RUNNER_TEMP}/evolve-deploy` | Where the binary is written. Prepended to `PATH` |
| `repository` | no | `evolve-platform/evolve-deploy` | Repository to take the release from |

## Outputs

| Output | |
|---|---|
| `version` | The tag that was installed — resolved, when `latest` was asked for |
| `path` | Full path to the binary |

:::caution[Pin `version` in anything that deploys]
The default is `latest`, which resolves at run time: convenient for a scratch
workflow, and one upstream release away from a pipeline that deploys differently
than it did yesterday for reasons that are not in your git history.
:::

## About the token

`token` is required. `evolve-deploy` is a release of a *different* repository
than the one calling the action, and `gh release download` needs a token either
way.

- **Both repositories are public**, so a workflow's own
  `${{ secrets.GITHUB_TOKEN }}` is enough. Give the job `contents: read`.
- **If you fork it private**, the calling repository's `GITHUB_TOKEN` cannot
  reach it. Use a PAT or a GitHub App token with read access, and point
  `repository` at your fork.

## What the action does

- Picks the archive for the runner's own OS and architecture — `linux` and
  `darwin`, `amd64` and `arm64`, which is what the release publishes. Anything
  else is an error naming what was asked for.
- **Verifies the download against the release's `SHA256SUMS`.** Verified rather
  than trusted: the download is a binary that then runs with credentials to a
  live environment. A release predating that file installs with a warning
  instead of failing.
- Writes the binary under `RUNNER_TEMP` rather than `/usr/local/bin`, so it
  needs no write access outside the runner's workspace, and adds that directory
  to `GITHUB_PATH`.
- Runs `evolve-deploy version`, so a broken install fails in *that* step rather
  than in the deploy.

## Versioning

Tags are `vX.Y.Z`, with a moving `vX` alias per major. Reference the major
(`@v1`); `main` can carry unreleased changes.

## Authenticating to the cloud

The tool reads the standard credential chain for each cloud, so the official
login actions are all it needs. Use OIDC — there is then no long-lived cloud
credential in the repository at all.

### Azure

```yaml
permissions:
  id-token: write
  contents: read

steps:
  - uses: azure/login@v2
    with:
      client-id: ${{ vars.AZURE_CLIENT_ID }}
      tenant-id: ${{ vars.AZURE_TENANT_ID }}
      subscription-id: ${{ vars.AZURE_SUBSCRIPTION_ID }}
```

### AWS

```yaml
permissions:
  id-token: write
  contents: read

steps:
  - uses: aws-actions/configure-aws-credentials@v5
    with:
      role-to-assume: arn:aws:iam::513712104672:role/deploy
      aws-region: eu-west-1
```

The `account` in your config is checked against `sts:GetCallerIdentity`, so a
workflow that assumed the wrong role fails at plan time rather than deploying to
the wrong place.

### GCP

```yaml
permissions:
  id-token: write
  contents: read

steps:
  - uses: google-github-actions/auth@v3
    with:
      workload_identity_provider: ${{ vars.GCP_WIF_PROVIDER }}
      service_account: ${{ vars.GCP_DEPLOY_SA }}
```

## A complete workflow

Deploy on merge to `main`, at the commit that was merged:

```yaml
name: Deploy tst

on:
  push:
    branches: [main]

concurrency:
  group: deploy-tst
  cancel-in-progress: false

permissions:
  id-token: write
  contents: read

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: tst
    steps:
      - uses: actions/checkout@v5

      - uses: azure/login@v2
        with:
          client-id: ${{ vars.AZURE_CLIENT_ID }}
          tenant-id: ${{ vars.AZURE_TENANT_ID }}
          subscription-id: ${{ vars.AZURE_SUBSCRIPTION_ID }}

      - uses: evolve-platform/setup-evolve-deploy@v1
        with:
          version: v0.5.0
          token: ${{ secrets.GITHUB_TOKEN }}

      - name: Deploy
        run: |
          evolve-deploy apply deploy/tst.yaml \
            --set site=${GITHUB_SHA::7} \
            --set purchase=${GITHUB_SHA::7} \
            --var ref=${GITHUB_SHA} \
            --verbose
        env:
          HONEYCOMB_API_KEY: ${{ secrets.HONEYCOMB_API_KEY }}
          SENTRY_AUTH_TOKEN: ${{ secrets.SENTRY_AUTH_TOKEN }}
```

Three things worth copying from that:

**`concurrency` without `cancel-in-progress`.** Two deploys to one environment
racing is the one thing statelessness does not protect you from — and cancelling
one halfway is worse than queueing it.

**Secrets for hooks go in `env`, not on the command line.** A hook inherits the
environment of the process that ran `evolve-deploy`, which is how
`HONEYCOMB_API_KEY` reaches the `honeycomb` action. An action whose key is
missing [fails the plan](../../deploying/actions/), naming the variable, before
anything is deployed.

**`--verbose` in CI.** The log is the only account of the release you will ever
have, and it costs nothing there.

More patterns — production promotion, drift detection, a manual rollback button
— are in [Workflow recipes](../recipes/).
