# Database monitoring (PostgreSQL, MongoDB, FerretDB)

This is a Nexfar-specific addition to Gatus. It adds two native endpoint types,
`postgres://` and `mongodb://`, so a tenant's databases can be monitored directly —
connection health, query latency and engine metrics — instead of only the liveness
that a `tcp://` check on the same port would give you.

A `tcp://` check on port 5432 tells you something is listening. It tells you nothing
about whether PostgreSQL is accepting queries, how full its connection pool is, or
whether a query against real tenant data comes back correct. These two endpoint
types close that gap using the existing `body:` field and the existing per-tenant
`group` + `tenancy.root-domain` mechanism — no new config concepts.

The same mechanism covers [FerretDB](https://www.ferretdb.com/) tenants (MongoDB
wire protocol backed by PostgreSQL): the `mongodb://` type connects to it exactly as
it would to real MongoDB, and simply gets a thinner body back.

## Table of contents

- [Which scheme maps to which type](#which-scheme-maps-to-which-type)
- [The probe query and its defaults](#the-probe-query-and-its-defaults)
- [Metric collection is opt-in by condition](#metric-collection-is-opt-in-by-condition)
- [Response time semantics](#response-time-semantics)
- [PostgreSQL body schema](#postgresql-body-schema)
- [MongoDB body schema](#mongodb-body-schema)
- [FerretDB body schema](#ferretdb-body-schema)
- [Required monitoring roles](#required-monitoring-roles)
- [Startup validation](#startup-validation)
- [Worked per-tenant examples](#worked-per-tenant-examples)
- [A note on thresholds](#a-note-on-thresholds)

## Which scheme maps to which type

| Scheme                           | Type          |
|:----------------------------------|:--------------|
| `postgres://`, `postgresql://`    | `TypePostgres` |
| `mongodb://`, `mongodb+srv://`    | `TypeMongoDB`  |

Both connect and disconnect on every single check — no connection pooling across
checks, and no state that survives a config reload. `listenToConfigurationFileChanges`
re-runs the full stop/reload/start cycle in-process every 30s when the config
changes, and a cached pool would leak file descriptors across that cycle.

```yaml
endpoints:
  - name: postgres
    group: navarromed
    url: "postgres://gatus_monitor:${DB_MONITOR_PASSWORD}@navarromed-db.internal:5432/navarromed?sslmode=require"
    interval: 1m
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 200"
```

## The probe query and its defaults

`endpoints[].body` carries the probe: a `SELECT` for PostgreSQL, a command document
for MongoDB/FerretDB. When `body:` is blank, a default is used:

| Type      | Default probe            |
|:----------|:--------------------------|
| PostgreSQL | `SELECT 1`                |
| MongoDB / FerretDB | `{"ping": 1}`      |

Because `body:` is the mechanism, probe queries inherit this fork's
`[NOW_EPOCH±N]` placeholders for free, so a probe can express a sliding time
window (see the design's endpoint condition placeholders).

The probe's **result** is exposed in the body, not just its latency:

- PostgreSQL: `[BODY].probe.value` is the first column of the first row;
  `[BODY].probe.rows` is the row count.
- MongoDB / FerretDB: `[BODY].probe` is the raw command reply (e.g. `{"ok": 1}` for
  `ping`, or whatever fields the command you chose returns).

This is what turns a probe into more than a liveness ping: it can run against real
tenant data, and the answer is itself a condition.

```yaml
  body: "SELECT count(*) FROM orders WHERE created_at > now() - interval '1 hour'"
  conditions:
    - "[RESPONSE_TIME] < 500"
    - "[BODY].probe.value > 0"
```

## Metric collection is opt-in by condition

Nothing configures whether engine metrics are collected — Gatus infers it. If no
condition on the endpoint references `[BODY]`, only the connect + probe happens: no
`pg_stat_activity` scrape, no `serverStatus`/`replSetGetStatus` calls. This is the
same rule HTTP, SSH and gRPC endpoints already follow
(`Endpoint.needsToReadBody()`), applied consistently to the two new types.

Practically: an endpoint whose only condition is `[CONNECTED] == true` never touches
the metrics query. Add any `[BODY]...` condition and the full collection runs.

## Response time semantics

`[RESPONSE_TIME]` = **connection establishment + probe query/command**. Metric
collection is deliberately excluded from it.

A slow connect is itself a symptom — an exhausted connection pool shows up there
first — so it belongs in the number that alerts key off. The cost of scraping
`pg_stat_activity` or running `serverStatus` does not; including it would make
"is the database slow" a function of how many conditions you happened to write.

The body separates all three phases so a slow result can be attributed without
guessing:

```json
{
  "connect_ms": 12,
  "probe_ms": 3,
  "metrics_ms": 8
}
```

`connect_ms` + `probe_ms` always sum to `[RESPONSE_TIME]` (in milliseconds);
`metrics_ms` is extra work that happened after the timer that produces
`[RESPONSE_TIME]` had already stopped.

## PostgreSQL body schema

Every metric field is a pointer under the hood, marshalled with `omitempty`: a value
that could not be collected is **absent from the JSON body**, not present as a zero.
Treat this as a contract, not an implementation detail — a condition that references
an absent field fails visibly, on purpose. A defaulted zero would let a threshold
pass on a value that was never actually measured (imagine `[BODY].blocked_sessions
== 0` silently "passing" because the field was never collected).

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
  "cache_hit_ratio_since_reset": 0.997,
  "database_size_bytes": 1234567,
  "metrics_errors": []
}
```

| Field | Meaning |
|:--|:--|
| `connect_ms`, `probe_ms`, `metrics_ms` | Phase timings, in milliseconds. See [Response time semantics](#response-time-semantics). |
| `probe.value`, `probe.rows` | The probe query's result — first column of first row, and row count. |
| `version` | `server_version`. |
| `in_recovery` | `pg_is_in_recovery()`. A pointer so a legitimate `false` is distinguishable from "never measured". |
| `connections.used` / `.max` / `.used_pct` | Connection saturation. **Requires `pg_monitor`** — see below. |
| `longest_running_query_seconds` | Longest currently-active client query. **Requires `pg_monitor`.** |
| `longest_idle_in_transaction_seconds` | Longest session sitting `idle in transaction`. **Requires `pg_monitor`.** |
| `blocked_sessions` | Sessions currently waiting on a lock. **Requires `pg_monitor`.** |
| `replication_lag_seconds` | `0` when not in recovery; otherwise seconds since the last replayed transaction. |
| `cache_hit_ratio_since_reset` | See the callout immediately below — **read this before using it in a condition.** |
| `database_size_bytes` | `pg_database_size(current_database())`. |
| `metrics_errors` | Human-readable reasons any of the above are missing. Never fails the check by itself. |

> ⚠️ **`cache_hit_ratio_since_reset` is a cumulative average, not a live gauge.**
> It is `blks_hit / (blks_hit + blks_read)` from `pg_stat_database`, accumulated
> since the last statistics reset — typically server start, which on a long-lived
> production instance can mean months of history. A real, ongoing cache
> regression that started five minutes ago barely moves this number, because it is
> averaged against everything that came before it. The `_since_reset` suffix is in
> the field name specifically so nobody sets a threshold against it expecting an
> instantaneous rate. Every other field in this body is instantaneous; this is the
> one documented exception.

Cumulative counters proper — `deadlocks`, `xact_rollback`, and the like — are not
collected at all. Gatus checks are stateless: a counter has no previous value to
diff against, so a condition on it can never express anything meaningful.

## MongoDB body schema

The same code path serves both real MongoDB and FerretDB; only the body shape
differs, based on what the backend actually reports.

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

| Field | Meaning |
|:--|:--|
| `connect_ms`, `probe_ms`, `metrics_ms` | Phase timings, in milliseconds. |
| `probe` | The raw reply to your probe command (default `{"ping": 1}` → `{"ok": 1}`). |
| `backend` | `"mongodb"` or `"ferretdb"`, derived from `buildInfo`. See the FerretDB caveat below — **this label is not fully trustworthy yet.** |
| `version` | From `buildInfo`. |
| `is_writable_primary` | From `hello`. |
| `repl_set_state` | From `replSetGetStatus.myState`, decoded to a name (`PRIMARY`, `SECONDARY`, ...). Absent on a standalone deployment. |
| `replication_lag_seconds` | `primary.optimeDate - self.optimeDate` from `replSetGetStatus`. Absent when that command fails (standalone, or FerretDB). |
| `connections.current` / `.available` / `.used_pct` | From `serverStatus.connections`. |
| `global_lock_queue_total`, `active_clients_total` | From `serverStatus.globalLock`. |
| `uptime_seconds` | From `serverStatus.uptime`. |
| `metrics_errors` | Present whenever a diagnostic command failed or a backend didn't populate a section. Never fails the check by itself. |

**Deliberately excluded: `opLatencies`.** It is a cumulative average since process
start, so — like PostgreSQL's cache hit ratio, but worse, since there is no
`_since_reset`-style name to warn you — it drifts and cannot express current
latency. `[RESPONSE_TIME]` is the latency signal for MongoDB/FerretDB checks; do not
look for a latency field inside the body.

## FerretDB body schema

FerretDB accepts the same commands, but has no `mongod` internals to report — its
connections, locks, and replication state actually live in the backing PostgreSQL,
not in FerretDB itself. The body is thinner by design, not by bug:

```json
{
  "connect_ms": 20, "probe_ms": 2, "metrics_ms": 4,
  "probe": { "ok": 1 },
  "backend": "ferretdb", "version": "2.x",
  "uptime_seconds": 891234,
  "metrics_errors": [
    "serverStatus: connections and globalLock are not reported by this backend",
    "replSetGetStatus: not supported by this backend or not a replica set member"
  ]
}
```

> ⚠️ **This shape is a prediction, not a confirmed observation.** The integration
> test suite that exercises a live FerretDB instance did not run during
> development — the Docker daemon required to bring up the stack was unavailable —
> so `detectMongoBackend`'s heuristics (checking for a `ferretdb` key in `buildInfo`,
> and for `"ferretdb"` inside the version string) have never actually seen a real
> FerretDB response. The exact self-identification field FerretDB uses in
> `buildInfo` remains an **open question**. Before relying on `[BODY].backend` in a
> condition, confirm it against a live instance using
> [`.examples/docker-compose-database-monitoring/README.md`](../.examples/docker-compose-database-monitoring/README.md),
> which brings up a local PostgreSQL, MongoDB and FerretDB stack and runs the opt-in
> integration tests against them.

Because the real signals live elsewhere, cover a FerretDB tenant with more than the
`mongodb://` check:

- **The backing PostgreSQL**, monitored with the `postgres://` type described above
  — this is where connections, locks and replication actually are.
- **`GET /debug/readyz`** — validates both the MongoDB wire-protocol path and the
  PostgreSQL connection, and confirms the DocumentDB extension is installed.
- **`GET /debug/livez`** — whether FerretDB is accepting wire-protocol connections
  at all.
- **`/debug/metrics`** — Prometheus format, no exporter needed, for anything not
  covered above. FerretDB documents this metric set as unstable across minor
  releases, so treat it as supplementary rather than something to build hard
  thresholds on.

These debug endpoints bind to `127.0.0.1:8088` by default, so they are **not
reachable from Gatus** unless FerretDB is started with `--debug-addr` (or the
`FERRETDB_DEBUG_ADDR` environment variable) pointed at an address Gatus can reach.
Whether a given deployment currently does this is environment-specific and remains
an open question — verify it per-deployment before relying on these checks.

The `mongodb://` check still matters for a FerretDB tenant even though its body is
thin: `[CONNECTED]` and `[RESPONSE_TIME]` measure the actual path the application
takes — through the wire-protocol facade and into PostgreSQL — which is the most
faithful "this tenant's database is slow" signal available, independent of whatever
`/debug/*` reports.

## Required monitoring roles

**PostgreSQL** — a dedicated, read-only role per tenant database:

```sql
CREATE ROLE gatus_monitor WITH LOGIN PASSWORD '...';
GRANT pg_monitor TO gatus_monitor;
GRANT CONNECT ON DATABASE <tenant> TO gatus_monitor;
```

> ⚠️ **Without `pg_monitor`, four fields are omitted, and this is deliberate.**
> `connections`, `longest_running_query_seconds`,
> `longest_idle_in_transaction_seconds` and `blocked_sessions` all read from
> `pg_stat_activity`. Without `pg_monitor` membership, PostgreSQL restricts that
> view to the querying role's own sessions — so `gatus_monitor` would see only
> itself, and `connections.used` would report something like `1` on a database
> that actually has 80 active connections. That is not a missing measurement, it
> is a **wrong** one, and a wrong number in a threshold is worse than no number:
> it looks healthy while lying. The collector checks
> `pg_has_role(current_user, 'pg_monitor', 'member')` first and, when false, omits
> all four fields and records why in `metrics_errors` instead of reporting the
> misleading count. Grant `pg_monitor` if you want these fields at all.

**MongoDB / FerretDB** — the equivalent is the built-in `clusterMonitor` role,
granted on the `admin` database, e.g.:

```javascript
db.getSiblingDB("admin").createUser({
  user: "gatus_monitor",
  pwd: "...",
  roles: [{ role: "clusterMonitor", db: "admin" }]
})
```

Both roles are read-only. No probe query in this design needs table access beyond
what `clusterMonitor` and `pg_monitor` already grant for their respective
diagnostic commands.

## Startup validation

Two validations are fatal at boot, by design — both panic during
`ValidateAndSetDefaults` rather than surfacing as a runtime error on every check
interval:

- **`ErrEndpointWithInvalidDatabaseURL`** — a `postgres://`/`postgresql://` or
  `mongodb://`/`mongodb+srv://` URL that parses but has no host (e.g.
  `postgres:///tenant` or a bare `mongodb://`). Such a URL can never be connected
  to.
- **`ErrEndpointWithInvalidProbeCommand`** — a `mongodb://` endpoint whose `body:`
  does not parse as a valid BSON command document (e.g. malformed JSON, or an empty
  document).

Both are caught at config load, not at check time. The reasoning is the same in
both cases: a config typo in a URL or a probe document would otherwise render on
the dashboard as a real outage, forever, on every single interval — indistinguishable
from the database actually being down. Failing fast at startup turns that into a
one-time, loud, obvious error instead of a silent, permanent false alarm. This
matches how invalid config is treated everywhere else in this fork: fatal at
startup, by design.

## Worked per-tenant examples

A tenant on real MongoDB needs two endpoints:

```yaml
endpoints:
  - name: postgres
    group: navarromed
    visibility: public
    url: "postgres://gatus_monitor:${NAVARROMED_PG_MONITOR_PASSWORD}@navarromed-db.internal:5432/navarromed?sslmode=require"
    interval: 1m
    client:
      timeout: 5s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 200"
      - "[BODY].connections.used_pct < 80"
      - "[BODY].longest_running_query_seconds < 60"
      - "[BODY].blocked_sessions == 0"

  - name: mongodb
    group: navarromed
    visibility: public
    url: "mongodb://gatus_monitor:${NAVARROMED_MONGO_MONITOR_PASSWORD}@navarromed-mongo.internal:27017/navarromed?authSource=admin&replicaSet=rs0"
    interval: 1m
    client:
      timeout: 5s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 200"
      - "[BODY].is_writable_primary == true"
      - "[BODY].connections.used_pct < 80"
      - "[BODY].replication_lag_seconds < 10"
```

A tenant on FerretDB needs three or four, depending on whether FerretDB's backing
PostgreSQL is a separate instance from the tenant's application PostgreSQL (an
open question — check per-deployment):

```yaml
endpoints:
  - name: postgres
    group: plena
    visibility: public
    url: "postgres://gatus_monitor:${PLENA_PG_MONITOR_PASSWORD}@plena-db.internal:5432/plena?sslmode=require"
    interval: 1m
    client:
      timeout: 5s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 200"
      - "[BODY].connections.used_pct < 80"

  - name: mongodb-facade
    group: plena
    visibility: public
    url: "mongodb://gatus_monitor:${PLENA_MONGO_MONITOR_PASSWORD}@plena-ferretdb.internal:27017/plena"
    interval: 1m
    client:
      timeout: 5s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 200"

  - name: ferretdb-readyz
    group: plena
    visibility: public
    url: "http://plena-ferretdb.internal:8088/debug/readyz"   # requires --debug-addr / FERRETDB_DEBUG_ADDR to be reachable
    interval: 1m
    conditions:
      - "[STATUS] == 200"

  # Only needed when FerretDB's backing PostgreSQL is a distinct instance from
  # the tenant's application PostgreSQL above — confirm this per-deployment.
  - name: ferretdb-postgres
    group: plena
    url: "postgres://gatus_monitor:${PLENA_FERRETDB_PG_MONITOR_PASSWORD}@plena-ferretdb-db.internal:5432/postgres?sslmode=require"
    interval: 1m
    client:
      timeout: 5s
    conditions:
      - "[CONNECTED] == true"
      - "[RESPONSE_TIME] < 200"
```

Conditions reference only the fields that tenant's backend actually provides — the
FerretDB `mongodb-facade` endpoint above deliberately asserts nothing beyond
connectivity and latency, since its body carries little else. Since config is
already generated per-tenant, this costs nothing extra.

## A note on thresholds

Every threshold value shown anywhere in this document — `< 200`, `< 80`, `< 60`,
`< 10`, and so on — is a **placeholder**. None of them come from a production
baseline. Before using any of these conditions for real, observe the metric on the
actual database for a representative period and set the threshold from that, not
from this document.
