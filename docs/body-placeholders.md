# Body placeholders (Nexfar fork additions)

Upstream Gatus already substitutes a few placeholders in the request `body`
(`[ENDPOINT_NAME]`, `[ENDPOINT_GROUP]`, `[ENDPOINT_URL]`, `[RANDOM_STRING_N]`).
This fork adds dynamic timestamps and relaxes JSON `null` handling so that
log-store queries (Quickwit) can be expressed as plain endpoints.

## `[NOW_EPOCH]`, `[NOW_EPOCH-N]`, `[NOW_EPOCH+N]`

Replaced at request time with the current unix timestamp in seconds, plus or
minus an offset of `N` seconds. This lets a static body express a sliding
time window:

```yaml
endpoints:
  - name: Estabilidade
    group: medicamental
    url: https://quickwit.nexfar.com.br/api/v1/logs-app/search
    method: POST
    headers:
      Content-Type: application/json
    body: '{"query": "status:500 AND tenant:medicamental", "max_hits": 0,
            "start_timestamp": [NOW_EPOCH-900], "end_timestamp": [NOW_EPOCH]}'
    conditions:
      - "[STATUS] == 200"
      - "[BODY].num_hits < 5"
```

The substitution happens in `getParsedBody()` (config/endpoint/endpoint.go),
i.e. on every check, so the window slides with each poll.

## JSON `null` resolves to `"null"` instead of failing

Upstream, walking a body path that lands on a JSON `null` fails the condition
with a walk error. That breaks aggregations over empty windows — e.g. a
percentile over zero hits:

```json
{"aggregations": {"pct_time": {"values": [{"key": 99.0, "value": null}]}}}
```

This fork resolves such a path to the literal string `"null"`, which numerical
conditions then coerce to `0` — so "no traffic in the window" evaluates as
healthy rather than erroring. A genuinely *missing* key is still a condition
error, as upstream.
