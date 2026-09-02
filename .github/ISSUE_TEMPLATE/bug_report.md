---
name: Bug report
about: Something in tentacron behaves differently from the documentation
title: ''
labels: ''
assignees: ''

---

**Describe the bug**
A clear and concise description of what the bug is.

**To Reproduce**
1. Relevant `targets`/`resolvents` entries of your config (credentials redacted)
2. The request body sent to `POST /v1/requests` (with `api_key` removed)
3. What you observed: the `state`, `error.code`/`error.message` from
   `GET /v1/requests/{id}`, and if possible the job's audit trail
   (`select from_state, to_state, detail from job_events where job_id = '…'`)

**Expected behavior**
A clear and concise description of what you expected to happen.

**Logs**
Relevant log lines (API keys are never logged; upstream excerpts are already
redacted).

**Environment**
 - tentacron version / commit:
 - Go version or `environment/` image tag:
 - OS:

**Additional context**
Add any other context about the problem here.
