# Per-tenant database monitoring (PostgreSQL, MongoDB, FerretDB)

Status: proposed
Date: 2026-08-17

## 1. Problem

Nexfar runs one PostgreSQL database and one document database per tenant. We need
Gatus to answer, per tenant, two questions:

1. Is the database reachable and healthy?
2. Is it slow?

Gatus today cannot do this. `Endpoint.Type()` (`config/endpoint/endpoint.go:174`)
recognises only DNS, TCP, SCTP, UDP, ICMP, TLS, STARTTLS, HTTP(S), gRPC, WS and SSH.
A `tcp://` check proves a port accepts connections; it says nothing about database
health or query latency.

The document-database tier is mixed: some tenants run real MongoDB, others run
FerretDB (MongoDB wire protocol over PostgreSQL + the DocumentDB extension). Both
must be covered.

## 2. Goals and non-goals

**Goals**

- Native `postgres://` and `mongodb://` endpoint types, probed directly by Gatus.
- Latency measured as a first-class, trustworthy signal.
- A configurable representative read query per endpoint, whose latency *and*
  result are both assertable.
- Engine health exposed as a JSON body so thresholds are expressed as ordinary
  Gatus conditions in config, not as code.
- Per-tenant scoping reusing the existing `group` + `tenancy.root-domain`
  mechanism. No new tenancy concept.
- Graceful behaviour against FerretDB, whose diagnostic surface is much thinner
  than mongod's.

**Non-goals**

- Alerting configuration. Deferred by decision; endpoints will carry no `alerts:`
  block initially. The design must not preclude adding them later — it does not,
  since alerts are a per-endpoint field.
- Writing to the monitored databases. Every probe is read-only.
- Replacing a metrics/APM system. This is availability and latency monitoring, not
  time-series observability.
- Config generation/templating for the tenant loop. That lives in the deployment
  repo, not here.

## 3. Config surface

No new config package. Two new URL schemes, and the existing `body:` field carries
the optional probe query — exactly as SSH already uses `body:` to carry the command
(`config/endpoint/endpoint.go:557`).

| Scheme | Type |
|:--|:--|
| `postgres://`, `postgresql://` | `TypePostgres` |
| `mongodb://`, `mongodb+srv://` | `TypeMongoDB` |

```yaml
- name: postgres
  group: navarromed              # tenant -> subdomain, already works
  visibility: public
  url: "postgres://gatus_monitor:${DB_MONITOR_PASSWORD}@navarromed-db.internal:5432/navarromed?sslmode=require"
  interval: 1m
  client:
    timeout: 5s
  conditions:
    - "[CONNECTED] == true"
    - "[RESPONSE_TIME] < 200"
    - "has([BODY].connections) == true"
    - "[BODY].connections.used_pct < 80"
    - "has([BODY].longest_running_query_seconds) == true"
    - "[BODY].longest_running_query_seconds < 60"
```

Every `<` or `<=` condition is paired with a `has(...) == true` guard as a
separate list entry. Conditions are AND'd, so the guard is what makes an absent
field fail the check: an unguarded `<` against a field that was never collected
passes silently, which is exactly backwards during an incident. See
`docs/database-monitoring.md`, "`<` and `<=` conditions need a presence guard".

Defaults for the probe query when `body:` is empty: `SELECT 1` for Postgres,
`{"ping": 1}` for MongoDB.

The probe query's **result is exposed** under `probe` in the body, so a
representative read query can be asserted on directly:

```yaml
  body: "SELECT count(*) FROM orders WHERE created_at > now() - interval '1 hour'"
  conditions:
    - "[RESPONSE_TIME] < 500"
    - "[BODY].probe.value > 0"
```

`probe.value` is the first column of the first row; `probe.rows` is the row
count. This is what makes a probe more than a liveness ping: the query runs
against real tenant data and its answer is a condition.

Because `body:` is used, probe queries inherit this fork's `[NOW_EPOCH±N]`
placeholders for free, so a probe can express a sliding time window.

