# Contributing

Thank you for taking the time to contribute to **tentacron**.

This project welcomes contributions such as bug reports, feature requests,
documentation improvements, code changes, and general feedback. Please read
this guide before opening an issue or submitting a pull request.

## Code of Conduct

By participating in this project, you agree to follow the rules and
expectations described in the [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to Contribute

- Reporting bugs
- Requesting features or improvements (a new target or resolvent backend,
  for example)
- Improving documentation
- Fixing issues
- Reviewing pull requests
- Asking and answering questions

## Before You Start

- Read the [README](README.md) for the project purpose and quickstart, and
  [docs/architecture.md](docs/architecture.md) for the design.
- Check existing issues and pull requests to avoid duplicates.
- Use the issue templates.

## Reporting Bugs and Requesting Changes

Use the issue tracker at
<https://github.com/enerplanet/tentacron/issues> for bug reports, feature
requests and documentation issues. When reporting a bug, include:

- What you expected to happen and what actually happened
- The request body you sent (with the `api_key` removed) and the relevant
  target/resolvent entries of your config (credentials redacted)
- The job's state, `error.code`/`error.message` from `GET /v1/requests/{id}`
  and, if possible, its audit trail (`job_events`, see
  [docs/operations.md](docs/operations.md#inspecting-state))
- Relevant log lines (API keys are never logged; upstream excerpts are
  already redacted)
- The tentacron version/commit and Go version

## Development Workflow

### 1) Clone the repository

```bash
git clone https://github.com/enerplanet/tentacron.git
cd tentacron
```

Fork first if you do not have write access.

### 2) Create a branch

Branch names follow the org-wide
[branch naming convention](docs/getting-started/branch-naming.md) and are
linted on every pull request:

```bash
git checkout -b feat/short-description
```

Examples: `fix/poll-deadline-race`, `feat/resolvent-ignis-match`,
`docs/configuration-defaults`.

### 3) Make your changes

Keep changes focused and small where possible. Go >= 1.26 is required
(`go.mod`); the containerized environment in [environment/](environment/)
provides the toolchain if you prefer not to install it.

### 4) Test your changes

```bash
make lint           # go vet + golangci-lint incl. gofmt/goimports (CI runs the same)
make lint-openapi   # when docs/openapi/openapi.yaml changed (needs Node)
make test           # unit, integration and golden E2E suites
make test-race      # what CI runs: race detector, shuffled order
```

The golden end-to-end corpus under [test/e2e](test/e2e) freezes the
observable behaviour of the whole service. A behaviour change shows up as a
transcript diff; when the change is intended, run `make golden-update` and
review the golden diff like any other code. Every file in
[examples/](examples/) is executed by that suite as well — keep them in
sync with the contract. The test pyramid is described in
[test/README.md](test/README.md).

Update the documentation (`README.md`, `docs/`, `config.example.yaml`,
`CHANGELOG.md`) whenever your change affects usage or behaviour. An API
change also updates the OpenAPI description,
[docs/openapi/openapi.yaml](docs/openapi/openapi.yaml); a test fails until
it and the registered routes agree, and `make lint-openapi` runs the Redocly
lint CI applies, at the version pinned in the Makefile and the workflows.

### 5) Commit your changes

Commit messages follow
[Conventional Commits](docs/getting-started/commit-conventions.md) and are
linted on every pull request:

```bash
git commit -m "feat(resolver): support nested containers"
```

### 6) Push and open a pull request

```bash
git push -u origin <your-branch-name>
```

Open the pull request against `main`. Describe what changed and why, add
testing notes (which suites you ran, whether goldens were regenerated), and
link related issues (e.g. `Closes #123`). CI must pass: lint, the race
suite, the build with both reference configs validated, the fuzz smoke,
the OpenAPI lint, govulncheck, the image build, and the branch/commit
linters.

## Pull Request Checklist

- [ ] The change is relevant and scoped appropriately
- [ ] `make lint` and `make test-race` pass locally
- [ ] Golden files were regenerated only for intended behaviour changes
- [ ] Documentation and `CHANGELOG.md` are updated where applicable
- [ ] No credentials or private data are included (config examples use
      `${ENV}` references)
- [ ] Related issues are linked

## Documentation Contributions

Documentation lives in [docs/](docs/), published with MkDocs Material to
[GitHub Pages](https://enerplanet.github.io/tentacron) through
`.github/workflows/docs.yml` on every push to `main` that touches `docs/`
or `mkdocs.yml` (build it locally with
`pip install -r docs/requirements.txt && mkdocs serve`), plus the folder
READMEs. Keep wording clear and practical, prefer short
examples, check links and commands, and match the style of the existing
pages.

## Releasing

Releases are cut from `main` by a signed `vX.Y.Z` tag. The release
workflow refuses a tag whose version has no matching changelog section, so
the order is:

1. Move the *Unreleased* entries of `CHANGELOG.md` under a new
   `## [X.Y.Z] - YYYY-MM-DD` heading and leave *Unreleased* empty.
2. Set `version` in `CITATION.cff` to `X.Y.Z`.
3. Commit as `chore(release): X.Y.Z`, tag that commit
   (`git tag -s vX.Y.Z -m "tentacron X.Y.Z"`) and push the branch and the
   tag.

The workflow then checks the changelog section, runs the race suite,
publishes static binaries with checksums and the release notes extracted
from that section on the GitHub release, and pushes the multi-architecture
image to `ghcr.io/enerplanet/tentacron` tagged `X.Y.Z`, `X.Y` and, for a
final version, `latest`. The checks run against the tagged commit, so a tag
can only point at a commit whose changelog already carries the section:
cut releases forward, never by tagging an older commit.

## Licensing of Contributions

By contributing to this project, you confirm that your contribution is your
own work (or you have the right to submit it), and you agree that it will
be licensed under the same [MIT license](LICENSE) as this repository.

## Need Help?

If you are unsure where to start, open an issue and ask. Maintainers can
help point you in the right direction.
