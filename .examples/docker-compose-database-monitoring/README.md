## Usage

This stack exists to run the opt-in integration tests in `client/database_integration_test.go`
against real servers: a PostgreSQL instance, a real MongoDB instance, and a FerretDB instance
(MongoDB wire-protocol compatible, backed by PostgreSQL). Those tests exercise the actual catalog
queries and command shapes that the unit tests cannot — they are skipped by default and only run
when you point them at a live server with the environment variables below.

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
GATUS_TEST_FERRETDB_URI="mongodb://username:password@127.0.0.1:27018/postgres" \
  go test ./client/ -run 'Integration' -v
```

The FerretDB credentials depend on how the `postgres-documentdb` image initializes its default
user. If the FerretDB test reports a connection refused or an authentication error, check the
container logs for the actual bootstrap credentials:

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
