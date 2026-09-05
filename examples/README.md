# `examples/` — ready-to-send requests

Complete request bodies for `POST /v1/requests`. Send them with your key
from `auth.api_keys` in the `X-API-Key` header (the `api_key` field the files
carry is the deprecated fallback; a header always wins over it):

```bash
curl -s -X POST localhost:8080/v1/requests \
  -H 'Content-Type: application/json' -H 'X-API-Key: your-tentacron-api-key' \
  -d @examples/demo-direct.json
# → {"id":"6f1c9be2…","state":"received","links":{"self":"/v1/requests/6f1c9be2…"}}

curl -s localhost:8080/v1/requests/<id> -H 'X-API-Key: your-tentacron-api-key'
```

The `target` names and resolvent types match
[`config.example.yaml`](../config.example.yaml).

| File | Shows |
|---|---|
| [`demo-direct.json`](demo-direct.json) | Array-style `time-series` container for a direct-mode target: two POST resolvents (`resolvent-pv1`, `resolvent-wind`) next to a pass-through measured series. |
| [`meme-poll.json`](meme-poll.json) | MEME-style canonical model with a `model.timeseries` name→object registry; one `resolvent-pv1` capacity-factor placeholder beside a literal demand series. Tentacron polls MEME's job to completion. |
| [`buem-buildings.json`](buem-buildings.json) | The real [buem-gateway](https://github.com/enerplanet/buem-gateway) contract (`POST /api/v1/buem/buildings`): a `resolvent-weather` placeholder at the payload root is resolved via [weather](https://github.com/enerplanet/weather)'s point-query GET (`provider`/`lat`/`lon`/`year`/`use_case` become query parameters) into the `{index, variables}` block BuEM requires — with `attach_resolvent: false`, so the forwarded payload carries no tentacron marker. |
| [`city2tabula-ignis.json`](city2tabula-ignis.json) | Two GET resolvents in a name→object registry: `resolvent-city2tabula` maps `country`/`osm_ids` onto query parameters (arrays join comma-separated) and picks one building out of the list response via `response_path: "0"`; `resolvent-ignis` fills the `{code}` path placeholder. The objects carry only API parameters, because a GET resolvent sends every field. |
| [`meme-with-buem.json`](meme-with-buem.json) | Target composition: a MEME model whose `heat_demand` series is produced by a BuEM run — `resolvent-buem` forwards its `payload` field through the `buem-building` target and substitutes the extracted load-profile timeseries, next to a plain `resolvent-pv1` resolved in the same request. |
| [`ignis-calculate.json`](ignis-calculate.json) | A proxy target: the payload is handed through to [ignis](https://github.com/THD-Spatial-AI/ignis)'s heating-demand calculation unresolved — the `code` field fills the `{code}` URL template (and is stripped from the body), the rest are TABULA overrides. |
| [`pvgis-hourly.json`](pvgis-hourly.json) | A real third-party backend without a shim: `resolvent-pvgis` calls [PVGIS](https://re.jrc.ec.europa.eu/pvg_tools/en/) with its fields renamed onto PVGIS parameters (`query_map`) and reshapes the hourly rows into a time-series object in kW (`response_map`). Run it for real with `make live`. |
| [`buildings-chain.json`](buildings-chain.json) | Resolvent chaining: `resolvent-city2tabula` looks the building up, `resolvent-ignis` takes its `code` from that result (`{"$from": "building", "path": "tabula_variant_code"}`), and `resolvent-buem`'s nested simulation payload takes storeys, U-values and the weather block from the three of them — one request runs the whole pipeline, level by level. |
| [`no-resolvents.json`](no-resolvents.json) | A payload with only literal series — nothing to resolve, forwarded as-is. |
| [`big-numbers.json`](big-numbers.json) | Integer ids above 2^53 in the payload and the resolvent; the golden suite proves they reach the target byte-exact. |

Every file here is executed by the golden end-to-end suite
([`test/e2e`](../test/e2e)) against an in-process tentacron on every
`go test ./...` run — an example that drifts from the actual contract fails
CI. The frozen transcripts (accept response, final state, the exact payload
and requests the upstreams received, the audit trail) live under
[`test/e2e/testdata/golden/example-*.golden.json`](../test/e2e/testdata/golden).
