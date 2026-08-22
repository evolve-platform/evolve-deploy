---
title: Workflow recipes
description: Deploy on merge, plan on a pull request, promote to production, detect drift, and a rollback button.
sidebar:
  order: 2
---

Copy-paste starting points. All of them assume the [setup and cloud
authentication](../github-actions/) from the previous page.

## Plan on a pull request

`diff` changes nothing, so it is safe to run against production from a pull
request — and a plan in the review is the whole point of the config being a
lockfile.

```yaml
name: Plan

on: pull_request

permissions:
  id-token: write
  contents: read
  pull-requests: write

jobs:
  plan:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        env: [tst, prd]
    steps:
      - uses: actions/checkout@v5
      # ... cloud login ...
      - uses: evolve-platform/setup-evolve-deploy@v1
        with:
          version: v0.5.0
          token: ${{ secrets.GITHUB_TOKEN }}

      - name: Plan
        run: |
          {
            echo "### \`${{ matrix.env }}\`"
            echo '```'
            evolve-deploy diff deploy/${{ matrix.env }}.yaml 2>&1
            echo '```'
          } >> "$GITHUB_STEP_SUMMARY"
```

Give the credentials for this job read-only cloud permissions. Nothing here
needs to write, and a plan job that *could* deploy is a plan job that will,
eventually, by accident.

## Promote to production

Production deploys the version that is committed, not one substituted from the
pipeline. The version in `deploy/prd.yaml` is the release, and the pull request
that changes it is the change request.

```yaml
name: Deploy prd

on:
  push:
    branches: [main]
    paths: ['deploy/prd.yaml']
  workflow_dispatch:

concurrency:
  group: deploy-prd
  cancel-in-progress: false

permissions:
  id-token: write
  contents: read

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production        # a required reviewer lives here
    steps:
      - uses: actions/checkout@v5
      # ... cloud login, setup ...
      - run: evolve-deploy apply deploy/prd.yaml --verbose
        env:
          HONEYCOMB_API_KEY: ${{ secrets.HONEYCOMB_API_KEY }}
```

`paths:` means the workflow only runs when the file that decides what production
runs actually changed. A README commit does not redeploy production — and even
if it did, the run would be a no-op.

Pair it with a GitHub Environment carrying a required reviewer, which is the
approval gate without a tool that has to know what an approval is.

## Detect drift on a schedule

`diff --exit-code` exits non-zero when there is anything to apply. On production
that means: something changed that is not in git.

```yaml
name: Drift

on:
  schedule:
    - cron: '17 7 * * 1-5'
  workflow_dispatch:

permissions:
  id-token: write
  contents: read
  issues: write

jobs:
  drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      # ... read-only cloud login, setup ...

      - name: Check
        id: check
        run: |
          if ! evolve-deploy diff deploy/prd.yaml --exit-code > drift.txt 2>&1; then
            echo "drifted=true" >> "$GITHUB_OUTPUT"
          fi
          cat drift.txt

      - name: Open an issue
        if: steps.check.outputs.drifted == 'true'
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          gh issue create \
            --title "Production has drifted from deploy/prd.yaml" \
            --body "$(printf '```\n%s\n```' "$(cat drift.txt)")"
```

This catches a hand-made hotfix that nobody wrote down, and it catches Terraform
having moved a base task definition without a deploy since.

## A rollback button

`rollback` needs no arguments and does not ask for confirmation, so it makes a
good `workflow_dispatch`. During an incident, the fewer decisions the better.

```yaml
name: Rollback

on:
  workflow_dispatch:
    inputs:
      environment:
        type: choice
        options: [tst, prd]
        default: prd
      only:
        description: 'Limit to these services (comma separated). Leave empty for all.'
        required: false

permissions:
  id-token: write
  contents: read

jobs:
  rollback:
    runs-on: ubuntu-latest
    environment: ${{ inputs.environment }}
    steps:
      - uses: actions/checkout@v5
      # ... cloud login, setup ...

      - name: Where is it now
        run: evolve-deploy traffic deploy/${{ inputs.environment }}.yaml

      - name: Roll back
        run: |
          evolve-deploy rollback deploy/${{ inputs.environment }}.yaml \
            ${{ inputs.only && format('--only {0}', inputs.only) || '' }} \
            --verbose
```

Running `traffic` first means the log records what it was before, which the
incident review will want and nobody will otherwise have.

Note that this rolls back **traffic**, not the config. The file still names the
new version, so the next `apply` deploys it again — fix forward, or revert the
commit. [Rollback and traffic](../../blue-green/rollback/) covers the
distinction.

## Deploy only what was rebuilt

In a monorepo, the pipeline knows which services it just built. `--only` and
`--set` are how it says so, and a `depends_on` naming a service that is not in
the run is [simply satisfied](../../deploying/ordering/) rather than pulling it
in.

```yaml
- name: Deploy the changed services
  run: |
    args=()
    for svc in ${{ needs.build.outputs.changed }}; do
      args+=(--set "${svc}=${GITHUB_SHA::7}")
    done
    evolve-deploy apply deploy/tst.yaml \
      --only "$(echo '${{ needs.build.outputs.changed }}' | tr ' ' ',')" \
      "${args[@]}"
```

## Not GitHub Actions

Nothing above is specific to it. The tool is one binary that reads a config file
and cloud credentials from the environment — GitLab CI, Buildkite, Jenkins,
Azure Pipelines or a laptop all work the same way. Only [the setup
action](../github-actions/) is GitHub-shaped, and it is four lines of `curl` and
`tar` if you need to write it yourself; [Install](../../getting-started/install/)
has them.
