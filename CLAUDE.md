# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This is **nf-gatus**, Nexfar's fork of [TwiN/gatus](https://github.com/TwiN/gatus) (the Go module path is still `github.com/TwiN/gatus/v5`). Gatus is a developer-oriented health dashboard that monitors services via HTTP, ICMP, TCP, DNS, SSH, etc., evaluates responses against a list of conditions, and fires alerts through ~40 providers.

## Read AGENTS.md first

`AGENTS.md` is the canonical, detailed contributor guide and is kept up to date. It covers commands, the exact step-by-step recipes for **adding an alerting provider** (6 locations) and **adding a config section**, frontend rules, and API route ordering. Do not duplicate that content here — follow it. This file adds the big-picture architecture that AGENTS.md does not spell out.

## Commands

| Task | Command |
|:-----|:--------|
| Build binary | `make install` |
| Run dev server (uses `./config.yaml`, CORS for frontend dev) | `make run` |
| Run all tests with coverage | `make test` |
| Run a single package's tests | `go test ./watchdog/... -cover` |
| Run a single test | `go test ./config/endpoint/ -run TestCondition_evaluate -v` |
| Build the Vue frontend (**required after any `web/app/` change**) | `make frontend-build` |
| Frontend dev server | `make frontend-dev` |

After changing Go dependencies: `go mod tidy && go mod vendor` (the `vendor/` directory is committed).

## Architecture: the monitoring data flow

The whole app is wired up in `main.go`'s `start()`. Understanding these four moving parts and how they connect is the key to the codebase:

1. **Config (`config/`)** — `config.LoadConfiguration` reads `GATUS_CONFIG_PATH` (a file, or a directory of YAML files that are deep-merged) into one `config.Config` struct. Each config section lives in its own sub-package (`config/endpoint`, `config/suite`, `config/maintenance`, `config/ui`, ...) and exposes a `ValidateAndSetDefaults()`. `parseAndValidateConfigBytes` calls these in a **specific, order-dependent sequence** — alerting must be validated before endpoints. Invalid config is **fatal (panics at startup)**, by design.

2. **Watchdog (`watchdog/`)** — `watchdog.Monitor` is the monitoring loop. It launches one goroutine per endpoint/suite, gated by a semaphore sized to `config.concurrency`. Each cycle: the goroutine uses **`client/`** to execute the check, builds an `endpoint.Result`, evaluates the endpoint's **conditions** (`config/endpoint/condition.go` + `placeholder.go`, which resolve `[STATUS]`, `[BODY]`, JSONPath, `len/has/pat/any` functions), persists the result via the store, and then `watchdog/alerting.go` decides whether to trigger/resolve alerts based on consecutive failure/success counts.

3. **Storage (`storage/store/`)** — `store.Get()` returns a singleton store implementing a common interface, backed by `memory/` (default, non-persistent) or `sql/` (SQLite and PostgreSQL share this package). The store holds health-check results, uptime, events, and **persisted triggered-alert state** that is reloaded on startup (see `initializeStorage` in `main.go`) so alerts survive restarts and config reloads.

4. **API + UI (`controller/` + `api/` + `web/`)** — `controller.Handle` starts the Fiber HTTP server defined in `api/api.go`, which serves the JSON API (dashboard data, badges at `api/badge.go`, charts, raw data, the external-endpoint push endpoint) and the embedded SPA (`api/spa.go`). The Vue 3 frontend in `web/app/` is built to `web/static/` and embedded into the binary via `//go:embed` — **never edit `web/static/` directly**.

**Hot reload:** `listenToConfigurationFileChanges` polls the config file every 30s; on change it runs the full `stop → save → reload → initializeStorage → start` cycle in-process. Any new startup logic must be safe to run repeatedly, not just once.

**Alerting (`alerting/`)** — `alerting/config.go` holds one field per provider (YAML tag must exactly match the `alert.Type` string for reflection-based lookup); `alerting/alert/type.go` enumerates types; each provider is a sub-package under `alerting/provider/`. Use `alerting/provider/slack/` as the reference when adding one.

## Conventions specific to this repo

- **Concept = sub-package.** New config sections and alert providers are new packages, each self-validating, registered back in the central `config.Config` / `alerting.Config` structs. Follow the existing wiring exactly (AGENTS.md lists every touch-point) — partial registration compiles but fails silently at runtime.
- **Tests live beside code** as `*_test.go`; config and API packages are expected to have them. API tests spin up real test servers.
- **This is a fork that tracks upstream.** Keep changes minimal and upstream-compatible where possible; the import path remains `github.com/TwiN/gatus/v5`.
