## What and why

<!-- One paragraph: the change, and the problem or item it addresses.
     Link the issue: Closes #123 -->

## How it was tested

<!-- Which suites you ran (make lint, make test-race, make e2e, make live),
     whether golden files were regenerated, and why each golden diff is
     intended. -->

## Checklist

- [ ] The change is scoped to one concern and the commits follow
      [Conventional Commits](../docs/getting-started/commit-conventions.md)
- [ ] `make lint` and `make test-race` pass locally
- [ ] Golden files were regenerated only for intended behaviour changes, and
      the diff was reviewed
- [ ] New parsers of untrusted input have a fuzz target; new background
      loops take an injected clock
- [ ] Documentation is updated where behaviour or usage changed:
      `docs/`, `docs/openapi/openapi.yaml`, `config.example.yaml`,
      `environment/config.yaml`, `README.md`
- [ ] `CHANGELOG.md` has an entry under *Unreleased*; a stricter validation
      rule or a changed response is listed under *Changed* with the keys or
      fields concerned
- [ ] No credentials or private data are included (config examples use
      `${ENV}` references)
