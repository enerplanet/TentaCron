# `examples/browser/` — a minimal browser client

One HTML file, no build step, no dependency: submit a request, wait for it
with a long-poll, download the result as a file, cancel it, and see what a
`429` tells a page. Its `fetch` code is what the
[Browser clients](../../docs/browser-clients.md) page quotes, and running
it is the manual browser check of every release.

## Run it

1. Allow the page's origin in the service's configuration and reload or
   restart:

   ```yaml
   server:
     cors:
       allowed_origins: ["http://localhost:5173"]
   ```

   With the development compose, set it in `environment/config.yaml` and
   `make -C environment run`.

2. Serve this directory and open it:

   ```bash
   cd examples/browser && python3 -m http.server 5173
   # open http://localhost:5173/
   ```

3. Enter the API base (`http://localhost:8080` for a local service), a key
   from `auth.api_keys`, a target, and use the four buttons. The log at the
   bottom shows every call and every error envelope.

## The release check

Run in Chromium and in Firefox, with the network panel open:

| Step | What to see |
|---|---|
| Submit | One `OPTIONS /v1/requests` answered `204` with `Access-Control-Allow-Origin: http://localhost:5173`, then the `POST` answered `202`. A second submit within ten minutes sends no new preflight: the answer is cached for `Access-Control-Max-Age`. |
| Wait for the result | `GET …?wait=25s` returns when the request ends; no preflight, a `GET` with only `X-API-Key` is simple enough not to need one. |
| Download the result | One `OPTIONS` for the result URL, then the `GET`; the file is saved under the name from `Content-Disposition`. |
| Cancel | One `OPTIONS` with `Access-Control-Request-Method: DELETE` answered `204`, then the `DELETE`; a queued request answers `200 cancelled`, a running one `409 not_cancellable`. |
| A `429` | Set the key's `max_queued` to 1, submit twice with a target that keeps the first request queued: the second submit logs `queue_full … (retry in Ns)` with the number from `Retry-After`. |
| A refused origin | Serve the page on another port (`python3 -m http.server 5174`), open <http://localhost:5174/> and submit: the console shows the browser's CORS error, the service's request log a line with `"origin":"http://localhost:5174"` and `"cors":"denied"`, and nothing at `warn` or above. |
| Reload | Add the second port to `allowed_origins`, send `SIGHUP` (`docker kill --signal=HUP …`), submit again from it: accepted, no restart. |
