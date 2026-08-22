---
title: Actions
description: uses honeycomb, sentry and http — the hooks that were never really commands.
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
      with: { url: '{{url "site"}}/healthz', retry: 5, delay: 2s }
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

## Anything else stays a command

```yaml
smoke:
  - npm run smoke -- --base-url {{url "site"}}
```

The tool has no opinion about your test suite and does not want one.
