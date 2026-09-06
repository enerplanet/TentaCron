# Browser clients

A page can call the API directly with `fetch`: submit a request, wait for
it with a long-poll, download the result as a file, cancel it. This page
walks through that with the code of the
[example client](https://github.com/enerplanet/tentacron/tree/main/examples/browser),
a single HTML file with no build step; every snippet below is quoted from
it, so what you read is what runs.

## Allow the page's origin

Off by default: with no origin listed, the service sends no
`Access-Control-*` header and the browser blocks every cross-origin call.
List the page's origin, and only that, under `server.cors`:

```yaml
server:
  cors:
    allowed_origins: ["https://app.example.org"]   # exact origin: scheme, host, port
```

An origin is `scheme://host[:port]`, no path and no trailing slash.
Preview deployments fit a subdomain wildcard
(`https://*.preview.example.org`, any depth below the host, never the host
itself); `*` allows every origin and makes `tentacron validate` print a
warning, because a key in a page on any origin is a key on every origin.
A reload (`SIGHUP`) applies the change without a restart. The
[configuration reference](configuration.md#server) lists the other knobs:
credentials for a page behind a cookie-based proxy, the preflight max-age,
Chrome's private-network preflights, and extra headers a proxy adds.

Check the answer before writing any JavaScript:

```bash
curl -si -X OPTIONS localhost:8080/v1/requests \
  -H 'Origin: https://app.example.org' -H 'Access-Control-Request-Method: POST'
# HTTP/1.1 204 No Content
# Access-Control-Allow-Origin: https://app.example.org
# Access-Control-Allow-Methods: GET, HEAD, POST, DELETE, OPTIONS
# Access-Control-Allow-Headers: Content-Type, X-API-Key, Idempotency-Key, X-Request-ID, Range, Authorization
# Access-Control-Max-Age: 600
# Vary: Origin, Access-Control-Request-Method, Access-Control-Request-Headers
```

An origin that is not listed gets the same `204` without any
`Access-Control-*` header; the browser then blocks the request itself and
the service's request log carries `cors: denied` with the origin.

## One helper for every call

The key goes in `X-API-Key`. Every error is a JSON envelope
`{"error": {"code", "message"}}`, and a `429 queue_full` also carries a
`Retry-After` in seconds, which the service exposes so a page can read it:

```js
async function api(base, key, method, path, body, headers = {}) {
  const resp = await fetch(base + path, {
    method,
    headers: { "X-API-Key": key, ...(body ? { "Content-Type": "application/json" } : {}), ...headers },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 429) {
    const retryAfter = Number(resp.headers.get("Retry-After") || 1);
    const { error } = await resp.json();
    throw new Error(`${error.code}: ${error.message} (retry in ${retryAfter}s)`);
  }
  if (!resp.ok && resp.status !== 202) {
    const text = await resp.text();
    let message = text;
    try { message = JSON.parse(text).error.message; } catch {}
    throw new Error(`${resp.status} ${message}`);
  }
  return resp;
}
```

The browser preflights the first `POST` and `DELETE` to each URL and caches
the answer for the max-age (ten minutes by default), so a page makes one
extra round trip per method and URL, not per call.

## Submit once per click

`POST /v1/requests` answers `202` with the request's id at once. An
`Idempotency-Key` per user action makes a retry of the same click, a
double tap, a flaky network, return the same request instead of a
duplicate:

```js
async function submit(base, key, target, payload) {
  const resp = await api(base, key, "POST", "/v1/requests", { target, payload }, { "Idempotency-Key": crypto.randomUUID() });
  return resp.json(); // { id, state, links }
}
```

## Wait with a long-poll

`GET /v1/requests/{id}?wait=25s` returns as soon as the request ends, or
after the wait window with the current state; loop until the state is
terminal instead of polling every second:

```js
async function waitForResult(base, key, id) {
  for (;;) {
    const resp = await api(base, key, "GET", `/v1/requests/${id}?wait=25s`);
    const job = await resp.json();
    if (["completed", "failed", "cancelled"].includes(job.state)) return job;
  }
}
```

The wait is capped a few seconds below `server.write_timeout`; a reverse
proxy in front must allow at least that long, see
[operations](operations.md#reverse-proxy).

## Download the result as a file

`GET /v1/requests/{id}/result` streams the stored result, a JSON document
or a file such as a simulation bundle. The filename is in
`Content-Disposition`, which the service exposes to pages; `Range` is
allowed, so a large bundle can be resumed:

```js
async function downloadResult(base, key, id) {
  const resp = await api(base, key, "GET", `/v1/requests/${id}/result`);
  const disposition = resp.headers.get("Content-Disposition") || "";
  const name = /filename="?([^";]+)"?/.exec(disposition)?.[1] || `${id}.bin`;
  const url = URL.createObjectURL(await resp.blob());
  const a = Object.assign(document.createElement("a"), { href: url, download: name });
  a.click();
  URL.revokeObjectURL(url);
  return name;
}
```

## Cancel

`DELETE /v1/requests/{id}` cancels a queued request or one awaiting its
target and answers `200` with the cancelled request; `409 not_cancellable`
while a worker is processing it or once it has ended:

```js
async function cancel(base, key, id) {
  const resp = await api(base, key, "DELETE", `/v1/requests/${id}`);
  return resp.json();
}
```

## The key in a page

A key embedded in a page is visible to everyone who can open the page.
That is a supported deployment with named limits, not an accident:

- Give the page **a key of its own** with the `client` role, so it sees
  only its own requests, and revoke that one key if it leaks.
- Bound it with the per-key ceilings: `max_queued` caps how many requests
  it may hold, `max_concurrent` how many run at once, `max_priority` how
  far it may jump the queue, `max_schedules` how many schedules it may
  create. See [configuration](configuration.md#auth).
- Never use an `admin` key in a page, and never combine `*` with a key
  that must stay secret.
- Where the key must not be visible at all, put a **backend-for-frontend**
  in front: the page talks to your own server, which holds the key and
  talks to TentaCron. CORS is then not needed at all.

CORS is browser policy, not access control: a client that is not a
browser ignores it, and the API key remains the authentication.

## Two situations that need a knob

**A cookie-based proxy in front.** If a page reaches TentaCron through a
proxy that authenticates with a session cookie, the browser must send
credentials on cross-origin calls: set `server.cors.allow_credentials:
true`, pass `credentials: "include"` to `fetch`, and list the exact
origin, since the Fetch specification forbids `*` with credentials.

**A public page, a private instance.** Chrome asks before a page on the
public internet may call an address on the local network or `localhost`;
set `server.cors.allow_private_network: true` to answer it. The service
must still be reachable from the user's machine, and the page must be
served over HTTPS for Chrome to allow it at all.

## Run the example

```bash
cd examples/browser && python3 -m http.server 5173
```

Allow `http://localhost:5173` in the service's configuration, open
<http://localhost:5173/>, enter the API base and a key, and use the four
buttons. The example's README lists what to check in the browser's
network panel; it is the manual browser check of every release.
