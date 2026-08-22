---
title: How it works
description: Stateless by design, a lockfile in git, and a hard boundary between what Terraform owns and what a deploy owns.
sidebar:
  order: 2
---

Three ideas carry the whole tool. Everything else follows from them.

## 1. The cloud is the state

Most deploy tooling keeps a record of what it did. That record can be lost,
locked, corrupted, or simply wrong — and when it is wrong, the tool is confidently
wrong with it.

`evolve-deploy` keeps none. Every run starts by asking the platform what is
actually running: the image on the revision, the environment on the task
definition, the weights in the traffic block. It compares that against the
config, and applies the difference.

Which gives you three properties for free:

- **Runs are idempotent.** A second `apply` finds nothing to do, because the
  first one already made the cloud match the file.
- **Drift is visible.** If someone changed something by hand, the next `diff`
  shows it — there is nothing for the tool to be out of date about.
- **There is nothing to unlock.** Two pipelines racing is a question about your
  pipelines, not a corrupted lock file to clear.

## 2. The config file is a lockfile

One file per environment. It names every service and the version it should be
running.

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

Because the file is in git and nothing else decides what is deployed:

- `git log deploy/prd.yaml` is your production deploy history, with authors,
  dates and review.
- `git revert` is a rollback, and it goes through the same review as anything
  else.
- `git diff` between two environments answers "what is on tst that is not on
  prd yet".

The config path is the only positional argument, and the only thing that decides
which environment is touched. There is no `--environment` that could contradict
the file you named.

## 3. Terraform owns the infrastructure, this owns the version

The boundary is deliberate and it is narrow:

| Terraform | evolve-deploy |
|---|---|
| Cpu, memory, probes, scaling | The image tag |
| Networking, IAM, identity | The environment variables you declare |
| Load balancers, listener rules, target groups | Which side serves traffic |
| Queues, event source mappings | Nothing else |
| Secret *declarations* on a resource | (never a secret value) |

This is not a division of labour invented for tidiness. It is what makes both
halves work:

- A deploy runs twenty times a day and must be fast, safe and boring. It touches
  one field.
- Infrastructure changes go through plan and review. They touch everything else.

Your IaC needs one change so that a `terraform apply` does not roll the image
back to whatever the module's default was — see [What Terraform must
do](../../infrastructure/terraform/).

### Why "it reads, it does not write"

When a blue-green release needs a traffic block, the tool does not create one.
It reads the block Terraform declared and refuses if it is not there, naming
what is missing. Same for ECS listener rules, same for Azure secret declarations,
same for Cloud Run scale settings.

A tool that quietly creates the infrastructure it needs produces resources that
nobody's IaC knows about, which the next `terraform apply` then destroys. So it
refuses instead, and the refusal tells you what to declare.

## The shape of a run

Every command follows the same three phases. Nothing is written until the first
two are complete.

**Plan.** Read the config, resolve every reference, expand every template,
check that every image exists, read the current state of every target, and work
out the difference. Anything wrong here stops the whole run before anything is
touched — a mistyped reference, a missing image, a secret that Terraform never
declared, a `depends_on` cycle, a hook that names a variable that does not exist.

**Gate.** Every service's `before` hooks run, all of them, concurrently. Only if
every one exits zero does anything get written. A schema check that goes red
means the release is already lost, and rolling the other services out anyway
leaves an environment half a version ahead.

**Rollout.** Services deploy concurrently — sixteen at a time by default,
respecting any [`depends_on`](../../deploying/ordering/). A release takes about
as long as its slowest service rather than the sum of all of them. Each service's
`after` hooks run once its own targets have all succeeded.

A failure in each phase is contained differently, and that is covered in
[Failure and recovery](../../deploying/failure/).