**Metrics collection is inferred, not configured.** `Endpoint.needsToReadBody()`
already reports whether any condition references `[BODY]`. When it returns false,
the metric queries are skipped entirely and only the liveness probe runs. HTTP, SSH
and gRPC all gate body work this way; we follow the same rule. There is no
`collect-metrics` flag.

Net new config concepts: zero.

## 4. Execution path

Two new files under `client/`, each a self-contained function mirroring the shape of
`client/grpc.go`'s `PerformGRPCHealthCheck`:

```go
// client/postgres.go
func QueryPostgres(url, probeQuery string, collectMetrics bool, cfg *Config) (connected bool, duration time.Duration, body []byte, err error)

// client/mongodb.go
func QueryMongoDB(url, probeCommand string, collectMetrics bool, cfg *Config) (connected bool, duration time.Duration, body []byte, err error)
```

Dispatch is two `else if` branches in `Endpoint.call()`, following the gRPC branch
verbatim:

```go
} else if endpointType == TypePostgres {
    result.Connected, result.Duration, result.Body, err = client.QueryPostgres(
        e.URL, e.getParsedBody(), e.needsToReadBody(), e.ClientConfig)
    if err != nil {
        result.AddError(err.Error())
        return
    }
}
```

### 4.1 Response-time semantics

`[RESPONSE_TIME]` = **connection establishment + probe query**, excluding metric
collection.

This is the decision that makes "slow database" trustworthy. A slow connect is
itself a symptom — an exhausted connection pool shows up there first — so it belongs
in the number. The cost of scraping `pg_stat_activity` does not; including it would
make response time a function of how many conditions you wrote.

The body exposes `connect_ms`, `probe_ms` and `metrics_ms` separately, so a slow
result can be attributed without guessing.

### 4.2 Connection lifecycle

Connections are opened and closed per check:

- Postgres: `sql.Open` + `SetMaxOpenConns(1)`, `defer db.Close()` on every path.
- MongoDB: `mongo.Connect` with `SetServerSelectionTimeout(cfg.Timeout)`,
  `defer client.Disconnect(ctx)` on every path.

