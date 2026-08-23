---
title: Failure and recovery
description: Where a failure is contained depends on when it happens, and what waiting for healthy actually means.
sidebar:
  order: 5
---

## Waiting for healthy

The tool waits until a target is actually healthy: ECS `services-stable`, a
Cloud Run ready condition, a Container Apps revision becoming the ready one.
Services roll out concurrently, so a release takes about as long as its slowest
service rather than the sum of all of them.

That makes the platform's own timings the floor. A readiness probe with
`initialDelaySeconds: 60` means every deploy of that service takes at least a
minute however fast the tool is. `--verbose` shows exactly where the time goes.

A rollout that fails **does not wait out the clock**. On Container Apps the tool
reads the revision itself, so an image that cannot be pulled or a container that
crash loops fails in seconds with the platform's own message, rather than after
ten minutes of nothing happening.

## Where a failure is contained

| When | What is affected |
|---|---|
| While planning | Everything. Nothing is deployed at all. |
| A `before` hook | Everything. It runs ahead of every write. |
| During rollout | That service only. Its targets go back together. |
| An `after` hook | Nothing is rolled back. Reported and moved past. |
| A blue-green `smoke` | The staged side is switched off. Traffic untouched. |

**Anything found while planning stops everything.** A mistyped reference, a
missing image, a secret Terraform never declared, a `depends_on` cycle, a hook
naming a variable that does not exist.

**A failing `before` hook stops everything too.** Calling the release off there
costs nothing and leaves nothing half applied.

**A failure during rollout stays with its service.** Its targets go back
together, because they share an image and may have a contract with each other.
Services that already succeeded are left alone. Services that depended on the
failed one are reported as *not deployed* rather than as failures of their own.

The rollback that follows a failed rollout gets a much shorter budget than the
deploy did: it is restoring containers that were serving a moment ago, and if
that does not come back quickly, waiting will not fix it.

## After a failed release

A `direct` release that failed partway leaves some services on the new version
and some on the old. There is no transaction and there cannot be one. What there
is instead:

```sh
evolve-deploy diff deploy/tst.yaml     # exactly what is where
```

Because there is no state file, that answer is read from the cloud and is
correct regardless of how the previous run died.

To go back, revert the config and apply it:

```sh
git revert <the deploy commit>
evolve-deploy apply deploy/tst.yaml
```

To go forward, fix and re-run — the services that already succeeded are no-ops
the second time.

## After a failed blue-green release

A failed blue-green deploy is normally a **non-event**. The staged side never
served a request.

| Fails | What happens | Does a user notice? |
|---|---|---|
| the staged revision never becomes ready | switched off, traffic untouched | no |
| `smoke` | switched off, traffic untouched | no |
| the switch itself | traffic is put back where it was | no |
| putting it back | said explicitly; go and look | possibly |
| the cleanup after a successful switch | a warning; the deploy succeeded | no |

The fourth row is the reason the third is not the last word. Switching a side
back is itself a set of calls to the platform, and if one of those fails the
release has no reassurance to offer — so it does not offer one.

### It says which of those happened

A failed release reads as a release rather than as a row in a table of services:
the whole staged side is one unit of work, so a failure in it is one paragraph.
It names the phase, lists a line per target, and **ends** on what is serving,
because that is what gets read first:

```console
the release failed while staging blue after 10m38s:
  - container-app/evolve-prd-purchase: revision evolve-prd-purchase--0000039 never started (container main: CrashLoopBackOff, restarted 5 times). Logs: az containerapp logs show -g evolve-prd -n evolve-prd-purchase --revision evolve-prd-purchase--0000039 --container main --type console --tail 50
No traffic moved: green still serves the version it served before, and the blue revisions were switched off
```

That last sentence is the one that differs, and the situations it separates used
to look identical:

| | |
|---|---|
| nothing moved | `No traffic moved: green still serves the version it served before, and the blue revisions were switched off` |
| it moved and was put back | `The traffic is back on green, which serves the version it served before` |
| the staged side would not switch off | `Some of the staged side could not be put back — check the traffic on the targets below before retrying` |
| the traffic would not go back | `The traffic could not be put back everywhere — check it on the targets below before retrying` |
| only an `after` hook failed | `The deploy itself worked: blue is serving and nothing was rolled back` |

The two middle ones are where the reassurance would have been a lie, so there is
none: the list above the sentence names the targets it would have been a lie
about.

The cleanup also prints as it goes, so "is the staged side still running, and
still being charged for" is answered by the deploy log rather than by the portal.

And if a release succeeded but the version turns out to be bad,
[`rollback`](../../blue-green/rollback/) is one write.
