## Usage

This stack exists to run the opt-in integration tests in `client/database_integration_test.go`
against real servers: a PostgreSQL instance, a real MongoDB instance, and a FerretDB instance
(MongoDB wire-protocol compatible, backed by PostgreSQL). Those tests exercise the actual catalog
queries and command shapes that the unit tests cannot — they are skipped by default and only run
when you point them at a live server with the environment variables below.

> ⚠️ **As of this writing, nobody has actually run this stack.** The `postgres://` and `mongodb://`
> endpoint types, and everything [`docs/database-monitoring.md`](../../docs/database-monitoring.md)
> says about their body shapes, are reasoned from driver/protocol documentation and unit-tested
> against fabricated responses — not observed against a live PostgreSQL, MongoDB or FerretDB. The
> FerretDB body shape in particular is a documented *prediction*. Running the steps below for the
> first time is expected to surface surprises; please update the docs (and the code, if needed) with
> what you find rather than assuming the current text is already correct.

> ⚠️ **The `mongodb` service here is a standalone `mongo:7`, not a replica set.** `replSetGetStatus`
> always fails against it, so `[BODY].repl_set_state` and `[BODY].replication_lag_seconds` are always
> absent when you point a check at this container — `TestQueryMongoDB_Integration` and any manual
> check against `GATUS_TEST_MONGODB_URI` will never exercise the replication-lag code path. The
> flagship example in `docs/database-monitoring.md` and the README (a `mongodb://` endpoint with
> `?replicaSet=rs0` asserting `[BODY].replication_lag_seconds < 10`) **cannot be validated by this
> stack** — and per the presence-guard trap documented there, an unguarded `< 10` condition against a
> field this stack never populates would silently *pass* rather than error, which is exactly the kind
> of false confidence that trap produces. Guard that condition with
> `has([BODY].replication_lag_seconds) == true` if you test it here, and expect that guard to fail
> (correctly) against this standalone instance. This stack is intentionally not converted to a
> replica set — do the replica-set-specific verification against a real replica set instead.

### 1. Start the stack

From the repository root:

```console
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml up -d
```

Give the containers a few seconds to finish starting (FerretDB in particular needs its backing
PostgreSQL to be ready first):

```console
sleep 20
```

### 2. Grant the Postgres metrics role

The Postgres connection/lock/replication metrics require `pg_monitor` membership. Create a
dedicated monitoring role for the `tenant` database:

```console
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml exec -T postgres \
  psql -U postgres -d tenant -c "CREATE ROLE gatus_monitor WITH LOGIN PASSWORD 'monitor'; GRANT pg_monitor TO gatus_monitor; GRANT CONNECT ON DATABASE tenant TO gatus_monitor;"
```

Without this step, `TestQueryPostgres_Integration` still passes, but the `connections`,
`longest_running_query_seconds`, `longest_idle_in_transaction_seconds` and `blocked_sessions`
fields will be absent from the body (see `metrics_errors` in the test log).

### 3. Run the integration tests

Set the three environment variables the tests look for, then run them:

```console
GATUS_TEST_POSTGRES_DSN="postgres://gatus_monitor:monitor@127.0.0.1:5432/tenant?sslmode=disable" \
GATUS_TEST_MONGODB_URI="mongodb://root:root@127.0.0.1:27017/admin" \
GATUS_TEST_FERRETDB_URI="mongodb://postgres:postgres@127.0.0.1:27018/postgres" \
  go test ./client/ -run 'Integration' -v
```

FerretDB authenticates against its backing PostgreSQL, so the credentials are that database's —
`postgres:postgres` for this stack, as set by `POSTGRES_USER`/`POSTGRES_PASSWORD` on the
`ferretdb-postgres` service. (This file previously said `username:password`, which fails with a
SCRAM-SHA-256 authentication error.) Connecting without credentials also succeeds, but then
`serverStatus` comes back `(Unauthorized)` and the body loses `uptime_seconds`.

Note also that `ferretdb` starts before `ferretdb-postgres` is accepting connections and logs a
burst of `dial tcp ... connection refused` errors before settling. That is the startup race the
`sleep 20` above covers — it is not a failure. If the FerretDB test still reports a connection or
authentication error, check the container logs for the actual bootstrap credentials:

```console
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml logs ferretdb-postgres
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml logs ferretdb
```

and adjust `GATUS_TEST_FERRETDB_URI` accordingly.

Each test skips cleanly (no failure) if its corresponding environment variable is unset, which is
what keeps these tests out of the default `go test ./...` / CI run.

### 4. Tear down

```console
docker compose -f .examples/docker-compose-database-monitoring/compose.yaml down -v
```

The `-v` flag also removes the named volumes, so the next `up -d` starts from a clean database.
