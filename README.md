# evolve-deploy

Stateless deployments to AWS, GCP, Azure and Kubernetes.

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

It reads a config file, compares it against what is actually running, and rolls
out the difference. There is no state file and no lock — the cloud already knows
what it runs — so running it twice does nothing the second time.

Terraform owns the infrastructure; this owns the version. The contract is one
sentence:

> I set the image and the environment on the running resource, and leave
> everything else alone.

## Documentation

**[deploy.evolve-platform.com](https://deploy.evolve-platform.com)** — install,
the config format, references and secrets, hooks, blue-green across three
clouds, what Terraform has to declare, and the CLI reference.

Worth starting at:

- [Introduction](https://deploy.evolve-platform.com/getting-started/introduction/)
  — what it is, and what it deliberately is not
- [How it works](https://deploy.evolve-platform.com/getting-started/how-it-works/)
  — the three ideas the whole thing rests on
- [Your first deploy](https://deploy.evolve-platform.com/getting-started/quickstart/)
- [GitHub Actions](https://deploy.evolve-platform.com/ci/github-actions/)

## Install

```sh
go install github.com/evolve-platform/evolve-deploy@latest
```

Or take a binary from the [releases](https://github.com/evolve-platform/evolve-deploy/releases).
It needs nothing else installed: no cloud CLI, no Terraform, no runtime. On a
GitHub runner, use
[setup-evolve-deploy](https://github.com/evolve-platform/setup-evolve-deploy).

## Versioned GCP configuration and rollback

Both `${param:secret-name/versions/7}` and `${secret:secret-name/versions/7}`
accept a numeric Secret Manager version (full `projects/P/secrets/S/versions/N`
paths work too). Without a suffix they keep the existing `latest` behavior.
A native secret reference is verified using version metadata, never its payload;
the deploy identity needs `secretmanager.versions.get` as well as the existing
permissions. Disabled or destroyed pinned versions fail planning.

An `envFrom` JSON document may contain whole `${secret:...}` or `${param:...}`
values. They follow the same target capabilities and `refs.resolve` policy as
explicit environment entries; explicit `env` entries win. Other strings remain
literal, including application templates containing `${...}`. Reference-shaped
values in older documents now opt into reference handling.

Pin the environment document itself and every secret it references, retain those
versions, and save the actual image versions supplied through `--set` for replay.
A runtime configuration store must also be addressed by an immutable version in
the container environment. Restoring traffic cannot restore a mutable store.

`rollback` compares versions within each service's sibling targets; different
services may have different tags. It refuses configs containing jobs or direct
targets because it cannot restore them by moving traffic. `--traffic-only`
explicitly leaves those targets unchanged. To restore jobs too, apply a saved
release config containing the old image versions, environment pins and commands.
Traffic rollback does not replay schema-publication hooks or undo external data.

## Development

```sh
task test       # run the tests
task format     # gofmt + go mod tidy, before committing
task lint       # golangci-lint
task build      # a local binary
```

CI runs formatting, `go test -race`, golangci-lint and a full goreleaser build.
Running the four above before pushing means a green run here is a green run
there — `go vet` alone is not enough, since the lint job adds staticcheck and
revive on top of it.

User-visible changes need a [changie](https://changie.dev) entry in
`.changes/unreleased/`. While a feature is still unreleased, amend its existing
entry rather than adding a second one for the same thing.

The docs site lives in [`docs/`](docs/) and is a separate build:

```sh
cd docs
pnpm install
pnpm dev         # http://localhost:4321
pnpm build       # astro check, build, and a link check over the output
```

Dependencies are held back until they are a fortnight old (`minimumReleaseAge`
in `pnpm-workspace.yaml`), so ranges there are deliberately loose enough to
leave a mature version to resolve to.

## License

[MIT](LICENSE)