No pooling across checks, and no state that can survive a config reload. This
matters because `listenToConfigurationFileChanges` re-runs the full
stop/reload/start cycle in-process every 30s when the config changes; a cached pool
would leak file descriptors, which is the exact class of bug `Endpoint.Close()`
exists to work around (upstream issue #536).

Cost: one handshake per check per tenant. At 40 tenants on a 1m interval that is
40 handshakes/min — negligible.

### 4.3 Dependencies

- Postgres: **none**. `github.com/lib/pq` is already a direct dependency of the SQL
  store (`go.mod:23`).
- MongoDB: adds `go.mongodb.org/mongo-driver`. Required regardless of FerretDB,
  because some tenants run real MongoDB.

## 5. Metrics body

Gauges and instantaneous measurements only. Gatus checks are stateless, so a
condition on a cumulative counter (`deadlocks`, `xact_rollback`, Mongo's
`opcounters`) has no previous value to diff against and is meaningless. Such fields
are deliberately excluded.

One documented exception: `cache_hit_pct_since_reset` derives from `blks_hit` /
`blks_read`, which accumulate since the last statistics reset. It is a cumulative
average, not a current rate — on a long-lived instance a recent regression barely
moves it. The field carries `_since_reset` in its name precisely so that nobody
writes a threshold against it expecting a live gauge. Every other field is
instantaneous.

Fields are pointers in the implementation so that an uncollectable value is
omitted rather than marshalled as a zero — including `in_recovery`, where a plain
`bool` would make a legitimate `false` indistinguishable from "never measured".

### 5.1 PostgreSQL

One round trip: a single `SELECT` of scalar subqueries against catalog views.

```json
{
  "connect_ms": 12,
  "probe_ms": 3,
  "metrics_ms": 8,
  "probe": { "rows": 1, "value": 1 },
  "version": "16.2",
  "in_recovery": false,
  "connections": { "used": 42, "max": 100, "used_pct": 42 },
  "longest_running_query_seconds": 4.2,
  "longest_idle_in_transaction_seconds": 0,
  "blocked_sessions": 0,
  "replication_lag_seconds": 0,
  "cache_hit_pct_since_reset": 99.7,
  "database_size_bytes": 1234567
}
```

```sql
SELECT
  current_setting('server_version')                                   AS version,
  pg_is_in_recovery()                                                 AS in_recovery,
  (SELECT count(*) FROM pg_stat_activity)                             AS connections_used,
  current_setting('max_connections')::int                             AS connections_max,
  COALESCE((SELECT max(extract(epoch FROM now() - query_start))
            FROM pg_stat_activity
            WHERE state = 'active' AND backend_type = 'client backend'), 0)
                                                                      AS longest_running_query_seconds,
  COALESCE((SELECT max(extract(epoch FROM now() - state_change))
            FROM pg_stat_activity
            WHERE state = 'idle in transaction'), 0)
                                                                      AS longest_idle_in_transaction_seconds,
  (SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock')
                                                                      AS blocked_sessions,
  CASE WHEN pg_is_in_recovery()
       THEN COALESCE(extract(epoch FROM now() - pg_last_xact_replay_timestamp()), 0)
       ELSE 0 END                                                     AS replication_lag_seconds,
  COALESCE((SELECT 100 * sum(blks_hit)::float8 / NULLIF(sum(blks_hit) + sum(blks_read), 0)
            FROM pg_stat_database), 0)                          AS cache_hit_pct_since_reset,
  pg_database_size(current_database())                                AS database_size_bytes;
```

Requires PostgreSQL 10+ (`backend_type`, `pg_monitor` both introduced in 10).

**The `pg_monitor` trap.** Without membership in `pg_monitor`, `pg_stat_activity`
shows only the current user's own sessions. `connections_used` then returns a small
number that is *silently wrong* rather than erroring — the worst possible failure
mode for a threshold. Mitigation: the collector first evaluates

```sql
SELECT pg_has_role(current_user, 'pg_monitor', 'member')
```

and, when false, omits `connections`, `longest_running_query_seconds`,
`longest_idle_in_transaction_seconds` and `blocked_sessions` from the body entirely
and appends an explanatory entry to `metrics_errors`. A missing field fails its
condition visibly; a wrong field does not.

Required grants per tenant database:

```sql
CREATE ROLE gatus_monitor WITH LOGIN PASSWORD '...';
GRANT pg_monitor TO gatus_monitor;
GRANT CONNECT ON DATABASE <tenant> TO gatus_monitor;
```

### 5.2 MongoDB and FerretDB — one implementation

A single `client/mongodb.go` serves both. It issues `hello`, then `serverStatus`,
then `replSetGetStatus`, and emits whatever succeeded. Commands that fail or return
absent fields are omitted from the body and recorded in `metrics_errors`. No config
flag distinguishes the two backends; the body is simply shaped differently.

A `backend` field (`"mongodb"` or `"ferretdb"`) is derived from the handshake.
FerretDB advertises itself in `buildInfo`/`serverStatus`; **the exact field name must
be confirmed against a live instance during implementation** — this design does not
depend on it beyond labelling.

Real MongoDB:

```json
{
  "connect_ms": 20, "probe_ms": 2, "metrics_ms": 9,
  "probe": { "ok": 1 },
  "backend": "mongodb", "version": "7.0.5",
  "is_writable_primary": true,
  "repl_set_state": "PRIMARY",
  "replication_lag_seconds": 0.4,
  "connections": { "current": 18, "available": 51182, "used_pct": 0.03 },
  "global_lock_queue_total": 0,
  "active_clients_total": 3,
  "uptime_seconds": 891234
}
```

FerretDB — the same code, a thinner body:

```json
{
  "connect_ms": 20, "probe_ms": 2, "metrics_ms": 4,
  "probe": { "ok": 1 },
  "backend": "ferretdb", "version": "2.x",
  "uptime_seconds": 891234,
  "metrics_errors": [
    "serverStatus: connections/globalLock not reported by this backend",
    "replSetGetStatus: not supported by this backend"
  ]
}
```

`replication_lag_seconds` for real MongoDB is computed from `replSetGetStatus` as
`primary.optimeDate - self.optimeDate`. On a standalone deployment the command
fails and the field is absent.

**Deliberately excluded: `opLatencies`.** It is a cumulative average since process
start, so it drifts and cannot express current latency. `[RESPONSE_TIME]` is the
latency signal.

### 5.3 Why FerretDB's thin body is not a coverage gap

FerretDB has no mongod internals to report — its connections, locks and replication
live in the backing PostgreSQL. The real signals are reachable by other endpoints
that need no new Go code:

- The backing PostgreSQL, via the `postgres://` type from §3.
- `GET /debug/readyz` — validates MongoDB **and** PostgreSQL connectivity and
  confirms the DocumentDB extension is installed.
- `GET /debug/livez` — whether FerretDB accepts wire-protocol connections.
- `/debug/metrics` — Prometheus format, no exporter needed. Not consumed by this
  design; noted for completeness. FerretDB documents this metric set as unstable
  across minor releases.

These listen on `127.0.0.1:8088` by default, so `--debug-addr` must be bound to an
address Gatus can reach.

The `mongodb://` check still matters for FerretDB tenants: `[CONNECTED]` and
`[RESPONSE_TIME]` measure the wire-protocol path the application actually takes,
through the facade and into PostgreSQL. That is the most faithful "this tenant's
database is slow" signal available.

## 6. Error handling

Three distinct failure classes, deliberately treated differently:

| Failure | Result |
|:--|:--|
| Cannot connect / probe query fails | `Connected = false`, error added, check fails. This is an outage. |
| Metric collection fails (permissions, unsupported command, standalone) | Check outcome unaffected. Partial body + `metrics_errors[]`, warn-level log. |
| A condition references an absent body field | That condition fails on its own terms, visibly. |

A missing `pg_monitor` grant is a configuration problem, not an outage; failing the
endpoint for it would train people to ignore red. Conversely, silently substituting a
default value for an uncollectable metric would let a threshold pass on fiction —
hence absent-not-defaulted.

Errors are sanitised before reaching `result.Errors`: any credentials appearing in a
driver error string are redacted, since `Result` is serialised to the API.

## 7. Security

- **DSN credentials do not leak to clients.** `Endpoint` carries no JSON tags and is
  never marshalled by the API; the API serialises `EndpointStatus`
  (`config/endpoint/status.go`), which exposes name, group, key, results and events.
  `result.Hostname` is set from `urlObject.Hostname()`, which strips userinfo.
  Verified, not assumed.
- Credentials come from the environment via the existing `os.ExpandEnv` pass over
  the raw config (`config/config.go:291`), so no plaintext password lands in the
  config file.
- The monitoring role is read-only: `pg_monitor` plus `CONNECT`. No table access is
  required by any query in §5.1. For MongoDB the equivalent is `clusterMonitor`.
- Endpoints must be marked `visibility: public` only when a tenant should see their
  own database status. Group membership alone does not expose them
  (`docs/multi-tenancy.md`).

## 8. Per-tenant endpoint sets

Tenant on real MongoDB — 2 endpoints:

```
postgres://…    application PostgreSQL
mongodb://…     MongoDB (full body)
```

Tenant on FerretDB — 3 or 4 endpoints:

```
postgres://…                  application PostgreSQL
mongodb://…                   latency through the facade
https://…:8088/debug/readyz   readiness: Mongo + PG + DocumentDB extension
postgres://…                  FerretDB's backing PostgreSQL, when separate
```

Conditions reference only the fields that tenant's backend actually provides. Since
the YAML is already per-tenant, this costs nothing.

## 9. Validation

- `ValidateAndSetDefaults` needs no new branch: `Type()` returning a known type is
  sufficient, and `postgres://host/db` parses cleanly through the existing
  `http.NewRequest` reachability check at `endpoint.go:272`.
- Reject a `postgres://` or `mongodb://` endpoint whose URL fails to parse as a DSN,
  with a named error in the style of `ErrEndpointWithNoURL`, so the failure is a
  startup panic with a clear message rather than a runtime error every interval.
- No ordering constraints are introduced in `parseAndValidateConfigBytes`.

## 10. Testing

The repository has no `testcontainers`, no `sqlmock`, and the SQL store's Postgres
path has no automated coverage — only SQLite is tested. This design does not pretend
otherwise. Three layers:

1. **Pure unit tests, no server** — scheme detection in `Type()`, probe-query
   defaulting when `body:` is empty, JSON body assembly from a fixture of scanned
   values, `metrics_errors` population, and credential redaction in error strings.
   These cover the logic most likely to regress.
2. **Failure-path tests, no server** — connecting to a closed port and to a
   malformed DSN returns `connected == false` with an error, respects
   `cfg.Timeout`, and leaks no connection. Follows the existing style of
   `TestCanCreateConnection` in `client/client_test.go`.
3. **Opt-in integration tests** — gated on `GATUS_TEST_POSTGRES_DSN` and
   `GATUS_TEST_MONGODB_URI`, skipped when unset. These are the only tests that can
   exercise the actual catalog queries and command shapes. A compose file under
   `.examples/` gives a one-command local Postgres, MongoDB and FerretDB to point
   them at.

Layer 3 is where the FerretDB assumptions in §5.2 get confirmed. Until it runs
against a real FerretDB instance, the shape of its thin body is a prediction.

## 11. Upstream compatibility

The change is additive and confined to: two `case` arms in `Type()`, two `else if`
arms in `call()`, two new files in `client/`, one new dependency. No existing
behaviour changes, which keeps rebases against TwiN/gatus cheap — the conflict
surface is two functions.

Worth offering upstream afterwards; `postgres://` in particular costs upstream no
new dependency either.

## 12. Documentation

- `docs/database-monitoring.md`, following the structure of `docs/multi-tenancy.md`:
  what it does, configuration, the required monitoring roles, the full body schema
  for each backend, the FerretDB caveats, and worked per-tenant examples.
- README.md: add the two types to the endpoint-type list, in alphabetical order.

## 13. Open questions

All four questions below remain open. The integration-test layer described in §10
that would have exercised a live FerretDB instance did not run during
implementation — the Docker daemon required to bring up
`.examples/docker-compose-database-monitoring/` was unavailable in the
implementation environment — so none of them were answered by a live run as
originally planned. `docs/database-monitoring.md` flags the FerretDB body shape as
an unconfirmed prediction for the same reason.

1. **Still open.** The exact FerretDB self-identification field in
   `buildInfo`/`serverStatus` (§5.2). `detectMongoBackend`'s heuristics (a
   top-level `ferretdb` key, or `"ferretdb"` inside the version string) are
   implemented but have never been checked against a real FerretDB response.
   Resolve by running `db.runCommand({buildInfo: 1})` against a live instance via
   `.examples/docker-compose-database-monitoring/`.
2. **Still open.** Whether FerretDB's `--debug-addr` is currently bound to a
   Gatus-reachable address in the Nexfar deployment, or whether that needs a
   change (§5.3).
3. **Still open.** Whether each FerretDB tenant's backing PostgreSQL is distinct
   from that tenant's application PostgreSQL, which decides whether §8 needs the
   fourth endpoint.
4. **Still open.** Thresholds. Every number in the examples here, and in
   `docs/database-monitoring.md`, is a placeholder; real values need a baseline
   from production before any of these conditions is meaningful.
