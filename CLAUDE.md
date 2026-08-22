# evolve-deploy

## Checks to run after changing code

Run all four before saying a change is done, and before committing. CI runs
exactly these (`.github/workflows/pull-request.yaml`), so a green run here means
a green run there.

```sh
gofmt -l ./cmd ./internal          # must print nothing
go build ./...
go vet ./...
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --max-same-issues=0 --max-issues-per-linter=0
```

Notes that have already cost a red pipeline once:

- **`go vet` is not enough.** The lint job runs golangci-lint with `staticcheck`
  and `revive` on top of vet, and it catches things vet never will — most often
  `ST1005: error strings should not end with punctuation or newlines`. This
  repo writes long multi-line error messages, so that rule fires easily: end the
  format string on a word or a verb, never on a full stop.
- **Pass `--max-same-issues=0 --max-issues-per-linter=0`.** golangci-lint
  defaults to reporting the same issue at most 3 times. Without these flags
  "3 issues" can mean five, you fix three, and the next CI run fails again on
  the two it never showed you.
- **Pin the version.** CI uses `v2.12.2`. A newer local golangci-lint can be
  stricter or laxer than the one that decides the build.
- **`-race` on tests.** CI runs `go test -race ./...`, so run it that way here
  too rather than plain `go test`.
- The linter config is `.golangci.yml`. Do not widen it to make a finding go
  away; fix the finding.

## Writing

- Everything is in English, except `specs/initial.md`, which is Dutch.
- Comments say *why*, not *what*. Look at the surrounding code before adding
  any: the density here is high and deliberate, and the useful ones record a
  decision, a trap, or something that was got wrong once. A comment restating
  the line below it does not match the house style.
- The same goes for commit messages: prose that explains the reasoning, not a
  bullet list of files touched. Read `git log` before writing one.
- Error messages are part of the interface. A refusal names what it found and
  what to do about it, and it is worth as much care as the code that raises it.

## Shape of the thing

- No state file and no lock: the cloud already knows what it runs, and running
  the tool twice does nothing the second time. Any change that would need
  remembered state between runs is going the wrong way.
- Terraform owns the infrastructure; this tool owns the version. It reads what
  Terraform declared — traffic blocks, listener rules, scale rules — and refuses
  when something is missing rather than writing it.
- The choreography lives in `internal/plan` and knows nothing about clouds. If
  a change makes `plan` or `ui` switch on a provider or a target type, the
  difference belongs on a driver interface instead.
- A capability a driver cannot honour is an explicit refusal at plan time, never
  a quiet fallback to a lesser behaviour.

## Changelog

User-visible changes need a changie entry in `.changes/unreleased/`. While a
feature is still unreleased, amend its existing entry rather than adding a
second one for the same thing.
