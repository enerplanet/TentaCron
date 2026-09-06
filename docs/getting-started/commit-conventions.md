# Commit Message Conventions

TentaCron follows the [Conventional Commits](https://www.conventionalcommits.org/)
specification. Commit messages are validated on every pull request by
`.github/scripts/lint_commits.py`; a PR cannot be merged until all commit
messages pass the check.

## Format

```
<type>(<scope>): <subject>

<body>

<footer>
```

Only the first line is required. Body and footer are optional.

## Types

| Type | When to use |
|---|---|
| `feat` | A new feature or a new configuration key |
| `fix` | A bug fix |
| `docs` | Documentation changes only |
| `style` | Formatting or whitespace — no logic changes |
| `refactor` | Code restructuring without new features or bug fixes |
| `perf` | Performance improvements |
| `test` | Adding or updating tests, including golden files |
| `build` | Build system, toolchain, dependencies, container images |
| `ci` | CI workflows and their scripts |
| `chore` | Repository housekeeping that fits no other type |
| `revert` | Reverting an earlier commit |

## Rules

- **Use imperative mood** — write `add login endpoint`, not `added login endpoint`
- **No capital letter** at the start of the subject
- **No period** at the end of the subject
- **Max 100 characters** per line
- Scope is optional but recommended — use the package or area the change
  affects (e.g. `api`, `worker`, `store`, `config`, `e2e`, `docs`)

## Examples

```
feat(config): add per-target retry policy
fix(store): batch retention deletes below the sqlite parameter limit
docs: document the callback signature
test(e2e): freeze the cancellation contract
build: move to the go 1.26 toolchain
ci: run govulncheck on every push
refactor(worker): split the resolve pipeline
```

## Breaking changes

Add a `BREAKING CHANGE:` footer when a change is not backwards compatible.
Before 1.0 this applies to any change that makes an existing configuration
file fail validation or alters a documented API response:

```
feat(config): reject job timeouts below the target timeout budget

BREAKING CHANGE: configs whose worker.job_timeout is shorter than a
target's timeout plus the longest resolvent timeout no longer load
```

## Common mistakes

| Wrong | Correct |
|---|---|
| `updated readme` | `docs: update readme` |
| `Fix bug` | `fix: correct null pointer in parser` |
| `feat: Add new endpoint.` | `feat: add new endpoint` |
| `WIP` | `chore: scaffold route handler` |

## Fixing a failed commit lint check

When the **Validate commit messages** check fails on your PR, follow the
steps below depending on how many commits need fixing.

### Fix the most recent commit

```bash
git commit --amend -m "feat(scope): your corrected message"
git push --force-with-lease
```

### Fix multiple commits

Use an interactive rebase to reword each failing commit. The workflow log
tells you exactly which commit SHAs need fixing.

```bash
# Replace N with the number of commits in your PR
git rebase -i HEAD~N
```

In the editor that opens, change `pick` to `reword` for each commit you want
to fix, save and close. Git will pause at each one so you can enter the
corrected message. Then push:

```bash
git push --force-with-lease
```

!!! warning "Force push requires branch to be up to date"
    After `--force-with-lease`, the PR will automatically re-run the commit
    lint check. If someone else pushed to the branch in the meantime, the
    force push will be rejected — pull first with `git pull --rebase` and
    then push again.
