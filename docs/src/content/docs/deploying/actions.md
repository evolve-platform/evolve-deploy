---
title: Actions
description: uses honeycomb, sentry, http and job — the hooks that were never really commands.
sidebar:
  order: 4
---

A hook can be a named action instead of a command line:

```yaml
after:
  - uses: honeycomb
    with: { dataset: purchase }
```

## Why these exist

A Honeycomb marker written as `curl` is six lines of flags, a hand-built JSON
body, a header out of an environment variable and a `|| echo` on the end so a
failed annotation does not fail the release — and **every value in it is
something the tool already knows**: the version, the service, the environment,
the side.

So the set stays small and stays about what a deploy *is* — say a version went
out, ask whether something answers — and `cmd` covers everything else.

An action can also **refuse while there is still a plan to refuse**. A marker
whose API key is nowhere in the environment fails the plan, with the name of the
variable, rather than turning up as a 401 from an `after` hook on a release that
already succeeded.

## `uses: honeycomb`

Marks the deploy on a dataset.

```yaml
after:
  - uses: honeycomb
    with:
      dataset: purchase
      endpoint: https://api.eu1.honeycomb.io
      url: https://github.com/evolve-platform/evolve-reference-b2b/commit/{{.version}}
```

| Option | | Default |
|---|---|---|
| `dataset` | the dataset to mark; `__all__` marks the whole environment | **required** |
| `message` | | `{{.name}} {{.version}}` |
| `type` | groups markers so they share a colour | `deploy` |
| `url` | what the marker links to, usually the commit | none |
| `endpoint` | | `https://api.honeycomb.io` |
| `key_env` | which variable holds the key | `HONEYCOMB_API_KEY` |

EU tenants need `endpoint: https://api.eu1.honeycomb.io`.

## `uses: sentry`

Registers the release and then the deploy of it — two things Sentry knows,
because the same release is deployed to tst and later to prd, and only the
second call differs.

```yaml
after:
  - uses: sentry
    with:
      org: evolve
      commit: '{{.version}}'
      repository: evolve-platform/evolve-reference-b2b
```

| Option | | Default |
|---|---|---|
| `org` | | **required** |
| `project` / `projects` | one, or several | `{{.name}}` |
| `version` | what Sentry calls a release | `{{.version}}` |
| `environment` | | `{{.env}}` |
| `repository` + `commit` | associates the release with what is in it | none |
| `endpoint` | | `https://sentry.io/api/0` |
| `key_env` | | `SENTRY_AUTH_TOKEN` |

## Neither of those can fail a release

An annotation that did not arrive is reported and forgiven. An `after` hook runs
on a deploy that has already succeeded, and pulling a working version because a
note about it went missing costs more than the missing note.

That is what the `|| echo` on the end of every curl line was already doing — in
one place instead of fourteen.

## `uses: http`

Asks for one url and says whether the answer was the expected one.

```yaml
strategy:
  smoke:
    - uses: http
      with: { url: '{{url_stage "site"}}/healthz', retry: 5, delay: 2s }
```

| Option | | Default |
|---|---|---|
| `url` | | **required** |
| `method` | | `GET` |
| `headers` / `body` | | none |
| `status` | the one status that counts as healthy | any 2xx |
| `timeout` | bounds one attempt | `10s` |
| `retry` | further attempts after the first | `0` |
| `delay` | between attempts | `3s` |

It replaces the row of flags this was written as — `--fail --silent
--show-error --max-time --retry --retry-delay --retry-connrefused` — where every
one had to be remembered, and leaving off `--fail` meant a 500 walked through
the gate.

**Retrying is what makes it usable as a smoke test.** A side that has staged is
not always answering the instant staging returns, and the first refused
connection is nearly always that rather than a broken deploy. It retries on a
refused connection as much as on a bad status.

A failure reports the status and what the body said, which is where a health
route explains itself.

Unlike the two above, this one **can** fail a release — that is the entire
point of it.

## `uses: job`

Runs a job once on the version being released and waits for it to finish — a
Container Apps job on Azure, a Cloud Run job on GCP.

```yaml
services:
  wagtail:
    version: abc1234
    before:
      - uses: job
        with: { name: suz-tst-caj-migrate }
    after:
      - uses: job
        with:
          name: suz-tst-caj-tasks
          command: [python, manage.py, loadperms]
```

| Option | | Default |
|---|---|---|
| `name` | the job, as Terraform named it | **required** |
| `command` | the whole command line, for this one run | the job's own |
| `container` | which container, in a job with sidecars | as for a target |
| `version` | the image tag to run | `{{.version}}` |

**The image is written onto the job before it runs.** A migration has to run
against what is about to go out, not what is serving — and on Cloud Run an
execution cannot be handed an image of its own. The job keeps that version
afterwards, and a second apply of the same release does not write it again.

**A `command` is for one run.** It replaces the job's command line, arguments
included, and Terraform's goes back once the run is over, whether it worked or
not. Without one the job runs what it was declared with.

**The job is Terraform's.** Its environment, secrets, identity and timeout are
left alone, and a job that does not exist is refused while planning rather than
created. So is `uses: job` on AWS, which has no jobs a hook can run.

There is no timeout of the tool's own: the job's is the one that counts, and the
platform fails the execution when it passes.

In `before` a failed run calls the release off like any other hook. In `after`
it is reported and rolls nothing back. A successful run prints nothing; a
failed one names the execution, and on Cloud Run links to its logs.

## Anything else stays a command

```yaml
smoke:
  - npm run smoke -- --base-url {{url_stage "site"}}
```

The tool has no opinion about your test suite and does not want one.
