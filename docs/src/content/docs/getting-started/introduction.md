---
title: Introduction
description: What evolve-deploy is, what it deliberately is not, and who it is for.
sidebar:
  order: 1
---

`evolve-deploy` rolls out application versions to cloud runtimes. It reads a
config file, compares it against what is actually running, and applies the
difference.

```console
$ evolve-deploy apply deploy/tst.yaml

purchase  27ec167
  container-app/evolve-tst-purchase                  c2a1950 -> 27ec167

site  27ec167
  container-app/evolve-tst-site                      5367b03 -> 27ec167

  container-app/evolve-tst-purchase                  27ec167 in 1m21s
  container-app/evolve-tst-site                      27ec167 in 1m27s

done in 1m45s
```

There is no state file and no lock — the cloud already knows what it runs — so
running it twice does nothing the second time.

## It was built for the Evolve Platform. It does not know that.

This exists because the [Evolve Platform](https://www.labdigital.nl/) deploys
composable commerce stacks to AWS, GCP and Azure, and wanted one deploy story
across all three. But nothing in the tool is about commerce, about Evolve, or
about any particular application.

What it actually requires of you is short:

- Your infrastructure is declared somewhere else — Terraform, in practice.
- Your workloads run on ECS, Lambda, Cloud Run, Azure Container Apps, Container
  App Jobs or Azure Function Apps.
- You want the version that is deployed to be a reviewable line in git.

If that describes your project, this will work for it. There is no registration,
no backend service and no account.

## The one-sentence contract

> I set the image and the environment on the running resource, and leave
> everything else alone.

Cpu, memory, probes, scaling, networking, IAM, load balancers, queues and event
source mappings stay with your IaC. That boundary is the whole design, and
[How it works](../how-it-works/) is about why it is drawn there.

## What you get for accepting that boundary

**A deployment lockfile.** One file per environment, with the version of every
service in it. Desired state in git, actual state in the cloud. `git log` is the
deploy history and `git revert` is a rollback.

**A plan before a release.** `evolve-deploy diff` resolves every reference,
checks that every image exists, picks the right container and compares the full
desired state — without touching anything. A broken reference or an image that
was never pushed is found before a release rather than halfway through one.

**Refusals instead of surprises.** A capability a driver cannot honour is an
explicit error at plan time, never a quiet fallback to something lesser. If
`keep_warm` means nothing on Cloud Run, asking for it there fails; it does not
silently do nothing.

**Blue-green, if you want it.** Stage the new version on the side carrying no
traffic, run a smoke test against the whole staged stack, switch in one write.
Or don't — `direct` is the default and covers most environments.

## What it deliberately does not do

- **It does not create infrastructure.** It reads what Terraform declared —
  traffic blocks, listener rules, scale rules — and refuses when something is
  missing rather than writing it.
- **It does not remember anything between runs.** No state file, no lock, no
  cleanup command. Anything that would need remembered state is not built.
- **It does not shift traffic gradually.** No 5% for a minute, then 20%. To hold
  at 5% and then decide something you need to know the error rate at 5%, and
  there is no metrics client here. The [smoke test](../../blue-green/overview/)
  checks the new version deliberately, before anyone reaches it.
- **It does not remove environment variables you did not ask it to remove.**
  The config sets; it never deletes. A variable that disappears from Terraform
  needs [an explicit flag](../../configuration/environment/) to be carried
  through.

## Where to go next

- [How it works](../how-it-works/) — the model, in one page.
- [Install](../install/) — a binary, or `go install`.
- [Your first deploy](../quickstart/) — a config file and a `diff`.
- [GitHub Actions](../../ci/github-actions/) — deploying from a pipeline.
